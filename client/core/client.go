package core

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Client holds all state for the networking core.
type Client struct {
	// Exported config (read-only after creation)
	Config    ClientConfig
	MyID      string
	MachineID string // deterministic ID from MAC + name

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

	// File transfer tracking
	fileTransfers   map[string]*activeFileTransfer
	fileTransfersMu sync.RWMutex

	// Multi-hop relay bridges (B's perspective: bridging A↔C)
	hops              map[string]*HopBridge  // hopID → bridge
	hopBridgeByTunnel map[string]*HopBridge  // tunnelID → bridge (both inbound and outbound)
	hopsMu            sync.RWMutex

	// TUN VPN (multiple simultaneous connections)
	tunDevices map[string]*TunDevice // peerID → device
	tunMu      sync.RWMutex
	tunAckChs  map[string]chan string // peerID → ack channel

	// Per-peer gVisor netstack for port forwarding
	fwdNetstacks   map[string]*forwardNetstack // peerID → netstack
	fwdNetstacksMu sync.RWMutex

	// Peer leave debounce: delay "peer left" to handle brief disconnects
	pendingLeaves   map[string]*time.Timer // name → cancel timer
	pendingLeavesMu sync.Mutex

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

	// Generate deterministic client ID from MAC address + name
	machineID := generateMachineID(cfg.Name)

	return &Client{
		Config:       cfg,
		MachineID:    machineID,
		events:       make(chan Event, 256),
		room:         cfg.Room,
		passwordHash: hash,
		name:         cfg.Name,
		verbose:      cfg.Verbose,
		forwards:     make(map[int]*Forward),
		tunnels:      make(map[string]*TunnelConn),
		peerConns:    make(map[string]*PeerConn),
		speedTests:   make(map[string]*activeSpeedTest),
		fileTransfers: make(map[string]*activeFileTransfer),
		hops:              make(map[string]*HopBridge),
		hopBridgeByTunnel: make(map[string]*HopBridge),
		pendingLeaves: make(map[string]*time.Timer),
		fwdNetstacks:  make(map[string]*forwardNetstack),
		tunDevices:    make(map[string]*TunDevice),
		tunAckChs:     make(map[string]chan string),
		allowForward: true,
		localOnly:    true,
		done:         make(chan struct{}),
	}
}

// Events returns the read-only event channel.
func (c *Client) Events() <-chan Event {
	return c.events
}

// ReportFeatures sends current active features to the server for dashboard display.
func (c *Client) ReportFeatures() {
	features := make(map[string]string)

	// VPN status
	c.tunMu.RLock()
	var vpnPeers []string
	for _, dev := range c.tunDevices {
		vpnPeers = append(vpnPeers, dev.peerName)
		if len(dev.routes) > 0 {
			features["vpn_routes"] = fmt.Sprintf("%v", dev.routes)
		}
	}
	if len(vpnPeers) > 0 {
		features["vpn"] = fmt.Sprintf("%v", vpnPeers)
	}
	c.tunMu.RUnlock()

	// Forward count
	c.forwardsMu.RLock()
	if len(c.forwards) > 0 {
		features["forwards"] = fmt.Sprintf("%d", len(c.forwards))
	}
	c.forwardsMu.RUnlock()

	featJSON, _ := json.Marshal(features)
	c.sendMsg(Message{
		Type:    "feature_update",
		Room:    c.room,
		Payload: json.RawMessage(featJSON),
	})
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
	// Pass machine ID as query parameter so server uses it as client ID
	serverURL := c.Config.ServerURL
	if c.MachineID != "" {
		sep := "?"
		if strings.Contains(serverURL, "?") {
			sep = "&"
		}
		serverURL += sep + "client_id=" + c.MachineID
	}
	conn, _, err := dialer.Dial(serverURL, nil)
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

	// Setup WebSocket keepalive (ping/pong)
	c.setupWSKeepAlive()

	// Start WebSocket read loop
	c.wg.Add(1)
	go c.readLoop()

	return nil
}

// setupWSKeepAlive configures ping/pong handlers and starts a ping sender goroutine.
// This keeps the WebSocket connection alive through NATs and load balancers.
func (c *Client) setupWSKeepAlive() {
	const pingPeriod = 30 * time.Second

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return
	}

	// No ReadDeadline on client side — we rely on ping/pong for liveness.
	// Setting ReadDeadline causes false disconnects when VPN data flows heavily.
	conn.SetReadDeadline(time.Time{}) // clear any deadline

	// Start ping sender goroutine — keeps NAT/firewall mappings alive
	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-ticker.C:
				c.connMu.Lock()
				curConn := c.conn
				c.connMu.Unlock()
				if curConn != conn {
					return // connection was replaced (reconnect), stop this goroutine
				}
				c.connMu.Lock()
				err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
				c.connMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
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

	// Stop TUN VPN if active
	c.tunCleanup()

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

	// Close active file transfers
	c.fileTransfersMu.Lock()
	for id, ft := range c.fileTransfers {
		ft.mu.Lock()
		if ft.File != nil {
			ft.File.Close()
		}
		select {
		case <-ft.Done:
		default:
			close(ft.Done)
		}
		ft.mu.Unlock()
		delete(c.fileTransfers, id)
	}
	c.fileTransfersMu.Unlock()

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

	// E2E encrypt relay data if we have a shared key with this peer
	// (skip key_exchange messages — they establish the key)
	finalPayload := envelope
	if innerType != "key_exchange" {
		c.peerConnsMu.RLock()
		pc, ok := c.peerConns[to]
		c.peerConnsMu.RUnlock()
		if ok && pc.Crypto != nil && pc.Crypto.Encrypted {
			encrypted, err := pc.Crypto.Encrypt(envelope)
			if err == nil {
				// Wrap in encrypted envelope
				encEnv, _ := json.Marshal(RelayEnvelope{
					Type:    "encrypted",
					Payload: json.RawMessage(`"` + base64.StdEncoding.EncodeToString(encrypted) + `"`),
				})
				finalPayload = encEnv
			}
		}
	}

	return c.sendMsg(Message{
		Type:    "relay_data",
		To:      to,
		Payload: json.RawMessage(finalPayload),
	})
}

// sendViaP2P sends a message to a peer via UDP P2P first, relay as fallback.
// prefix: UDP frame prefix (e.g. "SM:", "ST:", "SF:")
// udpPayload: raw bytes to send via UDP (prefix + payload)
// relayType/relayPayload: for relay fallback
func (c *Client) sendViaP2P(peerID string, udpPayload []byte, relayType string, relayPayload interface{}) {
	// Try UDP P2P
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
			if _, err := udp.WriteToUDP(udpPayload, addr); err == nil {
				return
			}
		}
	}

	// Fallback to relay
	c.sendRelay(peerID, relayType, relayPayload)
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
		if c.MachineID != "" {
			sep := "?"
			if strings.Contains(serverURL, "?") {
				sep = "&"
			}
			serverURL += sep + "client_id=" + c.MachineID
		}
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

		// Reset all P2P state — old connections are dead after network change
		c.resetP2PState()

		// Setup keepalive on new connection
		c.setupWSKeepAlive()

		// Re-discover STUN (network may have changed, old public addr is stale)
		if !c.Config.NoSTUN {
			servers := c.Config.STUNServers
			if len(servers) == 0 {
				servers = []string{"stun.cloudflare.com:3478", "stun.miwifi.com:3478"}
			}
			go c.DiscoverSTUN(servers)
		}

		c.emit(EventReconnected, LogEvent{Level: "info", Message: fmt.Sprintf("Reconnected (ID: %s)", c.MyID)})
		return true
	}
}

// resetP2PState clears all stale P2P connections after a reconnect.
func (c *Client) resetP2PState() {
	// Close old UDP socket
	c.connMu.Lock()
	if c.udpConn != nil {
		c.udpConn.Close()
		c.udpConn = nil
	}
	c.publicAddr = ""
	c.connMu.Unlock()

	// Reset all peer connections to "connecting"
	c.peerConnsMu.Lock()
	for _, pc := range c.peerConns {
		if pc.DirectTCP != nil {
			pc.DirectTCP.Close()
			pc.DirectTCP = nil
		}
		pc.Mode = "connecting"
		pc.UDPAddr = nil
		pc.Crypto = nil
	}
	c.peerConnsMu.Unlock()
}

// generateMachineID creates a deterministic client ID from MAC address + name.
// Same machine + same name = same ID across restarts and reconnects.
func generateMachineID(name string) string {
	mac := getPrimaryMAC()
	var seed string
	if mac != nil {
		seed = mac.String() + ":" + name
	} else {
		h, _ := os.Hostname()
		seed = h + ":" + name
	}
	hash := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(hash[:8]) // 16-char hex ID
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
	oldNameMap := make(map[string]string) // name → ID
	for _, p := range oldPeers {
		oldMap[p.ID] = true
		if p.Name != "" {
			oldNameMap[p.Name] = p.ID
		}
	}
	newMap := make(map[string]bool)
	newNameMap := make(map[string]string) // name → ID
	for _, p := range peers {
		newMap[p.ID] = true
		if p.Name != "" {
			newNameMap[p.Name] = p.ID
		}
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

	// Cancel pending leaves for peers that are back
	c.pendingLeavesMu.Lock()
	for name, timer := range c.pendingLeaves {
		if _, back := newNameMap[name]; back {
			timer.Stop()
			delete(c.pendingLeaves, name)
		}
	}
	c.pendingLeavesMu.Unlock()

	// Detect peers that left (debounced — wait 5s before confirming)
	for _, p := range oldPeers {
		if !newMap[p.ID] && p.ID != c.MyID {
			// Check if same name still present (reconnected with new ID)
			if p.Name != "" {
				if newID, ok := newNameMap[p.Name]; ok {
					// Update VPN peer ID if needed
					c.tunMu.Lock()
					if dev, ok := c.tunDevices[p.ID]; ok {
						delete(c.tunDevices, p.ID)
						dev.peerID = newID
						c.tunDevices[newID] = dev
						c.tunMu.Unlock()
						c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("VPN peer %s reconnected with new ID", p.Name)})
					} else {
						c.tunMu.Unlock()
					}
					continue
				}
			}

			// Debounce: schedule peer_left after 5 seconds
			peerCopy := p
			displayName := p.Name
			if displayName == "" {
				displayName = shortID(p.ID)
			}

			c.pendingLeavesMu.Lock()
			if _, pending := c.pendingLeaves[displayName]; !pending {
				c.pendingLeaves[displayName] = time.AfterFunc(5*time.Second, func() {
					c.pendingLeavesMu.Lock()
					delete(c.pendingLeaves, displayName)
					c.pendingLeavesMu.Unlock()

					// Re-check: is this peer (by name) still absent?
					c.peersMu.RLock()
					stillGone := true
					for _, cur := range c.peers {
						if cur.Name == peerCopy.Name && peerCopy.Name != "" {
							stillGone = false
							break
						}
						if cur.ID == peerCopy.ID {
							stillGone = false
							break
						}
					}
					c.peersMu.RUnlock()

					if !stillGone {
						return // peer came back, skip
					}

					c.emit(EventPeerLeft, PeerEvent{ID: peerCopy.ID, Name: displayName})

					c.peerConnsMu.Lock()
					delete(c.peerConns, peerCopy.ID)
					c.peerConnsMu.Unlock()

					// Clean up VPN if this peer was our VPN partner
					c.tunMu.Lock()
					if dev, ok := c.tunDevices[peerCopy.ID]; ok {
						delete(c.tunDevices, peerCopy.ID)
						c.tunMu.Unlock()
						dev.closeOnce.Do(func() { close(dev.done) })
						if dev.nsProxy != nil {
							dev.nsProxy.Close()
						}
						if dev.proxy != nil {
							dev.proxy.Close()
						}
						if dev.iface != nil {
							dev.iface.Close()
						}
						for _, route := range dev.routes {
							removeRoute(dev.ifName, route)
						}
						cleanupSNATRoute(dev.ifName, dev.snatIP)
						disableNAT(dev.ifName)
						removeTunInterface(dev.ifName)
						c.emit(EventTunStopped, LogEvent{Level: "info", Message: fmt.Sprintf("VPN stopped: peer %s disconnected", displayName)})
					} else {
						c.tunMu.Unlock()
					}
				})
			}
			c.pendingLeavesMu.Unlock()
		}
	}

	c.emit(EventPeerListUpdated, peers)
}

func (c *Client) handleRelayData(msg Message) {
	var envelope RelayEnvelope
	if err := json.Unmarshal(msg.Payload, &envelope); err != nil {
		return
	}

	// Decrypt E2E encrypted relay data
	if envelope.Type == "encrypted" {
		var encData string
		if err := json.Unmarshal(envelope.Payload, &encData); err != nil {
			return
		}
		ciphertext, err := base64.StdEncoding.DecodeString(encData)
		if err != nil {
			return
		}
		c.peerConnsMu.RLock()
		pc, ok := c.peerConns[msg.From]
		c.peerConnsMu.RUnlock()
		if !ok || pc.Crypto == nil || !pc.Crypto.Encrypted {
			return // can't decrypt without key
		}
		plaintext, err := pc.Crypto.Decrypt(ciphertext)
		if err != nil {
			return
		}
		// Re-parse the decrypted envelope
		if err := json.Unmarshal(plaintext, &envelope); err != nil {
			return
		}
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
	case "reverse_forward_offer":
		c.handleReverseForwardOffer(inner)
	case "reverse_forward_accept":
		c.handleReverseForwardAccept(inner)
	case "reverse_forward_reject":
		c.handleReverseForwardReject(inner)
	case "speed_test_request":
		c.handleSTBegin(inner) // legacy compat
	case "st_begin":
		c.handleSTBegin(inner)
	case "speed_test_ready":
		c.handleSTReady(inner) // legacy compat
	case "st_ready":
		c.handleSTReady(inner)
	case "speed_test_data":
		c.handleSpeedTestData(inner)
	case "speed_test_done":
		c.handleSTFinish(inner) // legacy compat
	case "st_finish":
		c.handleSTFinish(inner)
	case "st_result":
		c.handleSTResult(inner)
	case "file_offer":
		c.handleFileOffer(inner)
	case "file_accept":
		c.handleFileAccept(inner)
	case "file_data":
		c.handleFileData(inner)
	case "file_done":
		c.handleFileDone(inner)
	case "file_reject":
		c.handleFileReject(inner)
	case "file_cancel":
		c.handleFileCancel(inner)
	case "hop_forward":
		c.handleHopForward(inner)
	case "hop_forward_accept":
		c.handleHopForwardAccept(inner)
	case "hop_forward_reject":
		c.handleHopForwardReject(inner)
	case "tun_setup":
		c.handleTunSetup(inner)
	case "tun_ack":
		c.handleTunAck(inner)
	case "tun_data":
		c.handleTunData(inner)
	case "tun_teardown":
		c.handleTunTeardown(inner)
	case "fwd_data":
		c.handleFwdData(inner)
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
