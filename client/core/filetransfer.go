package core

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
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
	NackCh     chan FileNack // sender receives NACK for bad/missing chunks
	// Receiver: track received chunks for gap detection
	RecvChunks map[int]int64 // seq → offset (tracks which chunks arrived)
	mu         sync.Mutex
}

// PLACEHOLDER_REMAINING_CONTENT

const (
	fileChunkSizeP2P   = 1024      // 1KB — fits single UDP packet after base64+JSON+compress
	fileChunkSizeRelay = 32 * 1024 // 32KB — relay goes over WebSocket, no MTU limit
)

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
		NackCh:     make(chan FileNack, 256),
	}

	c.fileTransfersMu.Lock()
	c.fileTransfers[transferID] = ft
	c.fileTransfersMu.Unlock()

	// Send offer (prefer P2P for signaling too)
	c.sendViaP2P(fullID,
		append([]byte("SM:file_offer:"), mustJSON(FileOffer{
			TransferID: transferID, FileName: ft.FileName, FileSize: ft.FileSize, FileHash: fileHash,
		})...),
		"file_offer", FileOffer{TransferID: transferID, FileName: ft.FileName, FileSize: ft.FileSize, FileHash: fileHash})

	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("File offer sent to %s: %s (%s)", peerName, ft.FileName, fmtFileSize(ft.FileSize))})
	return transferID, nil
}

// PLACEHOLDER_ACCEPT_AND_BELOW

// AcceptFile accepts a pending incoming file offer and starts receiving.
// If a partial file already exists at savePath, resumes from where it left off.
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

	// Check for existing partial file (resume support)
	var resumeOffset int64
	if info, err := os.Stat(savePath); err == nil && info.Size() > 0 && info.Size() < ft.FileSize {
		resumeOffset = info.Size()
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
			"Resuming file %s from %s/%s", ft.FileName, fmtFileSize(resumeOffset), fmtFileSize(ft.FileSize))})
	}

	var f *os.File
	var err error
	if resumeOffset > 0 {
		f, err = os.OpenFile(savePath, os.O_WRONLY|os.O_APPEND, 0644)
	} else {
		f, err = os.Create(savePath)
	}
	if err != nil {
		ft.mu.Unlock()
		return fmt.Errorf("open file: %w", err)
	}

	ft.File = f
	ft.FilePath = savePath
	ft.Status = "active"
	ft.BytesDone = resumeOffset
	ft.StartTime = time.Now()
	ft.mu.Unlock()

	c.sendViaP2P(ft.PeerID,
		append([]byte("SM:file_accept:"), mustJSON(FileAccept{TransferID: transferID, Offset: resumeOffset})...),
		"file_accept", FileAccept{TransferID: transferID, Offset: resumeOffset})
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Accepted file: %s → %s (offset %s)", ft.FileName, savePath, fmtFileSize(resumeOffset))})
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
	// Security check: is incoming file receive allowed?
	if !c.AllowFileRecv() {
		c.emit(EventLog, LogEvent{Level: "warn", Message: "Rejected file offer from " + shortID(msg.From) + " (file receive disabled)"})
		c.sendRelay(msg.From, "file_reject", FileReject{Reason: "file receive disabled"})
		return
	}

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
	if accept.Offset > 0 && ft.File != nil {
		ft.File.Seek(accept.Offset, io.SeekStart)
		ft.BytesDone = accept.Offset
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
			"Resuming send from offset %s", fmtFileSize(accept.Offset))})
	}
	ft.mu.Unlock()

	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("File accepted by %s, sending %s...", ft.PeerName, ft.FileName)})

	c.wg.Add(1)
	go c.sendFileViaNetstack(ft)
}

// sendFileViaNetstack streams the file over a gVisor TCP connection (reliable).
func (c *Client) sendFileViaNetstack(ft *activeFileTransfer) {
	defer c.wg.Done()

	// Get or create forward netstack for this peer
	fn, err := c.getOrCreateFwdNetstack(ft.PeerID, true)
	if err != nil {
		c.emit(EventLog, LogEvent{Level: "warn", Message: "File netstack failed: " + err.Error() + ", falling back to chunks"})
		c.sendFileChunks(ft) // fallback to old chunk method
		return
	}

	// Allocate a virtual port for this file transfer
	vport := nextVirtualPort()

	// Tell receiver to listen on this virtual port
	c.sendViaP2P(ft.PeerID,
		append([]byte("SM:file_stream:"), mustJSON(map[string]interface{}{
			"transfer_id": ft.TransferID, "port": vport,
		})...),
		"file_stream", json.RawMessage(mustJSON(map[string]interface{}{
			"transfer_id": ft.TransferID, "port": vport,
		})))

	// Wait for receiver to register the port
	time.Sleep(500 * time.Millisecond)

	// Dial through gVisor TCP
	conn, err := fn.DialTCP(vport)
	if err != nil {
		c.emit(EventLog, LogEvent{Level: "warn", Message: "File TCP dial failed: " + err.Error() + ", falling back to chunks"})
		c.sendFileChunks(ft) // fallback
		return
	}
	defer conn.Close()

	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("File streaming via gVisor TCP: %s", ft.FileName)})

	// Stream file data over TCP
	ft.mu.Lock()
	f := ft.File
	offset := ft.BytesDone
	ft.mu.Unlock()

	if f == nil {
		return
	}

	buf := make([]byte, 64*1024)
	for {
		select {
		case <-ft.Done:
			return
		case <-c.done:
			return
		default:
		}

		n, readErr := f.Read(buf)
		if n > 0 {
			conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			_, writeErr := conn.Write(buf[:n])
			if writeErr != nil {
				ft.mu.Lock()
				ft.Status = "error"
				ft.mu.Unlock()
				c.emit(EventFileError, FileErrorEvent{TransferID: ft.TransferID, Error: writeErr.Error()})
				return
			}

			ft.mu.Lock()
			ft.BytesDone += int64(n)
			bytesDone := ft.BytesDone
			fileSize := ft.FileSize
			startTime := ft.StartTime
			ft.mu.Unlock()

			offset += int64(n)

			// Progress every 64KB
			if offset%(64*1024) < int64(n) {
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
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			ft.mu.Lock()
			ft.Status = "error"
			ft.mu.Unlock()
			c.emit(EventFileError, FileErrorEvent{TransferID: ft.TransferID, Error: readErr.Error()})
			return
		}
	}

	// Done — close file, send file_done with hash
	ft.mu.Lock()
	totalBytes := ft.BytesDone
	ft.Status = "complete"
	if ft.File != nil {
		ft.File.Close()
		ft.File = nil
	}
	ft.mu.Unlock()

	doneMsg := FileDone{TransferID: ft.TransferID, TotalBytes: totalBytes, FileHash: ft.FileHash}
	c.sendViaP2P(ft.PeerID,
		append([]byte("SM:file_done:"), mustJSON(doneMsg)...),
		"file_done", doneMsg)

	c.emit(EventFileComplete, FileCompleteEvent{
		TransferID: ft.TransferID, FileName: ft.FileName, Direction: "send",
	})
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("File sent: %s (%s) hash=%s",
		ft.FileName, fmtFileSize(totalBytes), ft.FileHash[:16])})
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

	raw, err := base64.StdEncoding.DecodeString(data.Data)
	if err != nil {
		return
	}
	chunk, err := Decompress(raw)
	if err != nil {
		return
	}

	// Verify chunk CRC32
	if data.ChunkHash != "" {
		actual := fmt.Sprintf("%08x", crc32.ChecksumIEEE(chunk))
		if actual != data.ChunkHash {
			c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf(
				"Chunk %d CRC mismatch (expected %s got %s), requesting retransmit", data.Seq, data.ChunkHash, actual)})
			c.sendViaP2P(msg.From,
				append([]byte("SM:file_nack:"), mustJSON(FileNack{TransferID: data.TransferID, Seq: data.Seq, Offset: data.Offset})...),
				"file_nack", FileNack{TransferID: data.TransferID, Seq: data.Seq, Offset: data.Offset})
			return
		}
	}

	c.writeFileChunk(ft, data, chunk)
}

// writeFileChunk writes a verified chunk to disk and emits progress.
func (c *Client) writeFileChunk(ft *activeFileTransfer, data FileData, chunk []byte) {
	ft.mu.Lock()
	if ft.File == nil || ft.Status != "active" {
		ft.mu.Unlock()
		return
	}
	_, writeErr := ft.File.Write(chunk)
	ft.BytesDone += int64(len(chunk))
	// Track received seq for gap detection
	if ft.RecvChunks == nil {
		ft.RecvChunks = make(map[int]int64)
	}
	ft.RecvChunks[data.Seq] = data.Offset
	bytesDone := ft.BytesDone
	fileSize := ft.FileSize
	startTime := ft.StartTime
	ft.mu.Unlock()

	if writeErr != nil {
		c.emit(EventFileError, FileErrorEvent{TransferID: data.TransferID, Error: writeErr.Error()})
		return
	}

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

	// Calculate expected total chunks
	ft.mu.Lock()
	expectedHash := done.FileHash
	if expectedHash == "" {
		expectedHash = ft.FileHash
	}
	filePath := ft.FilePath
	fileName := ft.FileName
	recvCount := len(ft.RecvChunks)
	ft.mu.Unlock()

	// Check if we received enough data by comparing BytesDone vs TotalBytes
	ft.mu.Lock()
	bytesDone := ft.BytesDone
	ft.mu.Unlock()

	if bytesDone < done.TotalBytes {
		// Missing chunks — calculate how many based on bytes gap
		missing := done.TotalBytes - bytesDone
		c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf(
			"File %s: missing %s (%d chunks received, expected %s). Requesting retransmit...",
			fileName, fmtFileSize(missing), recvCount, fmtFileSize(done.TotalBytes))})

		// Find missing seq numbers by checking gaps
		// We know the total bytes and what we received. Request retransmit from bytesDone offset.
		nack := FileNack{
			TransferID: done.TransferID,
			Seq:        recvCount, // next expected seq
			Offset:     bytesDone, // resume from here
		}
		c.sendViaP2P(msg.From,
			append([]byte("SM:file_nack:"), mustJSON(nack)...),
			"file_nack", nack)
		// Don't close file — wait for retransmitted chunks
		return
	}

	// All bytes received — close file and verify hash
	ft.mu.Lock()
	if ft.File != nil {
		ft.File.Close()
		ft.File = nil
	}
	ft.mu.Unlock()

	verified := false
	if expectedHash != "" && filePath != "" {
		if actualHash, err := hashFile(filePath); err == nil {
			if actualHash == expectedHash {
				verified = true
				c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
					"File hash verified: %s (SHA-256: %s)", fileName, actualHash[:16])})
			} else {
				c.emit(EventLog, LogEvent{Level: "error", Message: fmt.Sprintf(
					"File hash MISMATCH: %s expected=%s actual=%s", fileName, expectedHash[:16], actualHash[:16])})
				ft.mu.Lock()
				ft.Status = "error"
				ft.mu.Unlock()
				c.emit(EventFileError, FileErrorEvent{TransferID: done.TransferID, Error: "hash mismatch — file corrupted"})
				return
			}
		}
	}

	ft.mu.Lock()
	ft.Status = "complete"
	ft.mu.Unlock()

	verifyStr := ""
	if verified {
		verifyStr = " (verified)"
	}
	c.emit(EventFileComplete, FileCompleteEvent{
		TransferID: done.TransferID,
		FileName:   fileName,
		Direction:  "receive",
	})
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("File received: %s (%s)%s", fileName, fmtFileSize(done.TotalBytes), verifyStr)})
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

// sendFileChunks reads the file in chunks, compresses, and sends via P2P or relay.
// Includes rate limiting for P2P UDP to prevent buffer overflow and packet loss.
func (c *Client) sendFileChunks(ft *activeFileTransfer) {
	defer c.wg.Done()

	// Check if P2P is available for this peer
	useP2P := c.PeerMode(ft.PeerID) == "direct"
	if useP2P {
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("File transfer using P2P: %s", ft.FileName)})
	} else {
		c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf("File transfer using RELAY (no P2P): %s", ft.FileName)})
	}

	// Choose chunk size based on transport
	chunkSize := fileChunkSizeRelay
	if useP2P {
		chunkSize = fileChunkSizeP2P
	}

	buf := make([]byte, chunkSize)
	seq := 0

	// Rate limiter: limit P2P UDP send rate to prevent packet loss
	// Allow burst of 32 packets, then pace to ~500 chunks/sec for P2P
	var sendPacer <-chan time.Time
	if useP2P {
		ticker := time.NewTicker(2 * time.Millisecond) // ~500 chunks/sec × 1KB = ~500KB/s baseline
		defer ticker.Stop()
		sendPacer = ticker.C
	}

	consecutiveFailures := 0

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
			rawChunk := buf[:n]
			chunkCRC := fmt.Sprintf("%08x", crc32.ChecksumIEEE(rawChunk))
			compressed := Compress(rawChunk)
			encoded := base64.StdEncoding.EncodeToString(compressed)
			fileData := FileData{
				TransferID: ft.TransferID,
				Data:       encoded,
				Seq:        seq,
				Offset:     ft.BytesDone,
				ChunkHash:  chunkCRC,
			}

			// Try P2P first, fallback to relay
			var sendErr error
			if useP2P {
				payload, _ := json.Marshal(fileData)
				udpMsg := append([]byte("SF:"), Compress(payload)...)
				if !c.sendFileP2P(ft.PeerID, udpMsg) {
					consecutiveFailures++
					if consecutiveFailures > 10 {
						// P2P is broken, switch to relay permanently
						useP2P = false
						chunkSize = fileChunkSizeRelay
						buf = make([]byte, chunkSize)
						sendPacer = nil
						c.emit(EventLog, LogEvent{Level: "warn", Message: "P2P send failed repeatedly, switching to relay"})
					}
					sendErr = c.sendRelay(ft.PeerID, "file_data", fileData)
				} else {
					consecutiveFailures = 0
				}
			} else {
				sendErr = c.sendRelay(ft.PeerID, "file_data", fileData)
			}

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

			// P2P rate limiting: wait for pacer tick to prevent UDP buffer overflow
			if sendPacer != nil {
				select {
				case <-sendPacer:
				case <-ft.Done:
					return
				case <-c.done:
					return
				}
			}

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

			// Re-check P2P status periodically
			if seq%100 == 0 {
				newP2P := c.PeerMode(ft.PeerID) == "direct"
				if newP2P != useP2P {
					useP2P = newP2P
					if useP2P {
						chunkSize = fileChunkSizeP2P
						ticker := time.NewTicker(2 * time.Millisecond)
						defer ticker.Stop()
						sendPacer = ticker.C
					} else {
						chunkSize = fileChunkSizeRelay
						sendPacer = nil
					}
					buf = make([]byte, chunkSize)
				}
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

	// Process any pending NACKs — retransmit bad chunks before sending done
	c.retransmitNacks(ft, useP2P)

	ft.mu.Lock()
	totalBytes := ft.BytesDone
	ft.Status = "complete"
	if ft.File != nil {
		ft.File.Close()
		ft.File = nil
	}
	ft.mu.Unlock()

	doneMsg := FileDone{TransferID: ft.TransferID, TotalBytes: totalBytes, FileHash: ft.FileHash}
	c.sendViaP2P(ft.PeerID,
		append([]byte("SM:file_done:"), mustJSON(doneMsg)...),
		"file_done", doneMsg)

	c.emit(EventFileComplete, FileCompleteEvent{
		TransferID: ft.TransferID,
		FileName:   ft.FileName,
		Direction:  "send",
	})
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("File sent: %s (%s) hash=%s", ft.FileName, fmtFileSize(totalBytes), ft.FileHash[:16])})
}

// sendFileP2P sends file data via P2P UDP. Returns true if sent successfully.
func (c *Client) sendFileP2P(peerID string, udpMsg []byte) bool {
	addr, ok := c.directEndpoint(peerID)
	if !ok {
		return false
	}
	return c.udpSend(udpMsg, addr) == nil
}

// processFileDataP2P handles file data received via P2P UDP (SF: prefix).
func (c *Client) processFileDataP2P(data FileData) {
	c.fileTransfersMu.RLock()
	ft, ok := c.fileTransfers[data.TransferID]
	c.fileTransfersMu.RUnlock()
	if !ok || ft.Direction != "receive" {
		return
	}

	raw, err := base64.StdEncoding.DecodeString(data.Data)
	if err != nil {
		return
	}
	chunk, err := Decompress(raw)
	if err != nil {
		return
	}

	// Verify chunk CRC32
	if data.ChunkHash != "" {
		actual := fmt.Sprintf("%08x", crc32.ChecksumIEEE(chunk))
		if actual != data.ChunkHash {
			c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf(
				"P2P chunk %d CRC mismatch, requesting retransmit", data.Seq)})
			// Find peer ID from transfer
			c.sendViaP2P(ft.PeerID,
				append([]byte("SM:file_nack:"), mustJSON(FileNack{TransferID: data.TransferID, Seq: data.Seq, Offset: data.Offset})...),
				"file_nack", FileNack{TransferID: data.TransferID, Seq: data.Seq, Offset: data.Offset})
			return
		}
	}

	c.writeFileChunk(ft, data, chunk)
}

// fmtFileSize formats bytes into a human-readable string.
// handleFileStream is called when sender tells receiver which virtual port to expect data on.
func (c *Client) handleFileStream(msg Message) {
	var info struct {
		TransferID string `json:"transfer_id"`
		Port       uint16 `json:"port"`
	}
	if err := json.Unmarshal(msg.Payload, &info); err != nil {
		return
	}

	c.fileTransfersMu.RLock()
	ft, ok := c.fileTransfers[info.TransferID]
	c.fileTransfersMu.RUnlock()
	if !ok || ft.Direction != "receive" || ft.Status != "active" {
		return
	}

	// Get or create forward netstack (receiver side)
	fn, err := c.getOrCreateFwdNetstack(msg.From, false)
	if err != nil {
		c.emit(EventLog, LogEvent{Level: "error", Message: "File stream netstack failed: " + err.Error()})
		return
	}

	// Register this port to handle the file stream
	fn.RegisterFileTransfer(info.Port, ft, c)
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
		"File stream: listening on virtual port %d for %s", info.Port, ft.FileName)})
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

// hashFile computes SHA-256 of a file and returns hex string.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// retransmitNacks drains the NACK channel and retransmits requested chunks.
// After retransmitting, re-sends file_done so receiver can re-verify.
func (c *Client) retransmitNacks(ft *activeFileTransfer, useP2P bool) {
	maxRounds := 5 // prevent infinite retransmit loops
	for round := 0; round < maxRounds; round++ {
		timer := time.NewTimer(3 * time.Second)
		retransmitted := 0

		for {
			select {
			case nack := <-ft.NackCh:
				c.retransmitChunk(ft, nack, useP2P)
				retransmitted++
				timer.Reset(3 * time.Second)
			case <-timer.C:
				goto doneWaiting
			case <-ft.Done:
				timer.Stop()
				return
			}
		}
	doneWaiting:
		timer.Stop()

		if retransmitted == 0 {
			return // no NACKs received, we're done
		}

		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
			"Retransmitted %d chunks (round %d) for %s", retransmitted, round+1, ft.FileName)})

		// Re-send file_done so receiver can re-check completeness
		doneMsg := FileDone{TransferID: ft.TransferID, TotalBytes: ft.FileSize, FileHash: ft.FileHash}
		c.sendViaP2P(ft.PeerID,
			append([]byte("SM:file_done:"), mustJSON(doneMsg)...),
			"file_done", doneMsg)
	}
}

// retransmitChunk re-reads and re-sends a specific chunk.
func (c *Client) retransmitChunk(ft *activeFileTransfer, nack FileNack, useP2P bool) {
	ft.mu.Lock()
	f := ft.File
	ft.mu.Unlock()
	if f == nil {
		// File already closed, reopen
		var err error
		f, err = os.Open(ft.FilePath)
		if err != nil {
			return
		}
		defer f.Close()
	}

	chunkSize := fileChunkSizeRelay
	if useP2P {
		chunkSize = fileChunkSizeP2P
	}

	buf := make([]byte, chunkSize)
	f.Seek(nack.Offset, io.SeekStart)
	n, err := f.Read(buf)
	if err != nil || n == 0 {
		return
	}

	rawChunk := buf[:n]
	chunkCRC := fmt.Sprintf("%08x", crc32.ChecksumIEEE(rawChunk))
	compressed := Compress(rawChunk)
	encoded := base64.StdEncoding.EncodeToString(compressed)
	fileData := FileData{
		TransferID: ft.TransferID,
		Data:       encoded,
		Seq:        nack.Seq,
		Offset:     nack.Offset,
		ChunkHash:  chunkCRC,
	}

	if useP2P {
		payload, _ := json.Marshal(fileData)
		udpMsg := append([]byte("SF:"), Compress(payload)...)
		if !c.sendFileP2P(ft.PeerID, udpMsg) {
			c.sendRelay(ft.PeerID, "file_data", fileData)
		}
	} else {
		c.sendRelay(ft.PeerID, "file_data", fileData)
	}
}

// handleFileNack processes a NACK from the receiver requesting chunk retransmission.
func (c *Client) handleFileNack(msg Message) {
	var nack FileNack
	if err := json.Unmarshal(msg.Payload, &nack); err != nil {
		return
	}

	c.fileTransfersMu.RLock()
	ft, ok := c.fileTransfers[nack.TransferID]
	c.fileTransfersMu.RUnlock()
	if !ok || ft.Direction != "send" {
		return
	}

	select {
	case ft.NackCh <- nack:
	default:
		// Channel full, retransmit directly
		useP2P := c.PeerMode(ft.PeerID) == "direct"
		c.retransmitChunk(ft, nack, useP2P)
	}
}

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
