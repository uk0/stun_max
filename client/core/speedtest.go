package core

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// SpeedTest v3: ALL messages go P2P UDP first, relay fallback only.
// UDP prefixes: "SM:" signal, "ST:" data, "SF:" finish

type activeSpeedTest struct {
	TestID          string
	PeerID          string
	Phase           string // "upload", "download", "done"
	TotalSize       int64
	Sent            int64
	Received        int64
	StartTime       time.Time
	UploadMbps      float64
	Transport       string // "auto", "p2p", "relay"
	ActualTransport string // resolved: "p2p" or "relay"
	mu              sync.Mutex
}

var stDebug int32

func (c *Client) StartSpeedTest(peerID string, sizeMB int, mode string) (string, error) {
	fullID, err := c.resolvePeerID(peerID)
	if err != nil {
		return "", err
	}
	if sizeMB <= 0 {
		sizeMB = 10
	}
	if mode == "" {
		mode = "auto"
	}

	// Resolve actual transport
	actual := c.resolveSTTransport(fullID, mode)

	testID := generateTunnelID()
	test := &activeSpeedTest{
		TestID:          testID,
		PeerID:          fullID,
		Phase:           "upload",
		TotalSize:       int64(sizeMB) * 1024 * 1024,
		Transport:       mode,
		ActualTransport: actual,
	}

	c.speedTestsMu.Lock()
	c.speedTests[testID] = test
	c.speedTestsMu.Unlock()

	atomic.StoreInt32(&stDebug, 0)

	// Send begin via resolved transport
	payload, _ := json.Marshal(map[string]interface{}{
		"test_id": testID, "size": test.TotalSize, "direction": "upload",
		"transport": actual,
	})
	c.stSend(fullID, actual, append([]byte("SM:st_begin:"), payload...), "st_begin", json.RawMessage(payload))

	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
		"Speed test: %d MB with %s [%s]", sizeMB, shortID(fullID), actual)})

	go func() {
		time.Sleep(3 * time.Minute)
		c.speedTestsMu.Lock()
		if t, ok := c.speedTests[testID]; ok && t.Phase != "done" {
			delete(c.speedTests, testID)
			c.speedTestsMu.Unlock()
			c.emit(EventSpeedTestResult, SpeedTestResult{
				TestID: testID, PeerID: fullID, Error: "timeout", Done: true,
				Transport: actual,
			})
		} else {
			c.speedTestsMu.Unlock()
		}
	}()

	return testID, nil
}

// stSignal sends a signal message via the test's resolved transport.
func (c *Client) stSignal(peerID, msgType string, data interface{}) {
	payload, _ := json.Marshal(data)
	udpMsg := append([]byte("SM:"+msgType+":"), payload...)
	c.sendViaP2P(peerID, udpMsg, msgType, json.RawMessage(payload))
}

// stSignalWith sends a signal message via the specified transport.
func (c *Client) stSignalWith(peerID, transport, msgType string, data interface{}) {
	payload, _ := json.Marshal(data)
	udpMsg := append([]byte("SM:"+msgType+":"), payload...)
	c.stSend(peerID, transport, udpMsg, msgType, json.RawMessage(payload))
}

// resolveSTTransport determines the actual transport for a speed test.
func (c *Client) resolveSTTransport(peerID, mode string) string {
	if mode == "relay" {
		return "relay"
	}
	if mode == "p2p" {
		return "p2p"
	}
	// auto: check if P2P is available
	c.peerConnsMu.RLock()
	pc := c.peerConns[peerID]
	hasP2P := pc != nil && pc.Mode == "direct" && pc.UDPAddr != nil
	c.peerConnsMu.RUnlock()
	if hasP2P {
		return "p2p"
	}
	return "relay"
}

// stSend sends data via the specified transport only.
func (c *Client) stSend(peerID, transport string, udpPayload []byte, relayType string, relayPayload interface{}) {
	if transport == "p2p" {
		c.peerConnsMu.RLock()
		pc := c.peerConns[peerID]
		var addr *net.UDPAddr
		if pc != nil && pc.Mode == "direct" && pc.UDPAddr != nil {
			addr = pc.UDPAddr
		}
		c.peerConnsMu.RUnlock()
		if addr != nil {
			c.connMu.Lock()
			udp := c.udpConn
			c.connMu.Unlock()
			if udp != nil {
				udp.WriteToUDP(udpPayload, addr)
				return
			}
		}
		// P2P not available, fall back to relay for signal messages
		c.sendRelay(peerID, relayType, relayPayload)
		return
	}
	if transport == "relay" {
		c.sendRelay(peerID, relayType, relayPayload)
		return
	}
	// auto fallback
	c.sendViaP2P(peerID, udpPayload, relayType, relayPayload)
}

func (c *Client) handleSTBegin(msg Message) {
	var info struct {
		TestID    string `json:"test_id"`
		Size      int64  `json:"size"`
		Direction string `json:"direction"`
		Transport string `json:"transport"`
	}
	json.Unmarshal(msg.Payload, &info)
	if info.TestID == "" {
		return
	}

	// Responder resolves transport: use initiator's preference or auto-detect
	transport := info.Transport
	if transport == "" {
		transport = c.resolveSTTransport(msg.From, "auto")
	}

	// If this is a download-phase begin and we already have the test (initiator side),
	// transition the existing test to receive download data instead of creating a new one.
	c.speedTestsMu.RLock()
	existing, exists := c.speedTests[info.TestID]
	c.speedTestsMu.RUnlock()

	if exists && info.Direction == "download" {
		existing.mu.Lock()
		existing.Phase = "download"
		existing.Received = 0
		existing.StartTime = time.Time{}
		existing.mu.Unlock()

		c.emit(EventSpeedTestProgress, SpeedTestProgressEvent{
			TestID: info.TestID, PeerID: existing.PeerID,
			Phase: "download", Progress: 0, Transport: existing.ActualTransport,
		})
		c.stSignalWith(msg.From, existing.ActualTransport, "st_ready", map[string]string{"test_id": info.TestID})
		return
	}

	test := &activeSpeedTest{
		TestID: info.TestID, PeerID: msg.From,
		Phase: "receiving", TotalSize: info.Size,
		Transport: transport, ActualTransport: transport,
	}
	c.speedTestsMu.Lock()
	c.speedTests[info.TestID] = test
	c.speedTestsMu.Unlock()

	c.stSignalWith(msg.From, transport, "st_ready", map[string]string{"test_id": info.TestID})
}

func (c *Client) handleSTReady(msg Message) {
	var info map[string]string
	json.Unmarshal(msg.Payload, &info)
	testID := info["test_id"]

	c.speedTestsMu.RLock()
	test, ok := c.speedTests[testID]
	c.speedTestsMu.RUnlock()
	if !ok {
		return
	}
	go c.stSendData(test)
}

func (c *Client) stSendData(test *activeSpeedTest) {
	test.mu.Lock()
	test.StartTime = time.Now()
	transport := test.ActualTransport
	test.mu.Unlock()

	// P2P uses small chunks to fit UDP MTU; relay uses large chunks over WebSocket
	chunkSize := 32 * 1024
	if transport == "p2p" {
		chunkSize = 1024 // ~1.4KB after base64, fits single UDP packet
	}

	chunk := make([]byte, chunkSize)
	rand.Read(chunk)

	var sent int64
	seq := 0
	for sent < test.TotalSize {
		remaining := test.TotalSize - sent
		thisChunk := chunk
		if remaining < int64(len(chunk)) {
			thisChunk = chunk[:remaining]
		}
		encoded := base64.StdEncoding.EncodeToString(thisChunk)
		data := SpeedTestData{TestID: test.TestID, Data: encoded, Seq: seq}

		payload, _ := json.Marshal(data)
		compressed := Compress(payload)
		udpMsg := make([]byte, 3+len(compressed))
		copy(udpMsg[:3], []byte("ST:"))
		copy(udpMsg[3:], compressed)
		c.stSend(test.PeerID, transport, udpMsg, "speed_test_data", data)

		sent += int64(len(thisChunk))
		seq++

		c.emit(EventSpeedTestProgress, SpeedTestProgressEvent{
			TestID: test.TestID, PeerID: test.PeerID,
			Phase: test.Phase, Progress: float64(sent) / float64(test.TotalSize),
			Transport: transport,
		})
	}

	duration := time.Since(test.StartTime)

	finishData := map[string]interface{}{
		"test_id": test.TestID, "bytes": sent, "duration_ms": duration.Milliseconds(),
	}
	c.stSignalWith(test.PeerID, transport, "st_finish", finishData)
	time.Sleep(30 * time.Millisecond)
	c.stSignalWith(test.PeerID, transport, "st_finish", finishData) // redundant

	test.mu.Lock()
	test.Sent = sent
	if test.Phase == "upload" {
		test.Phase = "waiting_download"
		test.Received = 0
		test.StartTime = time.Now()
	}
	test.mu.Unlock()
}

func (c *Client) handleSpeedTestData(msg Message) {
	var data SpeedTestData
	json.Unmarshal(msg.Payload, &data)
	c.processSpeedTestData(data)
}

func (c *Client) processSpeedTestData(data SpeedTestData) {
	c.speedTestsMu.RLock()
	test, ok := c.speedTests[data.TestID]
	c.speedTestsMu.RUnlock()
	if !ok {
		return
	}

	decoded, _ := base64.StdEncoding.DecodeString(data.Data)
	test.mu.Lock()
	if test.StartTime.IsZero() {
		test.StartTime = time.Now()
	}
	test.Received += int64(len(decoded))
	progress := float64(test.Received) / float64(test.TotalSize)
	if progress > 1 {
		progress = 1
	}
	phase := test.Phase
	transport := test.ActualTransport
	test.mu.Unlock()

	displayPhase := phase
	if phase == "receiving" || phase == "waiting_download" || phase == "download" {
		displayPhase = "download"
	}
	c.emit(EventSpeedTestProgress, SpeedTestProgressEvent{
		TestID: test.TestID, PeerID: test.PeerID,
		Phase: displayPhase, Progress: progress, Transport: transport,
	})
}

func (c *Client) handleSTFinish(msg Message) {
	var info struct {
		TestID     string `json:"test_id"`
		Bytes      int64  `json:"bytes"`
		DurationMs int64  `json:"duration_ms"`
	}
	json.Unmarshal(msg.Payload, &info)
	if info.TestID == "" {
		return
	}
	c.processSTFinish(info.TestID, info.Bytes, info.DurationMs, msg.From)
}

func (c *Client) processSTFinish(testID string, senderBytes, senderDurationMs int64, from string) {
	c.speedTestsMu.RLock()
	test, ok := c.speedTests[testID]
	c.speedTestsMu.RUnlock()
	if !ok {
		return
	}

	test.mu.Lock()
	defer test.mu.Unlock()

	if test.Phase == "receiving" {
		// B received A's upload. Report speed, then start download test.
		elapsed := time.Since(test.StartTime).Milliseconds()
		if elapsed <= 0 {
			elapsed = 1
		}
		recvMbps := float64(test.Received) * 8 / float64(elapsed) / 1000
		transport := test.ActualTransport

		c.stSignalWith(from, transport, "st_result", map[string]interface{}{
			"test_id": testID, "mbps": recvMbps, "phase": "upload",
		})

		// Start download: B sends to A
		test.Phase = "download"
		test.Sent = 0
		test.Received = 0
		test.StartTime = time.Time{}
		test.mu.Unlock()

		// Tell A to prepare
		c.stSignalWith(from, transport, "st_begin", map[string]interface{}{
			"test_id": testID, "size": test.TotalSize, "direction": "download",
			"transport": transport,
		})
		test.mu.Lock()
		return
	}

	if test.Phase == "waiting_download" || test.Phase == "download" {
		// A received B's download. Final result.
		elapsed := time.Since(test.StartTime).Milliseconds()
		if elapsed <= 0 {
			elapsed = 1
		}
		dlMbps := float64(test.Received) * 8 / float64(elapsed) / 1000
		test.Phase = "done"

		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
			"Speed test download: %.1f Mbps [%s]", dlMbps, test.ActualTransport)})
		c.emit(EventSpeedTestResult, SpeedTestResult{
			TestID: testID, PeerID: test.PeerID,
			UploadMbps: test.UploadMbps, DownloadMbps: dlMbps, Done: true,
			Transport: test.ActualTransport,
		})
		c.speedTestsMu.Lock()
		delete(c.speedTests, testID)
		c.speedTestsMu.Unlock()
	}
}

func (c *Client) handleSTResult(msg Message) {
	var info struct {
		TestID string  `json:"test_id"`
		Mbps   float64 `json:"mbps"`
		Phase  string  `json:"phase"`
	}
	json.Unmarshal(msg.Payload, &info)

	c.speedTestsMu.RLock()
	test, ok := c.speedTests[info.TestID]
	c.speedTestsMu.RUnlock()
	if !ok {
		return
	}

	test.mu.Lock()
	if info.Phase == "upload" {
		test.UploadMbps = info.Mbps
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Speed test upload: %.1f Mbps", info.Mbps)})
	}
	test.mu.Unlock()
}
