package core

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type activeSpeedTest struct {
	TestID    string
	PeerID    string
	State     string // "requesting", "uploading", "downloading", "receiving", "sending_back", "waiting_download", "done"
	TotalSize int64
	Sent      int64
	Received  int64
	StartTime time.Time
	mu        sync.Mutex
}

// StartSpeedTest initiates a speed test with the given peer.
func (c *Client) StartSpeedTest(peerID string) (string, error) {
	fullID, err := c.resolvePeerID(peerID)
	if err != nil {
		return "", err
	}

	testID := generateTunnelID()
	test := &activeSpeedTest{
		TestID:    testID,
		PeerID:    fullID,
		State:     "requesting",
		TotalSize: 100 * 1024 * 1024, // 100 MB
	}

	c.speedTestsMu.Lock()
	c.speedTests[testID] = test
	c.speedTestsMu.Unlock()

	c.sendRelay(fullID, "speed_test_request", SpeedTestRequest{
		TestID:    testID,
		Direction: "upload",
		Size:      test.TotalSize,
	})

	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Speed test started with %s (test %s)", shortID(fullID), testID[:8])})

	return testID, nil
}

func (c *Client) handleSpeedTestRequest(msg Message) {
	var req SpeedTestRequest
	if err := json.Unmarshal(msg.Payload, &req); err != nil {
		return
	}

	// Send ready
	c.sendRelay(msg.From, "speed_test_ready", map[string]string{"test_id": req.TestID})

	test := &activeSpeedTest{
		TestID:    req.TestID,
		PeerID:    msg.From,
		State:     "receiving",
		TotalSize: req.Size,
		StartTime: time.Now(),
	}
	c.speedTestsMu.Lock()
	c.speedTests[req.TestID] = test
	c.speedTestsMu.Unlock()
}

func (c *Client) handleSpeedTestReady(msg Message) {
	var info map[string]string
	if err := json.Unmarshal(msg.Payload, &info); err != nil {
		return
	}
	testID := info["test_id"]

	c.speedTestsMu.RLock()
	test, ok := c.speedTests[testID]
	c.speedTestsMu.RUnlock()
	if !ok {
		return
	}

	go c.runSpeedTestUpload(test)
}

func (c *Client) runSpeedTestUpload(test *activeSpeedTest) {
	test.mu.Lock()
	test.State = "uploading"
	test.StartTime = time.Now()
	test.mu.Unlock()

	chunk := make([]byte, 32*1024) // 32KB chunks
	rand.Read(chunk)
	encoded := base64.StdEncoding.EncodeToString(chunk)

	var sent int64
	seq := 0
	for sent < test.TotalSize {
		remaining := test.TotalSize - sent
		if remaining < int64(len(chunk)) {
			chunk = chunk[:remaining]
			encoded = base64.StdEncoding.EncodeToString(chunk)
		}

		c.sendRelay(test.PeerID, "speed_test_data", SpeedTestData{
			TestID: test.TestID,
			Data:   encoded,
			Seq:    seq,
		})
		sent += int64(len(chunk))
		seq++

		c.emit(EventSpeedTestProgress, SpeedTestProgressEvent{
			TestID:   test.TestID,
			PeerID:   test.PeerID,
			Phase:    "upload",
			Progress: float64(sent) / float64(test.TotalSize),
		})
	}

	duration := time.Since(test.StartTime)
	c.sendRelay(test.PeerID, "speed_test_done", SpeedTestDone{
		TestID:     test.TestID,
		Bytes:      sent,
		DurationMs: duration.Milliseconds(),
	})

	test.mu.Lock()
	test.Sent = sent
	test.State = "waiting_download"
	test.mu.Unlock()
}

func (c *Client) handleSpeedTestData(msg Message) {
	var data SpeedTestData
	if err := json.Unmarshal(msg.Payload, &data); err != nil {
		return
	}

	c.speedTestsMu.RLock()
	test, ok := c.speedTests[data.TestID]
	c.speedTestsMu.RUnlock()
	if !ok {
		return
	}

	decoded, _ := base64.StdEncoding.DecodeString(data.Data)
	test.mu.Lock()
	test.Received += int64(len(decoded))
	progress := float64(test.Received) / float64(test.TotalSize)
	test.mu.Unlock()

	c.emit(EventSpeedTestProgress, SpeedTestProgressEvent{
		TestID:   test.TestID,
		PeerID:   test.PeerID,
		Phase:    "download",
		Progress: progress,
	})
}

func (c *Client) handleSpeedTestDone(msg Message) {
	var done SpeedTestDone
	if err := json.Unmarshal(msg.Payload, &done); err != nil {
		return
	}

	c.speedTestsMu.RLock()
	test, ok := c.speedTests[done.TestID]
	c.speedTestsMu.RUnlock()
	if !ok {
		return
	}

	test.mu.Lock()
	if test.State == "receiving" {
		// We received upload from peer, now send back
		test.State = "sending_back"
		test.mu.Unlock()
		go c.runSpeedTestUpload(test)
		return
	}

	// Both phases done - calculate results
	uploadMbps := float64(done.Bytes) * 8 / float64(done.DurationMs) / 1000
	elapsed := time.Since(test.StartTime).Milliseconds()
	downloadMbps := 0.0
	if elapsed > 0 {
		downloadMbps = float64(test.Received) * 8 / float64(elapsed) / 1000
	}
	test.State = "done"
	test.mu.Unlock()

	result := SpeedTestResult{
		TestID:       test.TestID,
		PeerID:       test.PeerID,
		UploadMbps:   uploadMbps,
		DownloadMbps: downloadMbps,
		Done:         true,
	}
	c.emit(EventSpeedTestResult, result)

	// Cleanup
	c.speedTestsMu.Lock()
	delete(c.speedTests, test.TestID)
	c.speedTestsMu.Unlock()
}
