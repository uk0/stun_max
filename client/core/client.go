package core

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Client holds all state for the networking core.
type Client struct {
	// Exported config (read-only after creation)
	Config ClientConfig
	MyID   string

	// Event channel for GUI consumption
	events chan Event

	// Internal state
	conn         *websocket.Conn
	connMu       sync.Mutex
	room         string
	passwordHash string
	name         string

	peers   []PeerInfo
	peersMu sync.RWMutex

	forwards   map[int]*Forward
	forwardsMu sync.RWMutex

	tunnels   map[string]*TunnelConn
	tunnelsMu sync.RWMutex

	// P2P / STUN fields
	peerConns   map[string]*PeerConn
	peerConnsMu sync.RWMutex
	udpConn     *net.UDPConn
	publicAddr  string
	verbose     bool

	// Access control
	allowForward bool // default true
	localOnly    bool // default true
	acMu         sync.RWMutex

	// Speed test tracking
	speedTests   map[string]*activeSpeedTest
	speedTestsMu sync.RWMutex

	done chan struct{}
	wg   sync.WaitGroup
}

// NewClient creates a new Client from the given config.
func NewClient(cfg ClientConfig) *Client {
	hash := ""
	if cfg.Password != "" {
		h := sha256.Sum256([]byte(cfg.Password))
		hash = hex.EncodeToString(h[:])
	}
	return &Client{
		Config:       cfg,
		events:       make(chan Event, 256),
		room:         cfg.Room,
		passwordHash: hash,
		name:         cfg.Name,
		verbose:      cfg.Verbose,
		forwards:     make(map[int]*Forward),
		tunnels:      make(map[string]*TunnelConn),
		peerConns:    make(map[string]*PeerConn),
		speedTests:   make(map[string]*activeSpeedTest),
		allowForward: true,
		localOnly:    true,
		done:         make(chan struct{}),
	}
}

// Events returns the read-only event channel.
func (c *Client) Events() <-chan Event {
	return c.events
}

// emit sends an event to the GUI in a non-blocking fashion.
func (c *Client) emit(t EventType, data interface{}) {
	select {
	case c.events <- Event{Type: t, Data: data}:
	default:
	}
}

// Connect dials the WebSocket server and reads the welcome message.
func (c *Client) Connect() error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.Dial(c.Config.ServerURL, nil)
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}
	c.conn = conn

	// Read welcome message to get our ID
	_, data, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return fmt.Errorf("reading welcome: %w", err)
	}

	var welcome Message
	if err := json.Unmarshal(data, &welcome); err != nil {
		conn.Close()
		return fmt.Errorf("parsing welcome: %w", err)
	}
	if welcome.Type != "welcome" {
		conn.Close()
		return fmt.Errorf("expected welcome, got %s", welcome.Type)
	}

	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(welcome.Payload, &payload); err != nil {
		conn.Close()
		return fmt.Errorf("parsing welcome payload: %w", err)
	}
	c.MyID = payload.ID

	c.emit(EventConnected, LogEvent{Level: "info", Message: fmt.Sprintf("Connected, ID: %s", c.MyID)})

	// Start WebSocket read loop
	c.wg.Add(1)
	go c.readLoop()

	return nil
}

// JoinRoom sends the join message to the server.
func (c *Client) JoinRoom() error {
	joinPayload, _ := json.Marshal(map[string]string{
		"room":          c.room,
		"password_hash": c.passwordHash,
		"name":          c.name,
	})
	err := c.sendMsg(Message{
		Type:    "join",
		Room:    c.room,
		Payload: json.RawMessage(joinPayload),
	})
	if err == nil {
		c.emit(EventJoinedRoom, LogEvent{Level: "info", Message: fmt.Sprintf("Joined room %q as %q", c.room, c.name)})
	}
	return err
}

// Disconnect cleanly tears down all resources.
func (c *Client) Disconnect() {
	select {
	case <-c.done:
		return
	default:
		close(c.done)
	}

	// Stop all forwards
	c.forwardsMu.Lock()
	ports := make([]int, 0, len(c.forwards))
	for port := range c.forwards {
		ports = append(ports, port)
	}
	c.forwardsMu.Unlock()

	for _, port := range ports {
		c.StopForward(port)
	}

	// Close remaining tunnels
	c.tunnelsMu.Lock()
	for id, tc := range c.tunnels {
		tc.Conn.Close()
		select {
		case <-tc.Done:
		default:
			close(tc.Done)
		}
		delete(c.tunnels, id)
	}
	c.tunnelsMu.Unlock()

	// Close UDP socket
	if c.udpConn != nil {
		c.udpConn.Close()
	}

	// Close WebSocket
	c.connMu.Lock()
	if c.conn != nil {
		c.conn.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		)
		c.conn.Close()
	}
	c.connMu.Unlock()

	c.emit(EventDisconnected, LogEvent{Level: "info", Message: "Disconnected"})
}

// Done returns a channel that is closed when the client shuts down.
func (c *Client) Done() <-chan struct{} {
	return c.done
}

// WaitDone waits for all goroutines with a timeout.
func (c *Client) WaitDone(timeout time.Duration) {
	waitDone := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(timeout):
	}
}

// Peers returns a snapshot of the current peer list.
func (c *Client) Peers() []PeerInfo {
	c.peersMu.RLock()
	defer c.peersMu.RUnlock()
	out := make([]PeerInfo, len(c.peers))
	copy(out, c.peers)
	return out
}

// Forwards returns a snapshot of all active forwards.
func (c *Client) Forwards() []ForwardInfo {
	c.forwardsMu.RLock()
	defer c.forwardsMu.RUnlock()

	var out []ForwardInfo
	for _, fwd := range c.forwards {
		fwd.Mu.Lock()
		count := fwd.ConnCount
		fr := fwd.ForceRelay
		fwd.Mu.Unlock()

		mode := c.getForwardMode(fwd.PeerID)
		if fr {
			mode = "RELAY"
		}

		bytesUp := atomic.LoadInt64(&fwd.BytesUp)
		bytesDown := atomic.LoadInt64(&fwd.BytesDown)
		lastUp := atomic.LoadInt64(&fwd.LastUp)
		lastDown := atomic.LoadInt64(&fwd.LastDown)

		// Rate = delta since last snapshot (called ~every 1s by GUI)
		rateUp := float64(bytesUp - lastUp)
		rateDown := float64(bytesDown - lastDown)
		atomic.StoreInt64(&fwd.LastUp, bytesUp)
		atomic.StoreInt64(&fwd.LastDown, bytesDown)

		out = append(out, ForwardInfo{
			LocalPort:  fwd.LocalPort,
			RemoteHost: fwd.RemoteHost,
			RemotePort: fwd.RemotePort,
			PeerID:     fwd.PeerID,
			PeerName:   fwd.PeerName,
			Mode:       mode,
			ForceRelay: fr,
			ConnCount:  count,
			BytesUp:    bytesUp,
			BytesDown:  bytesDown,
			RateUp:     rateUp,
			RateDown:   rateDown,
		})
	}
	return out
}

// StunStatus returns a snapshot of STUN/P2P state.
func (c *Client) StunStatus() StunInfo {
	info := StunInfo{
		PublicAddr: c.publicAddr,
		Enabled:    c.publicAddr != "",
		PeerConns:  make(map[string]string),
	}
	c.peerConnsMu.RLock()
	for id, pc := range c.peerConns {
		info.PeerConns[id] = pc.Mode
	}
	c.peerConnsMu.RUnlock()
	return info
}

// SetAllowForward controls whether incoming tunnel requests are accepted.
func (c *Client) SetAllowForward(allow bool) {
	c.acMu.Lock()
	c.allowForward = allow
	c.acMu.Unlock()
}

// SetLocalOnly controls whether tunnels to non-localhost targets are allowed.
func (c *Client) SetLocalOnly(local bool) {
	c.acMu.Lock()
	c.localOnly = local
	c.acMu.Unlock()
}

// AllowForward returns the current allow-forward setting.
func (c *Client) AllowForward() bool {
	c.acMu.RLock()
	defer c.acMu.RUnlock()
	return c.allowForward
}

// LocalOnly returns the current local-only setting.
func (c *Client) LocalOnly() bool {
	c.acMu.RLock()
	defer c.acMu.RUnlock()
	return c.localOnly
}

// PeerMode returns the connection mode for a given peer.
func (c *Client) PeerMode(peerID string) string {
	c.peerConnsMu.RLock()
	defer c.peerConnsMu.RUnlock()
	if pc, ok := c.peerConns[peerID]; ok {
		return pc.Mode
	}
	return "-"
}

// SetForwardMode: forceRelay=true forces relay even when P2P is available.
// Only useful as a fallback when P2P data path has issues.
func (c *Client) SetForwardMode(localPort int, forceRelay bool) {
	c.forwardsMu.RLock()
	fwd, ok := c.forwards[localPort]
	c.forwardsMu.RUnlock()
	if !ok {
		return
	}
	fwd.Mu.Lock()
	fwd.ForceRelay = forceRelay
	fwd.Mu.Unlock()

	mode := "P2P (auto)"
	if forceRelay {
		mode = "RELAY (forced)"
	}
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Forward :%d → %s", localPort, mode)})
}

// --- Internal helpers ---

func (c *Client) sendMsg(msg Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *Client) sendRelay(to string, innerType string, innerPayload interface{}) error {
	payloadBytes, err := json.Marshal(innerPayload)
	if err != nil {
		return err
	}
	envelope, err := json.Marshal(RelayEnvelope{
		Type:    innerType,
		Payload: json.RawMessage(payloadBytes),
	})
	if err != nil {
		return err
	}
	return c.sendMsg(Message{
		Type:    "relay_data",
		To:      to,
		Payload: json.RawMessage(envelope),
	})
}

// sendTunnelData sends tunnel data via WebSocket relay.
func (c *Client) sendTunnelData(peerID, tunnelID string, data []byte) error {
	encoded := base64.StdEncoding.EncodeToString(data)
	return c.sendRelay(peerID, "tunnel_data", TunnelData{
		TunnelID: tunnelID,
		Data:     encoded,
	})
}

func (c *Client) readLoop() {
	defer c.wg.Done()
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			select {
			case <-c.done:
				return
			default:
			}

			c.emit(EventDisconnected, LogEvent{Level: "warn", Message: "Connection lost, reconnecting..."})

			// Auto-reconnect loop
			if c.reconnect() {
				continue // reconnected, resume reading
			}

			// Reconnect failed permanently
			c.emit(EventError, LogEvent{Level: "error", Message: "Reconnect failed, disconnected"})
			close(c.done)
			return
		}
		for _, line := range splitMessages(data) {
			if len(line) == 0 {
				continue
			}
			var msg Message
			if err := json.Unmarshal(line, &msg); err != nil {
				continue
			}
			c.handleMessage(msg)
		}
	}
}

// reconnect attempts to re-establish the WebSocket connection and rejoin the room.
// Fixed 3-second interval, retries indefinitely until success or c.done is closed.
func (c *Client) reconnect() bool {
	for attempt := 1; ; attempt++ {
		select {
		case <-c.done:
			return false
		default:
		}

		c.emit(EventReconnecting, LogEvent{Level: "info", Message: fmt.Sprintf("Reconnect attempt %d...", attempt)})

		time.Sleep(3 * time.Second)

		select {
		case <-c.done:
			return false
		default:
		}

		// Use saved config for connection parameters
		serverURL := c.Config.ServerURL
		room := c.room
		passHash := c.passwordHash
		name := c.name

		// Try to connect
		dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
		conn, _, err := dialer.Dial(serverURL, nil)
		if err != nil {
			c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf("Reconnect failed: %v", err)})
			continue
		}

		// Read welcome
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, welcomeData, err := conn.ReadMessage()
		if err != nil {
			conn.Close()
			continue
		}
		conn.SetReadDeadline(time.Time{})

		var welcome Message
		if err := json.Unmarshal(welcomeData, &welcome); err != nil || welcome.Type != "welcome" {
			conn.Close()
			continue
		}
		var payload struct{ ID string `json:"id"` }
		json.Unmarshal(welcome.Payload, &payload)

		// Update connection
		c.connMu.Lock()
		oldConn := c.conn
		c.conn = conn
		c.MyID = payload.ID
		c.connMu.Unlock()
		if oldConn != nil {
			oldConn.Close()
		}

		// Rejoin room with correct credentials
		joinPayload, _ := json.Marshal(map[string]string{
			"room":          room,
			"password_hash": passHash,
			"name":          name,
		})
		c.sendMsg(Message{
			Type:    "join",
			Room:    room,
			Payload: json.RawMessage(joinPayload),
		})

		// Re-announce STUN info
		if c.publicAddr != "" {
			c.sendStunInfo("")
		}

		c.emit(EventReconnected, LogEvent{Level: "info", Message: fmt.Sprintf("Reconnected (ID: %s)", c.MyID)})
		return true
	}
}

func splitMessages(data []byte) [][]byte {
	var msgs [][]byte
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			msgs = append(msgs, []byte(trimmed))
		}
	}
	return msgs
}

func (c *Client) handleMessage(msg Message) {
	switch msg.Type {
	case "peer_list":
		c.handlePeerList(msg)
	case "relay_data":
		c.handleRelayData(msg)
	case "stun_info":
		c.handleStunInfo(msg)
	case "error":
		var errInfo struct {
			Error string `json:"error"`
		}
		if msg.Payload != nil {
			json.Unmarshal(msg.Payload, &errInfo)
		}
		if errInfo.Error != "" {
			c.emit(EventError, LogEvent{Level: "error", Message: "Server: " + errInfo.Error})
		}
	}
}

func (c *Client) handlePeerList(msg Message) {
	var peers []PeerInfo
	if err := json.Unmarshal(msg.Payload, &peers); err != nil {
		return
	}

	c.peersMu.Lock()
	oldPeers := c.peers
	c.peers = peers
	c.peersMu.Unlock()

	// Build maps for join/leave detection
	oldMap := make(map[string]bool)
	for _, p := range oldPeers {
		oldMap[p.ID] = true
	}
	newMap := make(map[string]bool)
	for _, p := range peers {
		newMap[p.ID] = true
	}

	// Detect new peers and send stun_info
	for _, p := range peers {
		if p.ID == c.MyID {
			continue
		}
		if !oldMap[p.ID] {
			displayName := p.Name
			if displayName == "" {
				displayName = shortID(p.ID)
			}
			c.emit(EventPeerJoined, PeerEvent{ID: p.ID, Name: displayName, Status: p.Status})

			// Initialize PeerConn for new peer
			c.peerConnsMu.Lock()
			if _, exists := c.peerConns[p.ID]; !exists {
				c.peerConns[p.ID] = &PeerConn{
					PeerID:  p.ID,
					Mode:    "connecting",
					UDPConn: c.udpConn,
				}
			}
			c.peerConnsMu.Unlock()

			// Send our STUN info to the new peer
			if c.publicAddr != "" {
				c.sendStunInfo(p.ID)
			}
		}
	}

	// Detect peers that left
	for _, p := range oldPeers {
		if !newMap[p.ID] && p.ID != c.MyID {
			displayName := p.Name
			if displayName == "" {
				displayName = shortID(p.ID)
			}
			c.emit(EventPeerLeft, PeerEvent{ID: p.ID, Name: displayName})

			c.peerConnsMu.Lock()
			delete(c.peerConns, p.ID)
			c.peerConnsMu.Unlock()
		}
	}

	c.emit(EventPeerListUpdated, peers)
}

func (c *Client) handleRelayData(msg Message) {
	var envelope RelayEnvelope
	if err := json.Unmarshal(msg.Payload, &envelope); err != nil {
		return
	}
	inner := Message{
		Type:    envelope.Type,
		From:    msg.From,
		To:      msg.To,
		Payload: envelope.Payload,
	}
	switch envelope.Type {
	case "open_tunnel":
		c.handleOpenTunnel(inner)
	case "tunnel_opened":
		c.handleTunnelOpened(inner)
	case "tunnel_data":
		c.handleTunnelData(inner)
	case "close_tunnel":
		c.handleCloseTunnel(inner)
	case "tunnel_rejected":
		c.handleTunnelRejected(inner)
	case "speed_test_request":
		c.handleSpeedTestRequest(inner)
	case "speed_test_ready":
		c.handleSpeedTestReady(inner)
	case "speed_test_data":
		c.handleSpeedTestData(inner)
	case "speed_test_done":
		c.handleSpeedTestDone(inner)
	}
}

// resolvePeerID finds a peer ID by prefix match or name match.
func (c *Client) resolvePeerID(input string) (string, error) {
	c.peersMu.RLock()
	defer c.peersMu.RUnlock()

	var matches []string
	for _, p := range c.peers {
		if p.ID == input {
			return p.ID, nil
		}
		if p.Name != "" && strings.EqualFold(p.Name, input) {
			return p.ID, nil
		}
		if strings.HasPrefix(p.ID, input) {
			matches = append(matches, p.ID)
		}
		if p.Name != "" && strings.HasPrefix(strings.ToLower(p.Name), strings.ToLower(input)) {
			matches = append(matches, p.ID)
		}
	}

	// Deduplicate
	seen := make(map[string]bool)
	var unique []string
	for _, m := range matches {
		if !seen[m] {
			seen[m] = true
			unique = append(unique, m)
		}
	}

	switch len(unique) {
	case 0:
		return "", fmt.Errorf("no peer matching %q", input)
	case 1:
		return unique[0], nil
	default:
		return "", fmt.Errorf("ambiguous peer %q: %d matches", input, len(unique))
	}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
