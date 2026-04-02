package core

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// activeFileTransfer tracks a single in-progress file transfer.
type activeFileTransfer struct {
	TransferID string
	PeerID     string
	PeerName   string
	FileName   string
	FileSize   int64
	FileHash   string // expected SHA-256 hex
	FilePath   string // local path (send: source, receive: destination)
	Direction  string // "send" or "receive"
	Status     string // "pending", "active", "complete", "error"
	File       *os.File
	BytesDone  int64
	StartTime  time.Time
	Done       chan struct{}
	mu         sync.Mutex
}

// PLACEHOLDER_REMAINING_CONTENT

const fileChunkSize = 32 * 1024 // 32KB per chunk

// SendFile opens a file, computes its SHA-256 hash, and sends a file_offer to the peer.
// Returns the transfer ID on success.
func (c *Client) SendFile(peerID, filePath string) (string, error) {
	fullID, err := c.resolvePeerID(peerID)
	if err != nil {
		return "", err
	}

	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open file: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return "", fmt.Errorf("stat file: %w", err)
	}
	if info.IsDir() {
		f.Close()
		return "", fmt.Errorf("cannot send a directory")
	}

	// Compute SHA-256
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		f.Close()
		return "", fmt.Errorf("hash file: %w", err)
	}
	fileHash := hex.EncodeToString(hasher.Sum(nil))

	// Seek back to start for later reading
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return "", fmt.Errorf("seek file: %w", err)
	}

	peerName := shortID(fullID)
	c.peersMu.RLock()
	for _, p := range c.peers {
		if p.ID == fullID && p.Name != "" {
			peerName = p.Name
			break
		}
	}
	c.peersMu.RUnlock()

	transferID := generateTunnelID()
	ft := &activeFileTransfer{
		TransferID: transferID,
		PeerID:     fullID,
		PeerName:   peerName,
		FileName:   filepath.Base(filePath),
		FileSize:   info.Size(),
		FileHash:   fileHash,
		FilePath:   filePath,
		Direction:  "send",
		Status:     "pending",
		File:       f,
		Done:       make(chan struct{}),
	}

	c.fileTransfersMu.Lock()
	c.fileTransfers[transferID] = ft
	c.fileTransfersMu.Unlock()

	// Send offer
	c.sendRelay(fullID, "file_offer", FileOffer{
		TransferID: transferID,
		FileName:   ft.FileName,
		FileSize:   ft.FileSize,
		FileHash:   fileHash,
	})

	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("File offer sent to %s: %s (%s)", peerName, ft.FileName, fmtFileSize(ft.FileSize))})
	return transferID, nil
}

// PLACEHOLDER_ACCEPT_AND_BELOW

// AcceptFile accepts a pending incoming file offer and starts receiving.
func (c *Client) AcceptFile(transferID, savePath string) error {
	c.fileTransfersMu.RLock()
	ft, ok := c.fileTransfers[transferID]
	c.fileTransfersMu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown transfer %s", transferID)
	}

	ft.mu.Lock()
	if ft.Direction != "receive" || ft.Status != "pending" {
		ft.mu.Unlock()
		return fmt.Errorf("transfer not pending receive")
	}

	// Ensure download directory exists
	dir := filepath.Dir(savePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		ft.mu.Unlock()
		return fmt.Errorf("create dir: %w", err)
	}

	f, err := os.Create(savePath)
	if err != nil {
		ft.mu.Unlock()
		return fmt.Errorf("create file: %w", err)
	}

	ft.File = f
	ft.FilePath = savePath
	ft.Status = "active"
	ft.StartTime = time.Now()
	ft.mu.Unlock()

	c.sendRelay(ft.PeerID, "file_accept", FileAccept{TransferID: transferID})
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Accepted file: %s → %s", ft.FileName, savePath)})
	return nil
}

// RejectFile rejects a pending incoming file offer.
func (c *Client) RejectFile(transferID string) error {
	c.fileTransfersMu.Lock()
	ft, ok := c.fileTransfers[transferID]
	if !ok {
		c.fileTransfersMu.Unlock()
		return fmt.Errorf("unknown transfer %s", transferID)
	}
	delete(c.fileTransfers, transferID)
	c.fileTransfersMu.Unlock()

	ft.mu.Lock()
	select {
	case <-ft.Done:
	default:
		close(ft.Done)
	}
	ft.mu.Unlock()

	c.sendRelay(ft.PeerID, "file_reject", FileReject{TransferID: transferID, Reason: "rejected by user"})
	return nil
}

// CancelFileTransfer cancels an active or pending transfer.
func (c *Client) CancelFileTransfer(transferID string) error {
	c.fileTransfersMu.Lock()
	ft, ok := c.fileTransfers[transferID]
	if !ok {
		c.fileTransfersMu.Unlock()
		return fmt.Errorf("unknown transfer %s", transferID)
	}
	delete(c.fileTransfers, transferID)
	c.fileTransfersMu.Unlock()

	ft.mu.Lock()
	ft.Status = "error"
	if ft.File != nil {
		ft.File.Close()
	}
	select {
	case <-ft.Done:
	default:
		close(ft.Done)
	}
	ft.mu.Unlock()

	c.sendRelay(ft.PeerID, "file_cancel", FileCancel{TransferID: transferID, Reason: "cancelled"})
	c.emit(EventFileError, FileErrorEvent{TransferID: transferID, Error: "cancelled"})
	return nil
}

// FileTransfers returns a snapshot of all active file transfers.
func (c *Client) FileTransfers() []FileTransferInfo {
	c.fileTransfersMu.RLock()
	defer c.fileTransfersMu.RUnlock()

	var out []FileTransferInfo
	for _, ft := range c.fileTransfers {
		ft.mu.Lock()
		progress := float64(0)
		if ft.FileSize > 0 {
			progress = float64(ft.BytesDone) / float64(ft.FileSize)
		}
		speed := float64(0)
		if ft.Status == "active" && !ft.StartTime.IsZero() {
			elapsed := time.Since(ft.StartTime).Seconds()
			if elapsed > 0 {
				speed = float64(ft.BytesDone) / elapsed
			}
		}
		info := FileTransferInfo{
			TransferID: ft.TransferID,
			PeerID:     ft.PeerID,
			PeerName:   ft.PeerName,
			FileName:   ft.FileName,
			FileSize:   ft.FileSize,
			BytesDone:  ft.BytesDone,
			Direction:  ft.Direction,
			Progress:   progress,
			Speed:      speed,
			Status:     ft.Status,
		}
		ft.mu.Unlock()
		out = append(out, info)
	}
	return out
}

// PLACEHOLDER_HANDLERS

// --- Internal message handlers ---

func (c *Client) handleFileOffer(msg Message) {
	var offer FileOffer
	if err := json.Unmarshal(msg.Payload, &offer); err != nil {
		return
	}

	peerName := shortID(msg.From)
	c.peersMu.RLock()
	for _, p := range c.peers {
		if p.ID == msg.From && p.Name != "" {
			peerName = p.Name
			break
		}
	}
	c.peersMu.RUnlock()

	ft := &activeFileTransfer{
		TransferID: offer.TransferID,
		PeerID:     msg.From,
		PeerName:   peerName,
		FileName:   offer.FileName,
		FileSize:   offer.FileSize,
		FileHash:   offer.FileHash,
		Direction:  "receive",
		Status:     "pending",
		Done:       make(chan struct{}),
	}

	c.fileTransfersMu.Lock()
	c.fileTransfers[offer.TransferID] = ft
	c.fileTransfersMu.Unlock()

	c.emit(EventFileOffer, FileOfferEvent{
		TransferID: offer.TransferID,
		PeerID:     msg.From,
		PeerName:   peerName,
		FileName:   offer.FileName,
		FileSize:   offer.FileSize,
	})
}

func (c *Client) handleFileAccept(msg Message) {
	var accept FileAccept
	if err := json.Unmarshal(msg.Payload, &accept); err != nil {
		return
	}

	c.fileTransfersMu.RLock()
	ft, ok := c.fileTransfers[accept.TransferID]
	c.fileTransfersMu.RUnlock()
	if !ok || ft.Direction != "send" {
		return
	}

	ft.mu.Lock()
	ft.Status = "active"
	ft.StartTime = time.Now()
	ft.mu.Unlock()

	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("File accepted by %s, sending %s...", ft.PeerName, ft.FileName)})

	c.wg.Add(1)
	go c.sendFileChunks(ft)
}

func (c *Client) handleFileData(msg Message) {
	var data FileData
	if err := json.Unmarshal(msg.Payload, &data); err != nil {
		return
	}

	c.fileTransfersMu.RLock()
	ft, ok := c.fileTransfers[data.TransferID]
	c.fileTransfersMu.RUnlock()
	if !ok || ft.Direction != "receive" {
		return
	}

	// Decode and decompress
	raw, err := base64.StdEncoding.DecodeString(data.Data)
	if err != nil {
		return
	}
	chunk, err := Decompress(raw)
	if err != nil {
		return
	}

	ft.mu.Lock()
	if ft.File == nil || ft.Status != "active" {
		ft.mu.Unlock()
		return
	}
	_, writeErr := ft.File.Write(chunk)
	ft.BytesDone += int64(len(chunk))
	bytesDone := ft.BytesDone
	fileSize := ft.FileSize
	startTime := ft.StartTime
	ft.mu.Unlock()

	if writeErr != nil {
		c.emit(EventFileError, FileErrorEvent{TransferID: data.TransferID, Error: writeErr.Error()})
		return
	}

	// Emit progress every 10 chunks
	if data.Seq%10 == 0 {
		progress := float64(0)
		if fileSize > 0 {
			progress = float64(bytesDone) / float64(fileSize)
		}
		speed := float64(0)
		elapsed := time.Since(startTime).Seconds()
		if elapsed > 0 {
			speed = float64(bytesDone) / elapsed
		}
		c.emit(EventFileProgress, FileProgressEvent{
			TransferID: data.TransferID,
			Progress:   progress,
			Speed:      speed,
			BytesDone:  bytesDone,
		})
	}
}

// PLACEHOLDER_DONE_AND_SEND

func (c *Client) handleFileDone(msg Message) {
	var done FileDone
	if err := json.Unmarshal(msg.Payload, &done); err != nil {
		return
	}

	c.fileTransfersMu.RLock()
	ft, ok := c.fileTransfers[done.TransferID]
	c.fileTransfersMu.RUnlock()
	if !ok || ft.Direction != "receive" {
		return
	}

	ft.mu.Lock()
	if ft.File != nil {
		ft.File.Close()
		ft.File = nil
	}
	ft.Status = "complete"
	ft.BytesDone = done.TotalBytes
	fileName := ft.FileName
	ft.mu.Unlock()

	c.emit(EventFileComplete, FileCompleteEvent{
		TransferID: done.TransferID,
		FileName:   fileName,
		Direction:  "receive",
	})
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("File received: %s (%s)", fileName, fmtFileSize(done.TotalBytes))})
}

func (c *Client) handleFileReject(msg Message) {
	var reject FileReject
	if err := json.Unmarshal(msg.Payload, &reject); err != nil {
		return
	}

	c.fileTransfersMu.Lock()
	ft, ok := c.fileTransfers[reject.TransferID]
	if ok {
		delete(c.fileTransfers, reject.TransferID)
	}
	c.fileTransfersMu.Unlock()
	if !ok {
		return
	}

	ft.mu.Lock()
	ft.Status = "error"
	if ft.File != nil {
		ft.File.Close()
		ft.File = nil
	}
	select {
	case <-ft.Done:
	default:
		close(ft.Done)
	}
	ft.mu.Unlock()

	reason := reject.Reason
	if reason == "" {
		reason = "rejected"
	}
	c.emit(EventFileError, FileErrorEvent{TransferID: reject.TransferID, Error: reason})
	c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf("File rejected by %s: %s", ft.PeerName, reason)})
}

func (c *Client) handleFileCancel(msg Message) {
	var cancel FileCancel
	if err := json.Unmarshal(msg.Payload, &cancel); err != nil {
		return
	}

	c.fileTransfersMu.Lock()
	ft, ok := c.fileTransfers[cancel.TransferID]
	if ok {
		delete(c.fileTransfers, cancel.TransferID)
	}
	c.fileTransfersMu.Unlock()
	if !ok {
		return
	}

	ft.mu.Lock()
	ft.Status = "error"
	if ft.File != nil {
		ft.File.Close()
		ft.File = nil
	}
	select {
	case <-ft.Done:
	default:
		close(ft.Done)
	}
	ft.mu.Unlock()

	c.emit(EventFileError, FileErrorEvent{TransferID: cancel.TransferID, Error: "cancelled by peer"})
	c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf("File transfer cancelled by %s", ft.PeerName)})
}

// sendFileChunks reads the file in 32KB chunks, compresses, and sends.
func (c *Client) sendFileChunks(ft *activeFileTransfer) {
	defer c.wg.Done()

	buf := make([]byte, fileChunkSize)
	seq := 0

	for {
		select {
		case <-ft.Done:
			return
		case <-c.done:
			return
		default:
		}

		n, err := ft.File.Read(buf)
		if n > 0 {
			compressed := Compress(buf[:n])
			encoded := base64.StdEncoding.EncodeToString(compressed)
			sendErr := c.sendRelay(ft.PeerID, "file_data", FileData{
				TransferID: ft.TransferID,
				Data:       encoded,
				Seq:        seq,
				Offset:     ft.BytesDone,
			})
			if sendErr != nil {
				ft.mu.Lock()
				ft.Status = "error"
				ft.mu.Unlock()
				c.emit(EventFileError, FileErrorEvent{TransferID: ft.TransferID, Error: sendErr.Error()})
				return
			}

			ft.mu.Lock()
			ft.BytesDone += int64(n)
			bytesDone := ft.BytesDone
			fileSize := ft.FileSize
			startTime := ft.StartTime
			ft.mu.Unlock()
			seq++

			// Emit progress every 10 chunks
			if seq%10 == 0 {
				progress := float64(0)
				if fileSize > 0 {
					progress = float64(bytesDone) / float64(fileSize)
				}
				speed := float64(0)
				elapsed := time.Since(startTime).Seconds()
				if elapsed > 0 {
					speed = float64(bytesDone) / elapsed
				}
				c.emit(EventFileProgress, FileProgressEvent{
					TransferID: ft.TransferID,
					Progress:   progress,
					Speed:      speed,
					BytesDone:  bytesDone,
				})
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			ft.mu.Lock()
			ft.Status = "error"
			ft.mu.Unlock()
			c.emit(EventFileError, FileErrorEvent{TransferID: ft.TransferID, Error: err.Error()})
			return
		}
	}

	// Send done
	ft.mu.Lock()
	totalBytes := ft.BytesDone
	ft.Status = "complete"
	if ft.File != nil {
		ft.File.Close()
		ft.File = nil
	}
	ft.mu.Unlock()

	c.sendRelay(ft.PeerID, "file_done", FileDone{
		TransferID: ft.TransferID,
		TotalBytes: totalBytes,
	})

	c.emit(EventFileComplete, FileCompleteEvent{
		TransferID: ft.TransferID,
		FileName:   ft.FileName,
		Direction:  "send",
	})
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("File sent: %s (%s)", ft.FileName, fmtFileSize(totalBytes))})
}

// fmtFileSize formats bytes into a human-readable string.
func fmtFileSize(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	if b < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	if b < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	}
	return fmt.Sprintf("%.2f GB", float64(b)/(1024*1024*1024))
}
