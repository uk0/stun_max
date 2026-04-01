package core

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// UDP packet prefixes
var (
	prefixPunch    = []byte("PUNCH:")
	prefixPunchAck = []byte("PUNCH_ACK:")
	prefixKey      = []byte("KEY:")
	prefixKeyAck   = []byte("KEY_ACK:")
	prefixData     = []byte{0x00} // encrypted tunnel data marker
)

// stunDiscover sends a STUN Binding Request and parses the XOR-MAPPED-ADDRESS.
func stunDiscover(stunServer string) (publicAddr string, localPort int, conn *net.UDPConn, err error) {
	serverAddr, err := net.ResolveUDPAddr("udp4", stunServer)
	if err != nil {
		return "", 0, nil, fmt.Errorf("resolve STUN server: %w", err)
	}

	conn, err = net.ListenUDP("udp4", nil)
	if err != nil {
		return "", 0, nil, fmt.Errorf("listen UDP: %w", err)
	}

	localPort = conn.LocalAddr().(*net.UDPAddr).Port

	// Build STUN Binding Request (20 bytes header, no attributes)
	req := make([]byte, StunHeaderSize)
	binary.BigEndian.PutUint16(req[0:2], StunBindingRequest)
	binary.BigEndian.PutUint16(req[2:4], 0)
	binary.BigEndian.PutUint32(req[4:8], StunMagicCookie)

	// Transaction ID: 12 random bytes
	txID := make([]byte, 12)
	if _, err := rand.Read(txID); err != nil {
		conn.Close()
		return "", 0, nil, fmt.Errorf("generate transaction ID: %w", err)
	}
	copy(req[8:20], txID)

	conn.SetWriteDeadline(time.Now().Add(StunTimeout))
	if _, err := conn.WriteToUDP(req, serverAddr); err != nil {
		conn.Close()
		return "", 0, nil, fmt.Errorf("send STUN request: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(StunTimeout))
	buf := make([]byte, 1024)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		conn.Close()
		return "", 0, nil, fmt.Errorf("read STUN response: %w", err)
	}

	// Clear deadline for future use
	conn.SetReadDeadline(time.Time{})
	conn.SetWriteDeadline(time.Time{})

	if n < StunHeaderSize {
		conn.Close()
		return "", 0, nil, fmt.Errorf("STUN response too short: %d bytes", n)
	}

	resp := buf[:n]

	// Verify it's a Binding Response (0x0101)
	msgType := binary.BigEndian.Uint16(resp[0:2])
	if msgType != 0x0101 {
		conn.Close()
		return "", 0, nil, fmt.Errorf("unexpected STUN message type: 0x%04x", msgType)
	}

	// Verify transaction ID matches
	if !bytes.Equal(resp[8:20], txID) {
		conn.Close()
		return "", 0, nil, fmt.Errorf("STUN transaction ID mismatch")
	}

	// Parse attributes to find XOR-MAPPED-ADDRESS (0x0020)
	msgLen := binary.BigEndian.Uint16(resp[2:4])
	attrs := resp[StunHeaderSize : StunHeaderSize+int(msgLen)]
	publicAddr, err = parseXorMappedAddress(attrs)
	if err != nil {
		conn.Close()
		return "", 0, nil, err
	}

	return publicAddr, localPort, conn, nil
}

// parseXorMappedAddress walks STUN attributes and extracts the XOR-MAPPED-ADDRESS.
func parseXorMappedAddress(attrs []byte) (string, error) {
	offset := 0
	for offset+4 <= len(attrs) {
		attrType := binary.BigEndian.Uint16(attrs[offset : offset+2])
		attrLen := int(binary.BigEndian.Uint16(attrs[offset+2 : offset+4]))
		offset += 4

		if offset+attrLen > len(attrs) {
			break
		}

		if attrType == StunAttrXorMapped {
			addr, _, err := decodeXorAddress(attrs[offset:offset+attrLen], true)
			return addr, err
		}

		// STUN attributes are padded to 4-byte boundaries
		offset += attrLen
		if attrLen%4 != 0 {
			offset += 4 - (attrLen % 4)
		}
	}
	return "", fmt.Errorf("XOR-MAPPED-ADDRESS not found in STUN response")
}

// decodeXorAddress decodes an XOR-MAPPED-ADDRESS attribute value (IPv4 only).
func decodeXorAddress(data []byte, xor bool) (string, int, error) {
	if len(data) < 8 {
		return "", 0, fmt.Errorf("XOR-MAPPED-ADDRESS too short: %d", len(data))
	}

	family := data[1]
	if family != 0x01 {
		return "", 0, fmt.Errorf("unsupported address family: 0x%02x (only IPv4)", family)
	}

	// Port: XOR with top 16 bits of magic cookie (0x2112)
	xorPort := binary.BigEndian.Uint16(data[2:4])
	port := int(xorPort)
	if xor {
		port = int(xorPort ^ uint16(StunMagicCookie>>16))
	}

	// IP: XOR with magic cookie (0x2112A442)
	xorIP := binary.BigEndian.Uint32(data[4:8])
	ip := xorIP
	if xor {
		ip = xorIP ^ StunMagicCookie
	}

	ipAddr := net.IPv4(byte(ip>>24), byte(ip>>16), byte(ip>>8), byte(ip))
	return fmt.Sprintf("%s:%d", ipAddr.String(), port), port, nil
}

func generateTunnelID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func tunnelIDToBytes(id string) []byte {
	b, err := hex.DecodeString(id)
	if err != nil || len(b) != 8 {
		raw := []byte(id)
		if len(raw) >= 8 {
			return raw[:8]
		}
		padded := make([]byte, 8)
		copy(padded, raw)
		return padded
	}
	return b
}

func tunnelIDFromBytes(b []byte) string {
	return hex.EncodeToString(b)
}

// DiscoverSTUN tries each STUN server until one succeeds.
func (c *Client) DiscoverSTUN(servers []string) error {
	for _, srv := range servers {
		srv = strings.TrimSpace(srv)
		if srv == "" {
			continue
		}
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("STUN: trying %s ...", srv)})
		publicAddr, _, udpConn, err := stunDiscover(srv)
		if err != nil {
			c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf("STUN: %s failed: %v", srv, err)})
			continue
		}
		c.publicAddr = publicAddr
		c.udpConn = udpConn
		c.emit(EventStunDiscovered, LogEvent{Level: "info", Message: fmt.Sprintf("STUN: public endpoint %s (via %s)", publicAddr, srv)})

		// Start UDP read loop
		c.wg.Add(1)
		go c.udpReadLoop()

		// Start retry loop for relay peers
		c.wg.Add(1)
		go c.startRetryLoop()

		// Start direct TCP listener for P2P upgrades
		c.startDirectTCPListener()

		// Broadcast our STUN info to the room
		c.sendStunInfo("")

		return nil
	}
	return fmt.Errorf("all STUN servers failed")
}

func (c *Client) sendStunInfo(to string) {
	if c.publicAddr == "" {
		return
	}
	localAddr := getLocalIP()
	var localUDP string
	c.connMu.Lock()
	udp := c.udpConn
	c.connMu.Unlock()
	if localAddr != "" && udp != nil {
		localPort := udp.LocalAddr().(*net.UDPAddr).Port
		localUDP = fmt.Sprintf("%s:%d", localAddr, localPort)
	}
	payload, _ := json.Marshal(map[string]string{
		"addr":  c.publicAddr,
		"local": localUDP,
	})
	c.sendMsg(Message{
		Type:    "stun_info",
		To:      to,
		Room:    c.room,
		Payload: json.RawMessage(payload),
	})
}

func (c *Client) sendStatusUpdate(status string) {
	statusJSON, _ := json.Marshal(status)
	c.sendMsg(Message{
		Type:    "status_update",
		Room:    c.room,
		Payload: json.RawMessage(statusJSON),
	})
}

func (c *Client) handleStunInfo(msg Message) {
	if msg.From == "" || msg.From == c.MyID {
		return
	}
	var info struct {
		Addr  string `json:"addr"`
		Local string `json:"local"`
	}
	if err := json.Unmarshal(msg.Payload, &info); err != nil || info.Addr == "" {
		return
	}

	targetAddr := info.Addr
	isLAN := false
	if info.Local != "" && c.publicAddr != "" {
		myPubIP, _, _ := net.SplitHostPort(c.publicAddr)
		peerPubIP, _, _ := net.SplitHostPort(info.Addr)
		if myPubIP != "" && myPubIP == peerPubIP {
			targetAddr = info.Local
			isLAN = true
		}
	}

	udpAddr, err := net.ResolveUDPAddr("udp4", targetAddr)
	if err != nil {
		return
	}

	c.peerConnsMu.Lock()
	pc, exists := c.peerConns[msg.From]
	if !exists {
		pc = &PeerConn{
			PeerID:  msg.From,
			Mode:    "connecting",
			UDPConn: c.udpConn,
		}
		c.peerConns[msg.From] = pc
	}

	// CRITICAL: don't overwrite UDPAddr if peer is already direct and working.
	// Changing addr mid-stream breaks crypto lookup and causes tunnel flaps.
	if pc.Mode == "direct" {
		c.peerConnsMu.Unlock()
		return // already connected, ignore new stun_info
	}

	pc.UDPAddr = udpAddr
	c.peerConnsMu.Unlock()

	if isLAN {
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("LAN peer detected: %s → using local address %s", shortID(msg.From), targetAddr)})
	} else if c.verbose {
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Received STUN endpoint from %s: %s", shortID(msg.From), info.Addr)})
	}

	// Send our stun_info back if we have one
	if c.publicAddr != "" {
		c.sendStunInfo(msg.From)
	}

	// Attempt hole punch if we have a UDP socket
	if c.udpConn != nil && pc.Mode != "direct" {
		go c.attemptHolePunch(msg.From, isLAN)
	}
}

func (c *Client) attemptHolePunch(peerID string, isLAN bool) {
	c.peerConnsMu.RLock()
	pc := c.peerConns[peerID]
	c.peerConnsMu.RUnlock()

	c.connMu.Lock()
	udp := c.udpConn
	c.connMu.Unlock()

	if pc == nil || pc.UDPAddr == nil || udp == nil {
		return
	}

	c.peerConnsMu.Lock()
	pc.LastPunch = time.Now()
	if pc.Crypto == nil {
		crypto, err := NewPeerCrypto()
		if err == nil {
			pc.Crypto = crypto
		}
	}
	c.peerConnsMu.Unlock()

	addr := pc.UDPAddr
	myID := []byte(c.MyID)

	// Phase 1: Rapid burst — 20 packets in 500ms from main socket
	punch := append([]byte("PUNCH:"), myID...)
	for i := 0; i < 20; i++ {
		select {
		case <-c.done:
			return
		default:
		}
		udp.WriteToUDP(punch, addr)
		time.Sleep(25 * time.Millisecond)
	}

	// Phase 2 & 3: only for WAN (Birthday Attack + port prediction).
	// Skip for LAN — no NAT to punch through, and extra sockets cause ICMP errors.
	if isLAN {
		return
	}

	// Phase 2: Multi-socket parallel punch (Birthday Attack style)
	// Open 8 extra sockets and punch from each — increases probability
	// of hitting the right NAT mapping for Symmetric NATs
	var extraConns []*net.UDPConn
	for i := 0; i < 8; i++ {
		conn, err := net.ListenUDP("udp4", nil)
		if err != nil {
			continue
		}
		extraConns = append(extraConns, conn)
	}

	if len(extraConns) > 0 {
		var wg sync.WaitGroup
		for _, conn := range extraConns {
			wg.Add(1)
			go func(c2 *net.UDPConn) {
				defer wg.Done()
				for j := 0; j < 5; j++ {
					c2.WriteToUDP(punch, addr)
					time.Sleep(50 * time.Millisecond)
				}
			}(conn)
		}
		wg.Wait()
		for _, conn := range extraConns {
			conn.Close()
		}
	}

	// Phase 3: Port prediction for Easy Symmetric NAT
	// Try ports around the known port ±10
	basePort := addr.Port
	for delta := -10; delta <= 10; delta++ {
		if delta == 0 {
			continue
		}
		predictedPort := basePort + delta
		if predictedPort <= 0 || predictedPort > 65535 {
			continue
		}
		predictedAddr := &net.UDPAddr{IP: addr.IP, Port: predictedPort}
		udp.WriteToUDP(punch, predictedAddr)
	}
}

func (c *Client) onHolePunchSuccess(peerID string, addr *net.UDPAddr) {
	c.peerConnsMu.Lock()
	pc, exists := c.peerConns[peerID]
	if !exists {
		pc = &PeerConn{
			PeerID:  peerID,
			UDPConn: c.udpConn,
		}
		c.peerConns[peerID] = pc
	}
	if pc.Mode == "direct" {
		c.peerConnsMu.Unlock()
		return
	}
	wasRelay := pc.Mode == "relay"
	pc.Mode = "direct"
	pc.UDPAddr = addr
	pc.PunchFails = 0 // reset on success

	if pc.Crypto == nil {
		crypto, err := NewPeerCrypto()
		if err == nil {
			pc.Crypto = crypto
		}
	}
	c.peerConnsMu.Unlock()

	if wasRelay {
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Upgraded %s from RELAY to P2P", shortID(peerID))})
	}
	c.emit(EventHolePunchSuccess, PeerEvent{ID: peerID, Status: "direct"})
	c.sendStatusUpdate("direct")
	c.sendKeyExchange(peerID)

	// Attempt direct TCP connection to peer for reliable tunnel data.
	// This runs in background — if it fails, relay is used automatically.
	go c.attemptDirectTCP(peerID, addr)
}

// sendKeyExchange sends our X25519 public key to a peer over UDP.
func (c *Client) sendKeyExchange(peerID string) {
	c.peerConnsMu.RLock()
	pc := c.peerConns[peerID]
	c.peerConnsMu.RUnlock()
	c.connMu.Lock()
	udp := c.udpConn
	c.connMu.Unlock()
	if pc == nil || pc.UDPAddr == nil || pc.Crypto == nil || udp == nil {
		return
	}

	msg := append([]byte("KEY:"+c.MyID+":"), pc.Crypto.PubKey...)
	udp.WriteToUDP(msg, pc.UDPAddr)
}

// handleKeyExchange processes an incoming public key and derives the shared secret.
func (c *Client) handleKeyExchange(peerID string, peerPubKey []byte, addr *net.UDPAddr) {
	c.peerConnsMu.Lock()
	pc, exists := c.peerConns[peerID]
	if !exists {
		c.peerConnsMu.Unlock()
		return
	}

	if pc.Crypto == nil {
		crypto, err := NewPeerCrypto()
		if err != nil {
			c.peerConnsMu.Unlock()
			return
		}
		pc.Crypto = crypto
	}

	if pc.Crypto.Encrypted {
		c.peerConnsMu.Unlock()
		return // already established
	}

	if err := pc.Crypto.DeriveKey(peerPubKey); err != nil {
		c.peerConnsMu.Unlock()
		c.emit(EventLog, LogEvent{Level: "error", Message: fmt.Sprintf("Key exchange failed with %s: %v", shortID(peerID), err)})
		return
	}
	c.peerConnsMu.Unlock()

	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Encrypted channel established with %s (X25519+AES-256-GCM)", shortID(peerID))})

	// Send KEY_ACK with our public key
	c.peerConnsMu.RLock()
	pubKey := pc.Crypto.PubKey
	c.peerConnsMu.RUnlock()
	ack := append([]byte("KEY_ACK:"+c.MyID+":"), pubKey...)
	c.connMu.Lock()
	udp := c.udpConn
	c.connMu.Unlock()
	if udp != nil {
		udp.WriteToUDP(ack, addr)
	}
}

func (c *Client) udpReadLoop() {
	defer c.wg.Done()
	buf := make([]byte, 65536)
	for {
		select {
		case <-c.done:
			return
		default:
		}
		// Check if udpConn was closed (e.g. by resetP2PState during reconnect)
		c.connMu.Lock()
		conn := c.udpConn
		c.connMu.Unlock()
		if conn == nil {
			return // socket closed, exit this goroutine
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			// Timeout is normal — just loop and check c.done
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			// Any other error: DON'T exit. Transient errors (ICMP unreachable,
			// buffer overflow, etc.) should not kill the entire P2P receive path.
			// Only exit when c.done is closed.
			select {
			case <-c.done:
				return
			default:
				continue // ignore transient error, keep reading
			}
		}
		if n == 0 {
			continue
		}
		data := buf[:n]

		// PUNCH handshake
		if bytes.HasPrefix(data, prefixPunch) && !bytes.HasPrefix(data, prefixPunchAck) {
			peerID := string(data[len(prefixPunch):])
			c.onHolePunchSuccess(peerID, addr)
			conn.WriteToUDP(append([]byte("PUNCH_ACK:"), []byte(c.MyID)...), addr)
			continue
		}
		if bytes.HasPrefix(data, prefixPunchAck) {
			peerID := string(data[len(prefixPunchAck):])
			c.onHolePunchSuccess(peerID, addr)
			continue
		}

		// Key exchange
		if bytes.HasPrefix(data, prefixKey) && !bytes.HasPrefix(data, []byte("KEY_ACK:")) {
			// KEY:<peerID>:<32-byte-pubkey>
			rest := data[len(prefixKey):]
			if idx := bytes.IndexByte(rest, ':'); idx > 0 && idx+32 < len(rest) {
				peerID := string(rest[:idx])
				pubKey := rest[idx+1:]
				if len(pubKey) == 32 {
					c.handleKeyExchange(peerID, pubKey, addr)
				}
			}
			continue
		}
		if bytes.HasPrefix(data, prefixKeyAck) {
			rest := data[len(prefixKeyAck):]
			if idx := bytes.IndexByte(rest, ':'); idx > 0 && idx+32 < len(rest) {
				peerID := string(rest[:idx])
				pubKey := rest[idx+1:]
				if len(pubKey) == 32 {
					c.handleKeyExchange(peerID, pubKey, addr)
				}
			}
			continue
		}

		// Keepalive PING — just ignore (keeps NAT mapping alive)
		if n == 4 && string(data) == "PING" {
			continue
		}

		// Tunnel data over UDP: [0x00][8-byte tunnel_id][plaintext payload]
		if n > 9 && data[0] == 0x00 {
			tunnelID := tunnelIDFromBytes(data[1:9])
			c.handleUDPTunnelData(tunnelID, data[9:])
			continue
		}

		// Legacy unencrypted tunnel data: [8-byte tunnel_id][raw data]
		if n > 8 {
			tunnelID := tunnelIDFromBytes(data[:8])
			c.handleUDPTunnelData(tunnelID, data[8:])
		}
	}
}

func (c *Client) handleUDPTunnelData(tunnelID string, data []byte) {
	c.tunnelsMu.RLock()
	tc, ok := c.tunnels[tunnelID]
	c.tunnelsMu.RUnlock()

	if !ok {
		return
	}

	if tc.Forward != nil {
		atomic.AddInt64(&tc.Forward.BytesDown, int64(len(data)))
	}

	// Generous deadline — don't kill tunnel on temporary TCP backpressure
	tc.Conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	tc.Conn.Write(data) // ignore error — TCP keepalive will detect dead conn
}

// handleEncryptedUDPData decrypts and processes tunnel data from a P2P peer.
func (c *Client) handleEncryptedUDPData(tunnelID string, encrypted []byte, addr *net.UDPAddr) {
	// Look up crypto by tunnel's peer ID (more reliable than addr matching)
	var crypto *PeerCrypto
	c.tunnelsMu.RLock()
	tc, ok := c.tunnels[tunnelID]
	c.tunnelsMu.RUnlock()
	if ok && tc.PeerID != "" {
		c.peerConnsMu.RLock()
		if pc, exists := c.peerConns[tc.PeerID]; exists {
			crypto = pc.Crypto
		}
		c.peerConnsMu.RUnlock()
	}

	// Fallback: look up by addr
	if crypto == nil {
		c.peerConnsMu.RLock()
		for _, pc := range c.peerConns {
			if pc.UDPAddr != nil && pc.UDPAddr.IP.Equal(addr.IP) && pc.UDPAddr.Port == addr.Port {
				crypto = pc.Crypto
				break
			}
		}
		c.peerConnsMu.RUnlock()
	}

	if crypto != nil && crypto.Encrypted {
		plaintext, err := crypto.Decrypt(encrypted)
		if err == nil {
			c.handleUDPTunnelData(tunnelID, plaintext)
			return
		}
		// Decryption failed — DON'T write garbage, just drop the packet
		return
	}

	// No encryption — treat as raw data
	c.handleUDPTunnelData(tunnelID, encrypted)
}

// attemptDirectTCP tries to establish a direct TCP connection to the peer.
// On LAN this connects instantly. On WAN it may fail (NAT blocks TCP).
// If successful, tunnel data uses this TCP connection instead of WS relay.
func (c *Client) attemptDirectTCP(peerID string, addr *net.UDPAddr) {
	// Try connecting to the peer's TCP listener (same IP, port+1)
	tcpAddr := net.JoinHostPort(addr.IP.String(), fmt.Sprintf("%d", addr.Port+1))

	conn, err := net.DialTimeout("tcp", tcpAddr, 3*time.Second)
	if err != nil {
		if c.verbose {
			c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Direct TCP to %s failed (using relay): %v", shortID(peerID), err)})
		}
		return
	}

	// Send our ID so the peer knows who connected
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte("STUNMAX:" + c.MyID + "\n"))

	c.peerConnsMu.Lock()
	pc := c.peerConns[peerID]
	if pc != nil {
		pc.DirectTCP = conn
	}
	c.peerConnsMu.Unlock()

	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Direct TCP established with %s", shortID(peerID))})

	// Start reading from this direct TCP connection
	c.wg.Add(1)
	go c.directTCPReadLoop(conn)
}

// startDirectTCPListener listens for incoming direct TCP connections from peers.
func (c *Client) startDirectTCPListener() {
	c.connMu.Lock()
	udp := c.udpConn
	c.connMu.Unlock()
	if udp == nil {
		return
	}
	localPort := udp.LocalAddr().(*net.UDPAddr).Port + 1
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", localPort))
	if err != nil {
		if c.verbose {
			c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Direct TCP listener failed: %v", err)})
		}
		return
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer ln.Close()
		for {
			select {
			case <-c.done:
				return
			default:
			}
			ln.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second))
			conn, err := ln.Accept()
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}

			// Read peer ID
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 256)
			n, err := conn.Read(buf)
			if err != nil {
				conn.Close()
				continue
			}
			line := strings.TrimSpace(string(buf[:n]))
			if !strings.HasPrefix(line, "STUNMAX:") {
				conn.Close()
				continue
			}
			peerID := strings.TrimPrefix(line, "STUNMAX:")
			peerID = strings.TrimSpace(peerID)

			c.peerConnsMu.Lock()
			pc, exists := c.peerConns[peerID]
			if exists && pc.DirectTCP == nil {
				pc.DirectTCP = conn
				c.peerConnsMu.Unlock()
				c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Direct TCP accepted from %s", shortID(peerID))})
				// Start reading from this direct TCP connection
				c.wg.Add(1)
				go c.directTCPReadLoop(conn)
			} else {
				c.peerConnsMu.Unlock()
				conn.Close()
			}
		}
	}()
}

func (c *Client) startRetryLoop() {
	defer c.wg.Done()

	retryTicker := time.NewTicker(15 * time.Second)
	keepaliveTicker := time.NewTicker(10 * time.Second) // NAT keepalive every 10s (safe for most NATs)
	defer retryTicker.Stop()
	defer keepaliveTicker.Stop()

	for {
		select {
		case <-c.done:
			return

		case <-keepaliveTicker.C:
			c.peerConnsMu.RLock()
			c.connMu.Lock()
			udp := c.udpConn
			c.connMu.Unlock()
			if udp != nil {
				for _, pc := range c.peerConns {
					if pc.Mode == "direct" && pc.UDPAddr != nil {
						udp.WriteToUDP([]byte("PING"), pc.UDPAddr)
					}
				}
			}
			c.peerConnsMu.RUnlock()

		case <-retryTicker.C:
			c.peerConnsMu.Lock()
			var retryPeers []string
			for peerID, pc := range c.peerConns {
				if pc.UDPAddr == nil {
					continue
				}
				if pc.Mode == "direct" {
					continue // already connected
				}
				// After 5 failed punches, mark as relay (but keep retrying)
				if pc.Mode == "connecting" && pc.PunchFails >= 5 {
					pc.Mode = "relay"
					c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf("P2P punch failed 5 times for %s, using relay", shortID(peerID))})
				}
				retryPeers = append(retryPeers, peerID)
			}
			c.peerConnsMu.Unlock()

			for _, peerID := range retryPeers {
				go func(pid string) {
					c.attemptHolePunch(pid, false)
					// If punch didn't succeed, increment failure counter
					c.peerConnsMu.Lock()
					if pc, ok := c.peerConns[pid]; ok && pc.Mode != "direct" {
						pc.PunchFails++
					}
					c.peerConnsMu.Unlock()
				}(peerID)
			}
		}
	}
}

// getLocalIP returns the preferred outbound local IP address.
func getLocalIP() string {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// directTCPReadLoop reads framed tunnel data from a direct TCP connection.
// Frame format: [8-byte tunnelID][4-byte length][data]
func (c *Client) directTCPReadLoop(conn net.Conn) {
	defer c.wg.Done()
	defer conn.Close()

	header := make([]byte, 12)
	for {
		select {
		case <-c.done:
			return
		default:
		}

		conn.SetReadDeadline(time.Now().Add(10 * time.Minute))

		// Read header: 8-byte tunnelID + 4-byte length
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}

		tunnelID := tunnelIDFromBytes(header[:8])
		dataLen := int(header[8])<<24 | int(header[9])<<16 | int(header[10])<<8 | int(header[11])
		if dataLen <= 0 || dataLen > 256*1024 {
			return // invalid frame
		}

		// Read data
		data := make([]byte, dataLen)
		if _, err := io.ReadFull(conn, data); err != nil {
			return
		}

		// Write to tunnel
		c.tunnelsMu.RLock()
		tc, ok := c.tunnels[tunnelID]
		c.tunnelsMu.RUnlock()
		if !ok {
			continue
		}

		if tc.Forward != nil {
			atomic.AddInt64(&tc.Forward.BytesDown, int64(dataLen))
		}
		tc.Conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		tc.Conn.Write(data)
	}
}

