package core

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand"
	"net"
	"sort"
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

	conn, err = bypassListenUDP()
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
	var firstSrv string
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
		firstSrv = srv
		c.emit(EventStunDiscovered, LogEvent{Level: "info", Message: fmt.Sprintf("STUN: public endpoint %s (via %s)", publicAddr, srv)})

		// Detect NAT type in background (non-blocking)
		go c.detectNATType(publicAddr, firstSrv, servers)

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

// detectNATType determines our NAT type using RFC 5780 mapping behavior test.
// Queries multiple STUN servers from the SAME socket to check if the NAT assigns
// the same external port regardless of destination (Endpoint-Independent Mapping = Cone)
// or different ports per destination (Endpoint-Dependent Mapping = Symmetric).
//
// Filtering behavior (Full Cone vs Restricted vs Port Restricted) requires a
// dual-IP STUN server with CHANGE-REQUEST support, which standard public STUN
// servers don't provide. We conservatively assume Port Restricted (NAT3) for
// cone NATs — this only affects hole punch strategy (not correctness).
func (c *Client) detectNATType(publicAddr, primarySrv string, servers []string) {
	pubIP, _, _ := net.SplitHostPort(publicAddr)

	// Check if we're on the public internet (no NAT).
	if hasLocalIP(pubIP) {
		c.natType = NATOpen
		c.emit(EventLog, LogEvent{Level: "info", Message: "NAT type: NAT1 (Open Internet)"})
		return
	}

	// === Mapping Behavior Test ===
	// Query 2+ STUN servers from the SAME socket.
	// Same mapped port → Cone NAT (NAT1). Different mapped port → Symmetric (NAT4).
	testConn, err := bypassListenUDP()
	if err != nil {
		c.natType = NATFullCone
		c.emit(EventLog, LogEvent{Level: "info", Message: "NAT type: NAT1 (default, socket failed)"})
		return
	}
	defer testConn.Close()

	type stunResult struct {
		server string
		port   string
	}
	var results []stunResult
	seenIPs := map[string]bool{}

	for _, srv := range servers {
		srv = strings.TrimSpace(srv)
		if srv == "" {
			continue
		}
		resolved, err := net.ResolveUDPAddr("udp4", srv)
		if err != nil {
			continue
		}
		ipKey := resolved.IP.String()
		if seenIPs[ipKey] {
			continue
		}
		addr := stunQueryFresh(testConn, srv)
		if addr == "" {
			continue
		}
		seenIPs[ipKey] = true
		_, port, _ := net.SplitHostPort(addr)
		results = append(results, stunResult{server: srv, port: port})
		if len(results) >= 2 {
			break
		}
	}

	if len(results) < 2 {
		c.natType = NATFullCone
		c.emit(EventLog, LogEvent{Level: "info", Message: "NAT type: NAT1 (default, only 1 STUN server reachable)"})
		return
	}

	if results[0].port == results[1].port {
		c.natType = NATFullCone
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
			"NAT type: NAT1 (Cone NAT, port consistent: %s via %s and %s)",
			results[0].port, results[0].server, results[1].server)})
	} else {
		c.natType = NATSymmetric
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
			"NAT type: NAT4 (Symmetric, port varies: %s vs %s)",
			results[0].port, results[1].port)})
		c.detectPortAllocation(servers)
	}
}

// hasLocalIP checks if the given IP matches any local network interface.
func hasLocalIP(ip string) bool {
	target := net.ParseIP(ip)
	if target == nil {
		return false
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ifIP net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ifIP = v.IP
			case *net.IPAddr:
				ifIP = v.IP
			}
			if ifIP != nil && ifIP.Equal(target) {
				return true
			}
		}
	}
	return false
}

// NAT port allocation model (from p2p_punch.py 6-model classifier)
type portModel struct {
	Name      string  // global_counter, pool_pairs, sequential, random
	DriftRate float64 // ports/second for global_counter
	Delta     int     // step size for sequential
	Anchor    int     // last observed port
	AnchorAt  time.Time
}

// detectPortAllocation probes the NAT to classify port allocation behavior.
// Uses 8 samples for better accuracy (inspired by p2p_punch.py model classifier).
func (c *Client) detectPortAllocation(servers []string) {
	if len(servers) == 0 {
		return
	}
	bestSrv := servers[0]

	type sample struct {
		port int
		at   time.Time
	}
	var samples []sample

	for i := 0; i < 8; i++ {
		conn, err := bypassListenUDP()
		if err != nil {
			continue
		}
		addr := stunQueryFresh(conn, bestSrv)
		conn.Close()
		if addr != "" {
			_, portStr, _ := net.SplitHostPort(addr)
			p := 0
			fmt.Sscanf(portStr, "%d", &p)
			if p > 0 {
				samples = append(samples, sample{port: p, at: time.Now()})
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(samples) < 3 {
		return
	}

	// Calculate sequential deltas
	deltas := make([]int, 0, len(samples)-1)
	for i := 1; i < len(samples); i++ {
		d := samples[i].port - samples[i-1].port
		if d < -30000 {
			d += 65536 // port wraparound
		}
		deltas = append(deltas, d)
	}

	// Count negative deltas
	negCount := 0
	for _, d := range deltas {
		if d < 0 {
			negCount++
		}
	}

	// Calculate drift rate (ports/second)
	elapsed := samples[len(samples)-1].at.Sub(samples[0].at).Seconds()
	totalDelta := samples[len(samples)-1].port - samples[0].port
	if totalDelta < -30000 {
		totalDelta += 65536
	}
	driftRate := float64(0)
	if elapsed > 0 {
		driftRate = float64(totalDelta) / elapsed
	}

	// Classify model
	model := portModel{
		Anchor:   samples[len(samples)-1].port,
		AnchorAt: samples[len(samples)-1].at,
	}

	mostlyPositive := negCount <= len(deltas)/5

	if mostlyPositive && driftRate > 10 {
		// Global counter (CGNAT): high drift rate, mostly positive
		model.Name = "global_counter"
		model.DriftRate = driftRate
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
			"NAT4 model: global_counter (drift=%.0f ports/sec) — predictable", driftRate)})
	} else if mostlyPositive && len(deltas) >= 2 {
		// Check for sequential (small consistent deltas)
		median := sortedMedian(deltas)
		maxDev := 0
		for _, d := range deltas {
			dev := d - median
			if dev < 0 {
				dev = -dev
			}
			if dev > maxDev {
				maxDev = dev
			}
		}
		if median >= 1 && median <= 10 && maxDev <= 3 {
			model.Name = "sequential"
			model.Delta = median
			c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
				"NAT4 model: sequential (delta=%d) — predictable", median)})
		} else {
			model.Name = "random"
			c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
				"NAT4 model: random (deltas vary widely) — birthday attack only")})
		}
	} else {
		model.Name = "random"
		c.emit(EventLog, LogEvent{Level: "info", Message: "NAT4 model: random — birthday attack only"})
	}

	// Store model for use in attemptHolePunch
	c.peerConnsMu.Lock()
	c.portModel = &model
	c.peerConnsMu.Unlock()
}

func sortedMedian(vals []int) int {
	sorted := make([]int, len(vals))
	copy(sorted, vals)
	sort.Ints(sorted)
	return sorted[len(sorted)/2]
}

// stunQueryFresh sends a STUN binding request to a server from the given conn.
func stunQueryFresh(conn *net.UDPConn, server string) string {
	serverAddr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return ""
	}

	req := make([]byte, StunHeaderSize)
	binary.BigEndian.PutUint16(req[0:2], StunBindingRequest)
	binary.BigEndian.PutUint16(req[2:4], 0)
	binary.BigEndian.PutUint32(req[4:8], StunMagicCookie)
	txID := make([]byte, 12)
	rand.Read(txID)
	copy(req[8:20], txID)

	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.WriteToUDP(req, serverAddr); err != nil {
		return ""
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1024)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil || n < StunHeaderSize {
		return ""
	}
	conn.SetReadDeadline(time.Time{})
	conn.SetWriteDeadline(time.Time{})

	resp := buf[:n]
	if binary.BigEndian.Uint16(resp[0:2]) != 0x0101 || !bytes.Equal(resp[8:20], txID) {
		return ""
	}
	msgLen := binary.BigEndian.Uint16(resp[2:4])
	attrs := resp[StunHeaderSize : StunHeaderSize+int(msgLen)]
	addr, err := parseXorMappedAddress(attrs)
	if err != nil {
		return ""
	}
	return addr
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
	info := map[string]interface{}{
		"addr":  c.publicAddr,
		"local": localUDP,
	}
	if c.natType != "" {
		info["nat_type"] = c.natType
	}
	payload, _ := json.Marshal(info)
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
		Addr    string `json:"addr"`
		Local   string `json:"local"`
		NATType string `json:"nat_type"`
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

	// Store peer's NAT type
	if info.NATType != "" {
		pc.NATType = info.NATType
	}

	// If peer is already direct AND the address hasn't changed, skip.
	// But if the address changed (peer reconnected with new STUN endpoint),
	// we need to re-punch even if mode was "direct".
	if pc.Mode == "direct" && pc.UDPAddr != nil && pc.UDPAddr.String() == udpAddr.String() {
		c.peerConnsMu.Unlock()
		return // same addr, already connected — no action needed
	}

	addrChanged := pc.UDPAddr != nil && pc.UDPAddr.String() != udpAddr.String()
	if addrChanged && pc.Mode == "direct" {
		// Peer reconnected with new address — reset to connecting so we re-punch
		pc.Mode = "connecting"
		pc.Crypto = nil
		peerID := msg.From
		c.peerConnsMu.Unlock()

		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
			"Peer %s address changed, resetting P2P state", shortID(peerID))})

		// Cancel active speed tests with this peer
		c.cancelSpeedTestsForPeer(peerID)

		// Close stale forward netstack (will be recreated on demand)
		c.fwdNetstacksMu.Lock()
		if fn, ok := c.fwdNetstacks[peerID]; ok {
			delete(c.fwdNetstacks, peerID)
			go fn.Close()
		}
		c.fwdNetstacksMu.Unlock()

		c.peerConnsMu.Lock()
	}

	pc.UDPAddr = udpAddr
	c.peerConnsMu.Unlock()

	if isLAN {
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("LAN peer detected: %s → using local address %s", shortID(msg.From), targetAddr)})
	} else if c.verbose {
		natStr := ""
		if info.NATType != "" {
			natStr = " [" + info.NATType + "]"
		}
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Received STUN endpoint from %s: %s%s", shortID(msg.From), info.Addr, natStr)})
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
	peerNAT := pc.NATType
	c.peerConnsMu.Unlock()

	addr := pc.UDPAddr
	myID := []byte(c.MyID)
	myNAT := c.natType

	// ──────────────────────────────────────────────────────────
	// Adaptive hole punch strategy (integrated from p2p_punch.py)
	//
	// Phase 1: Direct burst from main socket (all NAT types)
	// Phase 2: Birthday Attack with progressive sending (NAT4)
	// Phase 3: Drift-rate port prediction (global_counter NAT4)
	// Phase 4: Random port spray — Cone side only (Cone+NAT4)
	// ──────────────────────────────────────────────────────────
	anyHard := peerNAT == NATSymmetric || myNAT == NATSymmetric
	iAmCone := myNAT != NATSymmetric && peerNAT == NATSymmetric

	punch := append([]byte("PUNCH:"), myID...)
	burstCount := 20
	if anyHard {
		burstCount = 40
	}

	// Phase 1: Rapid burst from main socket
	for i := 0; i < burstCount; i++ {
		select {
		case <-c.done:
			return
		default:
		}
		udp.WriteToUDP(punch, addr)
		time.Sleep(25 * time.Millisecond)
	}

	if isLAN {
		return
	}

	if !anyHard {
		return // Cone+Cone: Phase 1 is enough
	}

	// Phase 2: Birthday Attack (multi-socket parallel punch)
	birthdaySockets := 256
	var extraConns []*net.UDPConn
	for i := 0; i < birthdaySockets; i++ {
		conn, err := bypassListenUDP()
		if err != nil {
			continue
		}
		extraConns = append(extraConns, conn)
	}

	if len(extraConns) > 0 {
		// Progressive: 5 rounds, all sockets send to known addr
		for round := 0; round < 5; round++ {
			select {
			case <-c.done:
				break
			default:
			}
			for _, conn := range extraConns {
				conn.WriteToUDP(punch, addr)
			}
			time.Sleep(50 * time.Millisecond)
		}
		// Keep sockets for Phase 4 if Cone side
		if !iAmCone {
			for _, conn := range extraConns {
				conn.Close()
			}
			extraConns = nil
		}
	}

	// Phase 3: Port prediction based on NAT model
	c.peerConnsMu.RLock()
	model := c.portModel
	c.peerConnsMu.RUnlock()

	if model != nil && model.Name == "global_counter" && model.DriftRate > 0 {
		// Drift-rate prediction: estimate where the NAT counter is NOW
		elapsed := time.Since(model.AnchorAt).Seconds()
		predicted := model.Anchor + int(model.DriftRate*elapsed)
		// Uncertainty grows with time + self-consumption from birthday sockets
		uncertainty := int(model.DriftRate*elapsed*0.5) + len(extraConns)*3
		if uncertainty < 20 {
			uncertainty = 20
		}
		if uncertainty > 2000 {
			uncertainty = 2000
		}
		backward := int(model.DriftRate * elapsed * 0.15)
		if backward < 10 {
			backward = 10
		}

		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
			"Port prediction: anchor=%d drift=%.0f/s elapsed=%.1fs predicted=%d ±%d",
			model.Anchor, model.DriftRate, elapsed, predicted, uncertainty)})

		for delta := -backward; delta <= uncertainty; delta++ {
			p := ((predicted + delta - 1024) % 64512) + 1024 // wraparound to 1024-65535
			udp.WriteToUDP(punch, &net.UDPAddr{IP: addr.IP, Port: p})
		}
	} else if model != nil && model.Name == "sequential" && model.Delta > 0 {
		// Sequential prediction: simple delta extrapolation
		basePort := addr.Port
		step := model.Delta
		for i := -10; i <= 50; i++ {
			p := basePort + i*step
			if p <= 1024 || p > 65534 {
				continue
			}
			udp.WriteToUDP(punch, &net.UDPAddr{IP: addr.IP, Port: p})
		}
	} else {
		// Unknown model: scan ±1000 around known port
		basePort := addr.Port
		for delta := -1000; delta <= 1000; delta++ {
			if delta == 0 {
				continue
			}
			p := basePort + delta
			if p <= 1024 || p > 65534 {
				continue
			}
			udp.WriteToUDP(punch, &net.UDPAddr{IP: addr.IP, Port: p})
		}
	}

	// Phase 4: Random port spray (Cone side of Cone+NAT4 pair)
	// Use the birthday sockets to spray random ports on the Symmetric peer's IP.
	// With 256 sockets open and ~1000 random probes, collision probability ≈98%.
	if iAmCone && len(extraConns) > 0 {
		sprayCount := 1024
		for i := 0; i < sprayCount; i++ {
			select {
			case <-c.done:
				break
			default:
			}
			rPort := mrand.Intn(64512) + 1024
			for _, conn := range extraConns {
				conn.WriteToUDP(punch, &net.UDPAddr{IP: addr.IP, Port: rPort})
			}
			if i%50 == 0 {
				time.Sleep(time.Millisecond) // yield to avoid flooding
			}
		}
	}

	// Cleanup birthday sockets
	for _, conn := range extraConns {
		conn.Close()
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
	// A fresh punch is a strong liveness signal — let the data plane use the direct
	// path immediately. The health controller (pathswitch.go) maintains it from here.
	pc.directHealthy = true
	pc.lastSwitch = time.Now()
	markDirectRecvNano(pc)

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

	// Broadcast updated P2P connectivity map so peers can discover auto-hop routes
	go c.broadcastP2PMap()
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

	if pc.Crypto.IsEncrypted() {
		c.peerConnsMu.Unlock()
		return // already established
	}

	if err := pc.Crypto.DeriveKey(peerPubKey); err != nil {
		c.peerConnsMu.Unlock()
		c.emit(EventLog, LogEvent{Level: "error", Message: fmt.Sprintf("Key exchange failed with %s: %v", shortID(peerID), err)})
		return
	}
	c.peerConnsMu.Unlock()

	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Encrypted channel established with %s (X25519+XChaCha20-Poly1305)", shortID(peerID))})

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

// dispatchSMMessage routes SM: signal messages to the appropriate handler.
func (c *Client) dispatchSMMessage(msg Message) {
	switch msg.Type {
	case "st_begin":
		c.handleSTBegin(msg)
	case "st_ready":
		c.handleSTReady(msg)
	case "st_finish":
		c.handleSTFinish(msg)
	case "st_result":
		c.handleSTResult(msg)
	case "st_cancel":
		c.handleSTCancel(msg)
	case "file_offer":
		c.handleFileOffer(msg)
	case "file_accept":
		c.handleFileAccept(msg)
	case "file_done":
		c.handleFileDone(msg)
	case "file_reject":
		c.handleFileReject(msg)
	case "file_cancel":
		c.handleFileCancel(msg)
	case "file_nack":
		c.handleFileNack(msg)
	case "file_stream":
		c.handleFileStream(msg)
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

		// Health probe with RTT (see health.go)
		if bytes.HasPrefix(data, prefixPing2) {
			c.handlePing2(addr, data[len(prefixPing2):])
			continue
		}
		if bytes.HasPrefix(data, prefixPong2) {
			c.handlePong2(addr, data[len(prefixPong2):])
			continue
		}

		// Keepalive PING — just ignore (keeps NAT mapping alive)
		if n == 4 && string(data) == "PING" {
			continue
		}

		// Forward netstack data over UDP: "FN:" + compressed IP packet
		if n > 3 && string(data[:3]) == "FN:" {
			compressed := data[3:]
			raw, err := Decompress(compressed)
			if err != nil {
				continue
			}
			c.handleFwdNetstackPacket(raw, addr)
			continue
		}

		// VPN data over UDP: "VPN:" + compressed IP packet
		if n > 4 && string(data[:4]) == "VPN:" {
			compressed := data[4:]
			raw, err := Decompress(compressed)
			if err != nil {
				continue
			}
			// Find peer by UDP address
			vpnPeerID := ""
			c.peerConnsMu.RLock()
			for id, pc := range c.peerConns {
				if pc.UDPAddr != nil && pc.UDPAddr.String() == addr.String() {
					vpnPeerID = id
					break
				}
			}
			c.peerConnsMu.RUnlock()
			c.handleTunDataDirect(raw, vpnPeerID)
			continue
		}

		// SpeedTest data over UDP: "ST:" + compressed JSON
		if n > 3 && string(data[:3]) == "ST:" {
			compressed := data[3:]
			payload, err := Decompress(compressed)
			if err != nil {
				continue
			}
			var stData SpeedTestData
			if err := json.Unmarshal(payload, &stData); err == nil {
				c.processSpeedTestData(stData)
			}
			continue
		}

		// File data over UDP: "SF:" + compressed JSON
		if n > 3 && string(data[:3]) == "SF:" {
			compressed := data[3:]
			payload, err := Decompress(compressed)
			if err != nil {
				continue
			}
			var fileData FileData
			if err := json.Unmarshal(payload, &fileData); err == nil {
				c.processFileDataP2P(fileData)
			}
			continue
		}

		// P2P signal message: "SM:<type>:<json>"
		if n > 3 && string(data[:3]) == "SM:" {
			rest := data[3:]
			// Find the colon separating type from payload
			colonIdx := -1
			for i, b := range rest {
				if b == ':' {
					colonIdx = i
					break
				}
			}
			if colonIdx > 0 && colonIdx < len(rest)-1 {
				msgType := string(rest[:colonIdx])
				payload := rest[colonIdx+1:]
				peerID := c.findPeerByUDPAddr(addr)
				if peerID != "" {
					inner := Message{
						Type:    msgType,
						From:    peerID,
						Payload: json.RawMessage(payload),
					}
					switch msgType {
					case "st_begin":
						c.handleSTBegin(inner)
					case "st_ready":
						c.handleSTReady(inner)
					case "st_finish":
						c.handleSTFinish(inner)
					case "st_result":
						c.handleSTResult(inner)
					default:
						c.dispatchSMMessage(inner)
					}
				}
			}
			continue
		}

		// Tunnel forward data over UDP with RUTP: "TF:" + [8B tunnelID] + [RUTP frame]
		if n > 11 && string(data[:3]) == "TF:" {
			tunnelID := tunnelIDFromBytes(data[3:11])
			rutpFrame := data[11:]

			typ, seq, payload, ok := rutpParseFrame(rutpFrame)
			if !ok {
				continue
			}

			if typ == rutpACK {
				// ACK for data we sent — find sender and notify
				// Senders track their own ACKs via the pending map
				// We need to find the right sender... store per-tunnel
				c.tunnelsMu.RLock()
				tc, exists := c.tunnels[tunnelID]
				c.tunnelsMu.RUnlock()
				if exists && tc.RutpSender != nil {
					tc.RutpSender.OnACK(seq)
				}
				continue
			}

			if typ == rutpDATA {
				c.tunnelsMu.RLock()
				tc, exists := c.tunnels[tunnelID]
				c.tunnelsMu.RUnlock()
				if !exists {
					continue
				}

				// Initialize receiver if needed
				if tc.RutpRecv == nil {
					c.connMu.Lock()
					udp := c.udpConn
					c.connMu.Unlock()
					if udp != nil {
						prefix := make([]byte, 11)
						copy(prefix[:3], []byte("TF:"))
						copy(prefix[3:], data[3:11])
						tc.RutpRecv = newRutpReceiver(udp, addr, prefix)
						if tc.RelayDedup == nil {
							tc.RelayDedup = &sync.Map{}
						}
					}
				}

				if tc.RutpRecv != nil {
					deduped := tc.RutpRecv.OnData(seq, payload)
					if deduped != nil {
						raw, derr := Decompress(deduped)
						if derr != nil {
							raw = deduped
						}
						// Mark in RelayDedup so relay fallback won't duplicate
						if tc.RelayDedup != nil {
							tc.RelayDedup.Store(simpleHash(raw), true)
						}
						if tc.Forward != nil {
							atomic.AddInt64(&tc.Forward.BytesDown, int64(len(raw)))
						}
						tc.Conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
						tc.Conn.Write(raw)
					}
				}
				continue
			}
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

	if crypto != nil && crypto.IsEncrypted() {
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

// startDirectTCPListener listens for incoming direct TCP connections from peers.
func (c *Client) findPeerByUDPAddr(addr *net.UDPAddr) string {
	c.peerConnsMu.RLock()
	defer c.peerConnsMu.RUnlock()
	for id, pc := range c.peerConns {
		if pc.UDPAddr != nil && pc.UDPAddr.IP.Equal(addr.IP) && pc.UDPAddr.Port == addr.Port {
			return id
		}
	}
	return ""
}

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
	keepaliveTicker := time.NewTicker(5 * time.Second) // PING2 = NAT keepalive + RTT/liveness probe
	defer retryTicker.Stop()
	defer keepaliveTicker.Stop()

	for {
		select {
		case <-c.done:
			return

		case <-keepaliveTicker.C:
			// Collect direct-peer addresses, then probe outside the lock.
			c.peerConnsMu.RLock()
			pingAddrs := make([]*net.UDPAddr, 0, len(c.peerConns))
			for _, pc := range c.peerConns {
				if pc.Mode == "direct" && pc.UDPAddr != nil {
					pingAddrs = append(pingAddrs, pc.UDPAddr)
				}
			}
			c.peerConnsMu.RUnlock()
			// PING2 doubles as NAT keepalive and RTT/liveness probe (health.go).
			for _, addr := range pingAddrs {
				c.sendDirectPing(addr)
			}
			// Re-evaluate transports and migrate live tunnels if a path changed.
			c.refreshPeerTransports()

		case <-retryTicker.C:
			// Periodically broadcast P2P map for auto-hop discovery
			go c.broadcastP2PMap()

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
					pc.directHealthy = false
					c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf("P2P punch failed 5 times for %s, using relay", shortID(peerID))})
					// Try to discover auto-hop route
					if pc.AutoHopVia == "" {
						go c.tryAutoHop(peerID)
					}
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

// directTCPReadLoop reads framed tunnel data from a direct TCP connection.
// Frame format: [8-byte tunnelID][4-byte length][compressed data]
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

		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}

		tunnelID := tunnelIDFromBytes(header[:8])
		dataLen := int(header[8])<<24 | int(header[9])<<16 | int(header[10])<<8 | int(header[11])
		if dataLen <= 0 || dataLen > 512*1024 {
			return
		}

		compressed := make([]byte, dataLen)
		if _, err := io.ReadFull(conn, compressed); err != nil {
			return
		}

		// Decompress
		data, err := Decompress(compressed)
		if err != nil {
			data = compressed // fallback: treat as raw
		}

		c.tunnelsMu.RLock()
		tc, ok := c.tunnels[tunnelID]
		c.tunnelsMu.RUnlock()
		if !ok {
			continue
		}

		if tc.Forward != nil {
			atomic.AddInt64(&tc.Forward.BytesDown, int64(len(data)))
		}
		tc.Conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		tc.Conn.Write(data)
	}
}

