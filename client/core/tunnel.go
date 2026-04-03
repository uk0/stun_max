package core

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Buffer pool for relay mode
var relayBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 64*1024)
		return &b
	},
}

// StartForward creates a local TCP listener that tunnels to a remote peer.
func (c *Client) StartForward(peerID, host string, remotePort, localPort int) error {
	fullID, err := c.resolvePeerID(peerID)
	if err != nil {
		return err
	}
	c.forwardsMu.RLock()
	if _, exists := c.forwards[localPort]; exists {
		c.forwardsMu.RUnlock()
		return fmt.Errorf("local port %d already in use", localPort)
	}
	c.forwardsMu.RUnlock()

	// Check if port is available on the system before binding
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", localPort))
	if err != nil {
		return fmt.Errorf("port %d unavailable (already in use by another program)", localPort)
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

	fwd := &Forward{
		LocalPort: localPort, RemoteHost: host, RemotePort: remotePort,
		PeerID: fullID, PeerName: peerName,
		Listener: listener, Cancel: make(chan struct{}),
	}
	c.forwardsMu.Lock()
	c.forwards[localPort] = fwd
	c.forwardsMu.Unlock()

	mode := c.getForwardMode(fullID)
	c.emit(EventForwardStarted, ForwardEvent{
		LocalPort: localPort, RemoteHost: host, RemotePort: remotePort,
		PeerName: peerName, Mode: mode,
	})
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Forwarding :%d -> %s:%d via %s [%s]", localPort, host, remotePort, peerName, mode)})

	c.wg.Add(1)
	go c.acceptLoop(fwd)
	return nil
}

func (c *Client) getForwardMode(peerID string) string {
	c.peerConnsMu.RLock()
	defer c.peerConnsMu.RUnlock()
	if pc, ok := c.peerConns[peerID]; ok && pc.Mode == "direct" {
		return "P2P"
	}
	return "RELAY"
}

// virtualPortCounter allocates unique virtual ports for forward netstack connections.
var virtualPortCounter uint32 = 10000

func nextVirtualPort() uint16 {
	return uint16(atomic.AddUint32(&virtualPortCounter, 1))
}

func (c *Client) acceptLoop(fwd *Forward) {
	defer c.wg.Done()
	defer fwd.Listener.Close()
	for {
		select {
		case <-fwd.Cancel:
			return
		case <-c.done:
			return
		default:
		}
		fwd.Listener.(*net.TCPListener).SetDeadline(time.Now().Add(1 * time.Second))
		conn, err := fwd.Listener.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			select {
			case <-fwd.Cancel:
			case <-c.done:
			default:
			}
			return
		}
		optimizeTCP(conn)

		// Get or create gVisor forward netstack for this peer
		fn, err := c.getOrCreateFwdNetstack(fwd.PeerID, true)
		if err != nil {
			c.emit(EventLog, LogEvent{Level: "error", Message: "forward netstack: " + err.Error()})
			conn.Close()
			continue
		}

		vport := nextVirtualPort()
		target := net.JoinHostPort(fwd.RemoteHost, strconv.Itoa(fwd.RemotePort))
		tunnelID := fmt.Sprintf("ns:%d", vport)

		// Create a channel to wait for B's confirmation
		readyCh := make(chan struct{}, 1)
		c.tunnelsMu.Lock()
		c.tunnels[tunnelID] = &TunnelConn{
			TunnelID: tunnelID, PeerID: fwd.PeerID,
			Done: readyCh, // reuse Done channel as ready signal
		}
		c.tunnelsMu.Unlock()

		// Tell B to register this virtual port → real target
		if err := c.sendRelay(fwd.PeerID, "open_tunnel", TunnelOpen{
			TunnelID:   tunnelID,
			TargetHost: fwd.RemoteHost,
			TargetPort: fwd.RemotePort,
		}); err != nil {
			conn.Close()
			c.tunnelsMu.Lock()
			delete(c.tunnels, tunnelID)
			c.tunnelsMu.Unlock()
			continue
		}

		fwd.Mu.Lock()
		fwd.ConnCount++
		fwd.Mu.Unlock()

		go c.bridgeForwardConn(conn, fn, vport, fwd, target, readyCh)
	}
}

// bridgeForwardConn bridges a local TCP connection through gVisor to the peer.
func (c *Client) bridgeForwardConn(local net.Conn, fn *forwardNetstack, vport uint16, fwd *Forward, target string, readyCh chan struct{}) {
	defer local.Close()
	defer func() {
		fwd.Mu.Lock()
		fwd.ConnCount--
		fwd.Mu.Unlock()
	}()

	// Wait for B to confirm port registration (tunnel_opened response)
	select {
	case <-readyCh:
		// B is ready
	case <-time.After(10 * time.Second):
		c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf(
			"forward %s: B did not confirm port %d in time", target, vport)})
		return
	case <-c.done:
		return
	}

	// Dial through gVisor to B's virtual IP:vport
	virtual, err := fn.DialTCP(vport)
	if err != nil {
		c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf(
			"forward dial %s via netstack failed: %v", target, err)})
		return
	}
	defer virtual.Close()

	// Bidirectional bridge — gVisor handles TCP reliability
	errc := make(chan error, 2)
	go func() {
		buf := make([]byte, 64*1024)
		n, err := io.CopyBuffer(virtual, local, buf)
		atomic.AddInt64(&fwd.BytesUp, n)
		errc <- err
	}()
	go func() {
		buf := make([]byte, 64*1024)
		n, err := io.CopyBuffer(local, virtual, buf)
		atomic.AddInt64(&fwd.BytesDown, n)
		errc <- err
	}()
	<-errc
}

func optimizeTCP(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetReadBuffer(512 * 1024)
		tc.SetWriteBuffer(512 * 1024)
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}
}

func (c *Client) StopForward(localPort int) error {
	c.forwardsMu.Lock()
	fwd, ok := c.forwards[localPort]
	if !ok {
		c.forwardsMu.Unlock()
		return fmt.Errorf("no forward on port %d", localPort)
	}
	delete(c.forwards, localPort)
	c.forwardsMu.Unlock()

	close(fwd.Cancel)
	fwd.Listener.Close()

	c.tunnelsMu.Lock()
	for id, tc := range c.tunnels {
		if tc.Forward == fwd {
			tc.Conn.Close()
			select {
			case <-tc.Done:
			default:
				close(tc.Done)
			}
			delete(c.tunnels, id)
		}
	}
	c.tunnelsMu.Unlock()

	c.emit(EventForwardStopped, ForwardEvent{LocalPort: localPort})
	return nil
}

func (c *Client) handleOpenTunnel(msg Message) {
	var req TunnelOpen
	if err := json.Unmarshal(msg.Payload, &req); err != nil {
		return
	}
	c.acMu.RLock()
	allowFwd := c.allowForward
	onlyLocal := c.localOnly
	c.acMu.RUnlock()

	if !allowFwd {
		c.sendRelay(msg.From, "tunnel_rejected", TunnelRejected{TunnelID: req.TunnelID, Reason: "forwarding disabled"})
		return
	}
	if onlyLocal && req.TargetHost != "127.0.0.1" && req.TargetHost != "localhost" && req.TargetHost != "::1" {
		c.sendRelay(msg.From, "tunnel_rejected", TunnelRejected{TunnelID: req.TunnelID, Reason: "local-only"})
		return
	}

	// Check if this is a netstack-based forward (TunnelID starts with "ns:")
	if strings.HasPrefix(req.TunnelID, "ns:") {
		vportStr := strings.TrimPrefix(req.TunnelID, "ns:")
		vport, err := strconv.Atoi(vportStr)
		if err != nil {
			return
		}

		fn, err := c.getOrCreateFwdNetstack(msg.From, false)
		if err != nil {
			c.emit(EventLog, LogEvent{Level: "error", Message: "forward netstack B: " + err.Error()})
			return
		}

		target := net.JoinHostPort(req.TargetHost, strconv.Itoa(req.TargetPort))
		fn.RegisterTarget(uint16(vport), target)

		c.sendRelay(msg.From, "tunnel_opened", TunnelClose{TunnelID: req.TunnelID})
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
			"Forward netstack: registered port %d → %s", vport, target)})
		return
	}

	// Legacy tunnel path (non-netstack)
	target := net.JoinHostPort(req.TargetHost, strconv.Itoa(req.TargetPort))
	conn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		c.sendTunnelClose(msg.From, req.TunnelID)
		return
	}
	optimizeTCP(conn)

	tc := &TunnelConn{
		TunnelID: req.TunnelID, PeerID: msg.From,
		Conn: conn, Done: make(chan struct{}),
	}
	c.tunnelsMu.Lock()
	c.tunnels[req.TunnelID] = tc
	c.tunnelsMu.Unlock()

	c.sendRelay(msg.From, "tunnel_opened", TunnelClose{TunnelID: req.TunnelID})

	c.wg.Add(1)
	go c.tunnelReadLoop(tc, msg.From)
}

// tunnelReadLoop reads TCP and sends to peer.
// Transport: Direct TCP (P2P) if available, WebSocket relay otherwise.
func (c *Client) tunnelReadLoop(tc *TunnelConn, peerID string) {
	defer c.wg.Done()
	defer func() {
		tc.Conn.Close()
		c.tunnelsMu.Lock()
		delete(c.tunnels, tc.TunnelID)
		c.tunnelsMu.Unlock()
		c.sendTunnelClose(peerID, tc.TunnelID)
	}()

	idBytes := tunnelIDToBytes(tc.TunnelID)

	// Try P2P UDP with reliable transport (RUTP)
	c.peerConnsMu.RLock()
	pc := c.peerConns[peerID]
	hasP2P := pc != nil && pc.Mode == "direct" && pc.UDPAddr != nil
	var directConn net.Conn
	if pc != nil {
		directConn = pc.DirectTCP
	}
	c.peerConnsMu.RUnlock()

	if hasP2P {
		c.tunnelReadUDP(tc, peerID, idBytes)
		return
	}

	if directConn != nil {
		c.tunnelReadDirect(tc, peerID, directConn, idBytes)
		return
	}

	c.tunnelReadRelay(tc, peerID)
}

// tunnelReadDirect: high-performance direct TCP path.
// Single write per frame (header+data in one buffer), with compression.
func (c *Client) tunnelReadDirect(tc *TunnelConn, peerID string, directConn net.Conn, idBytes []byte) {
	readBuf := make([]byte, 64*1024)

	for {
		select {
		case <-tc.Done:
			return
		case <-c.done:
			return
		default:
		}

		tc.Conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		n, err := tc.Conn.Read(readBuf)
		if n > 0 {
			if tc.Forward != nil {
				atomic.AddInt64(&tc.Forward.BytesUp, int64(n))
			}

			// Compress
			compressed := Compress(readBuf[:n])

			// Build frame: [8-byte tunnelID][4-byte length][compressed data]
			frameLen := 8 + 4 + len(compressed)
			frame := make([]byte, frameLen)
			copy(frame[:8], idBytes)
			clen := len(compressed)
			frame[8] = byte(clen >> 24)
			frame[9] = byte(clen >> 16)
			frame[10] = byte(clen >> 8)
			frame[11] = byte(clen)
			copy(frame[12:], compressed)

			directConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, werr := directConn.Write(frame)
			if werr != nil {
				c.peerConnsMu.Lock()
				if pc, ok := c.peerConns[peerID]; ok {
					pc.DirectTCP = nil
				}
				c.peerConnsMu.Unlock()
				directConn.Close()
				c.tunnelReadRelay(tc, peerID)
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// tunnelReadUDP: P2P UDP path with reliable transport (RUTP).
// Uses RUTP framing (magic + seq + checksum) to ensure no data loss.
func (c *Client) tunnelReadUDP(tc *TunnelConn, peerID string, idBytes []byte) {
	c.peerConnsMu.RLock()
	pc := c.peerConns[peerID]
	c.peerConnsMu.RUnlock()
	if pc == nil || pc.UDPAddr == nil {
		c.tunnelReadRelay(tc, peerID)
		return
	}

	c.connMu.Lock()
	udp := c.udpConn
	c.connMu.Unlock()
	if udp == nil {
		c.tunnelReadRelay(tc, peerID)
		return
	}

	// Build prefix: "TF:" + tunnelID (11 bytes)
	prefix := make([]byte, 3+8)
	copy(prefix[:3], []byte("TF:"))
	copy(prefix[3:], idBytes)

	sender := newRutpSender(udp, pc.UDPAddr, prefix, c, peerID, tc.TunnelID)
	tc.RutpSender = sender
	tc.RelayDedup = &sync.Map{} // dedup relay fallback data
	defer sender.Stop()

	buf := make([]byte, 32*1024)
	for {
		select {
		case <-tc.Done:
			return
		case <-c.done:
			return
		default:
		}

		tc.Conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		n, err := tc.Conn.Read(buf)
		if n > 0 {
			if tc.Forward != nil {
				atomic.AddInt64(&tc.Forward.BytesUp, int64(n))
			}
			compressed := Compress(buf[:n])
			sender.Send(compressed)
		}
		if err != nil {
			return
		}
	}
}

// tunnelReadRelay: WebSocket relay path (always works).
func (c *Client) tunnelReadRelay(tc *TunnelConn, peerID string) {
	bufPtr := relayBufPool.Get().(*[]byte)
	defer relayBufPool.Put(bufPtr)
	buf := *bufPtr

	for {
		select {
		case <-tc.Done:
			return
		case <-c.done:
			return
		default:
		}

		tc.Conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		n, err := tc.Conn.Read(buf)
		if n > 0 {
			if tc.Forward != nil {
				atomic.AddInt64(&tc.Forward.BytesUp, int64(n))
			}
			compressed := Compress(buf[:n])
			encoded := base64.StdEncoding.EncodeToString(compressed)
			c.sendRelay(peerID, "tunnel_data", TunnelData{TunnelID: tc.TunnelID, Data: encoded})
		}
		if err != nil {
			return
		}
	}
}

func (c *Client) handleTunnelOpened(msg Message) {
	var info TunnelClose
	if err := json.Unmarshal(msg.Payload, &info); err != nil {
		return
	}

	// For netstack-based forwards, signal the ready channel
	if strings.HasPrefix(info.TunnelID, "ns:") {
		c.tunnelsMu.Lock()
		tc, ok := c.tunnels[info.TunnelID]
		if ok {
			select {
			case tc.Done <- struct{}{}:
			default:
			}
			delete(c.tunnels, info.TunnelID)
		}
		c.tunnelsMu.Unlock()
		return
	}

	// Legacy tunnel path
	c.tunnelsMu.RLock()
	tc, ok := c.tunnels[info.TunnelID]
	c.tunnelsMu.RUnlock()
	if !ok {
		return
	}
	c.wg.Add(1)
	go c.tunnelReadLoop(tc, msg.From)
}

func (c *Client) handleTunnelData(msg Message) {
	var td TunnelData
	if err := json.Unmarshal(msg.Payload, &td); err != nil {
		return
	}

	// Check if this tunnel is part of a hop bridge (B's relay role).
	// If so, re-relay the data to the other side without decompress/recompress.
	c.hopsMu.RLock()
	bridge, isHop := c.hopBridgeByTunnel[td.TunnelID]
	c.hopsMu.RUnlock()

	if isHop {
		var targetPeer, targetTunnel string
		if td.TunnelID == bridge.InboundTunnelID {
			// Data from A → forward to C
			targetPeer = bridge.TargetPeerID
			targetTunnel = bridge.OutboundTunnelID
		} else {
			// Data from C → forward to A
			targetPeer = bridge.OriginPeerID
			targetTunnel = bridge.InboundTunnelID
		}
		// Re-relay as-is (data is already base64-encoded compressed payload)
		c.sendRelay(targetPeer, "tunnel_data", TunnelData{
			TunnelID: targetTunnel,
			Data:     td.Data,
		})
		return
	}

	c.tunnelsMu.RLock()
	tc, ok := c.tunnels[td.TunnelID]
	c.tunnelsMu.RUnlock()
	if !ok {
		return
	}

	compressed, err := base64.StdEncoding.DecodeString(td.Data)
	if err != nil {
		return
	}

	data, err := Decompress(compressed)
	if err != nil {
		data = compressed
	}

	// Dedup: if this data was already received via UDP P2P, skip
	if tc.RelayDedup != nil {
		hash := simpleHash(data)
		if _, dup := tc.RelayDedup.LoadOrStore(hash, true); dup {
			return
		}
	}

	if tc.Forward != nil {
		atomic.AddInt64(&tc.Forward.BytesDown, int64(len(data)))
	}
	tc.Conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	tc.Conn.Write(data)
}

func (c *Client) handleCloseTunnel(msg Message) {
	var info TunnelClose
	json.Unmarshal(msg.Payload, &info)

	// If this tunnel is part of a hop bridge, close the other side too.
	c.hopsMu.Lock()
	bridge, isHop := c.hopBridgeByTunnel[info.TunnelID]
	if isHop {
		delete(c.hopBridgeByTunnel, bridge.InboundTunnelID)
		delete(c.hopBridgeByTunnel, bridge.OutboundTunnelID)
		delete(c.hops, bridge.HopID)
		select {
		case <-bridge.Done:
		default:
			close(bridge.Done)
		}
	}
	c.hopsMu.Unlock()

	if isHop {
		// Propagate close to the other side of the bridge
		var targetPeer, targetTunnel string
		if info.TunnelID == bridge.InboundTunnelID {
			targetPeer = bridge.TargetPeerID
			targetTunnel = bridge.OutboundTunnelID
		} else {
			targetPeer = bridge.OriginPeerID
			targetTunnel = bridge.InboundTunnelID
		}
		c.sendTunnelClose(targetPeer, targetTunnel)
		return
	}

	c.closeTunnel(info.TunnelID)
}

func (c *Client) handleTunnelRejected(msg Message) {
	var info TunnelRejected
	json.Unmarshal(msg.Payload, &info)

	// If this is a hop bridge outbound tunnel being rejected by C, clean up the bridge
	// and notify A via hop_forward_reject.
	c.hopsMu.Lock()
	bridge, isHop := c.hopBridgeByTunnel[info.TunnelID]
	if isHop {
		delete(c.hopBridgeByTunnel, bridge.InboundTunnelID)
		delete(c.hopBridgeByTunnel, bridge.OutboundTunnelID)
		delete(c.hops, bridge.HopID)
		select {
		case <-bridge.Done:
		default:
			close(bridge.Done)
		}
	}
	c.hopsMu.Unlock()

	if isHop {
		c.sendRelay(bridge.OriginPeerID, "hop_forward_reject", HopForwardReject{
			HopID:  bridge.HopID,
			Reason: "target rejected: " + info.Reason,
		})
		return
	}

	c.closeTunnel(info.TunnelID)
	c.emit(EventTunnelRejected, LogEvent{Level: "error", Message: "Tunnel rejected: " + info.Reason})
}

func (c *Client) closeTunnel(tunnelID string) {
	c.tunnelsMu.Lock()
	tc, ok := c.tunnels[tunnelID]
	if ok {
		delete(c.tunnels, tunnelID)
	}
	c.tunnelsMu.Unlock()

	if ok && tc.Conn != nil {
		tc.Conn.Close()
		select {
		case <-tc.Done:
		default:
			close(tc.Done)
		}
		if tc.Forward != nil {
			tc.Forward.Mu.Lock()
			tc.Forward.ConnCount--
			tc.Forward.Mu.Unlock()
		}
	}
}

func (c *Client) sendTunnelClose(peerID, tunnelID string) {
	c.sendRelay(peerID, "close_tunnel", TunnelClose{TunnelID: tunnelID})
}

// ExposePort sends a reverse forward offer to a peer.
// The caller (B) asks the peer (A) to open a local listener on targetPort
// that tunnels back to B's sourceHost:sourcePort.
func (c *Client) ExposePort(peerID string, sourceHost string, sourcePort, targetPort int) error {
	fullID, err := c.resolvePeerID(peerID)
	if err != nil {
		return err
	}
	offerID := generateTunnelID()
	return c.sendRelay(fullID, "reverse_forward_offer", ReverseForwardOffer{
		OfferID:    offerID,
		SourceHost: sourceHost,
		SourcePort: sourcePort,
		TargetPort: targetPort,
	})
}

// handleReverseForwardOffer: A receives B's request to expose a port.
// A opens a local listener and creates a Forward that tunnels back to B.
func (c *Client) handleReverseForwardOffer(msg Message) {
	var offer ReverseForwardOffer
	if err := json.Unmarshal(msg.Payload, &offer); err != nil {
		return
	}

	c.acMu.RLock()
	allowed := c.allowForward
	c.acMu.RUnlock()
	if !allowed {
		c.sendRelay(msg.From, "reverse_forward_reject", ReverseForwardReject{
			OfferID: offer.OfferID, Reason: "forwarding disabled",
		})
		return
	}

	// Reuse StartForward: open local listener on targetPort, tunnel to B's sourceHost:sourcePort.
	if err := c.StartForward(msg.From, offer.SourceHost, offer.SourcePort, offer.TargetPort); err != nil {
		c.sendRelay(msg.From, "reverse_forward_reject", ReverseForwardReject{
			OfferID: offer.OfferID, Reason: err.Error(),
		})
		return
	}

	c.sendRelay(msg.From, "reverse_forward_accept", ReverseForwardAccept{
		OfferID: offer.OfferID, TargetPort: offer.TargetPort,
	})
	c.emit(EventReverseForwardStarted, ForwardEvent{
		LocalPort:  offer.TargetPort,
		RemoteHost: offer.SourceHost,
		RemotePort: offer.SourcePort,
		PeerName:   shortID(msg.From),
		Mode:       "REVERSE",
	})
}

func (c *Client) handleReverseForwardAccept(msg Message) {
	var accept ReverseForwardAccept
	if err := json.Unmarshal(msg.Payload, &accept); err != nil {
		return
	}
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("Reverse forward accepted: peer opened :%d", accept.TargetPort)})
}

func (c *Client) handleReverseForwardReject(msg Message) {
	var reject ReverseForwardReject
	if err := json.Unmarshal(msg.Payload, &reject); err != nil {
		return
	}
	c.emit(EventLog, LogEvent{Level: "error", Message: fmt.Sprintf("Reverse forward rejected: %s", reject.Reason)})
}

// simpleHash computes a fast hash for deduplication.
func simpleHash(data []byte) uint64 {
	var h uint64 = 14695981039346656037 // FNV offset
	for _, b := range data {
		h ^= uint64(b)
		h *= 1099511628211 // FNV prime
	}
	return h
}
