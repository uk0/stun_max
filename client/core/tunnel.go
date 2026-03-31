package core

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Buffer pools
var (
	// Small buffer for P2P mode: fits in one UDP packet (no fragmentation)
	p2pBufPool = sync.Pool{
		New: func() interface{} {
			b := make([]byte, 1200)
			return &b
		},
	}
	// Large buffer for relay mode: maximize throughput
	relayBufPool = sync.Pool{
		New: func() interface{} {
			b := make([]byte, 64*1024)
			return &b
		},
	}
)

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

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", localPort))
	if err != nil {
		return fmt.Errorf("listen :%d: %w", localPort, err)
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

// getForwardMode returns "P2P" if peer has direct connection, "RELAY" otherwise.
func (c *Client) getForwardMode(peerID string) string {
	c.peerConnsMu.RLock()
	defer c.peerConnsMu.RUnlock()
	if pc, ok := c.peerConns[peerID]; ok && pc.Mode == "direct" {
		return "P2P"
	}
	return "RELAY"
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
				c.emit(EventLog, LogEvent{Level: "error", Message: fmt.Sprintf("Accept error :%d: %v", fwd.LocalPort, err)})
			}
			return
		}

		optimizeTCP(conn)

		tunnelID := generateTunnelID()
		tc := &TunnelConn{
			TunnelID: tunnelID, PeerID: fwd.PeerID,
			Conn: conn, Forward: fwd, Done: make(chan struct{}),
		}

		c.tunnelsMu.Lock()
		c.tunnels[tunnelID] = tc
		c.tunnelsMu.Unlock()

		fwd.Mu.Lock()
		fwd.ConnCount++
		fwd.Mu.Unlock()

		if err := c.sendRelay(fwd.PeerID, "open_tunnel", TunnelOpen{
			TunnelID: tunnelID, TargetHost: fwd.RemoteHost, TargetPort: fwd.RemotePort,
		}); err != nil {
			conn.Close()
			c.tunnelsMu.Lock()
			delete(c.tunnels, tunnelID)
			c.tunnelsMu.Unlock()
			fwd.Mu.Lock()
			fwd.ConnCount--
			fwd.Mu.Unlock()
			continue
		}
	}
}

func optimizeTCP(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetReadBuffer(512 * 1024)
		tc.SetWriteBuffer(512 * 1024)
	}
}

// StopForward closes a forward and all its tunnels.
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

	target := net.JoinHostPort(req.TargetHost, strconv.Itoa(req.TargetPort))
	conn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		c.emit(EventLog, LogEvent{Level: "error", Message: fmt.Sprintf("Cannot connect to %s: %v", target, err)})
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

// tunnelReadLoop: reads TCP, sends to peer.
// P2P direct: read 1200 bytes (one UDP packet, no fragmentation).
// Relay: read 64KB (maximize throughput).
// Automatically picks the best path per-read based on current peer state.
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

	for {
		select {
		case <-tc.Done:
			return
		case <-c.done:
			return
		default:
		}

		// Check current peer mode to decide buffer size and send path
		c.peerConnsMu.RLock()
		pc := c.peerConns[peerID]
		isP2P := pc != nil && pc.Mode == "direct" && pc.UDPAddr != nil && c.udpConn != nil
		c.peerConnsMu.RUnlock()

		// Check if user forced relay
		forceRelay := false
		if tc.Forward != nil {
			tc.Forward.Mu.Lock()
			forceRelay = tc.Forward.ForceRelay
			tc.Forward.Mu.Unlock()
		}

		useP2P := isP2P && !forceRelay

		var bufPtr *[]byte
		if useP2P {
			bp := p2pBufPool.Get().(*[]byte)
			bufPtr = bp
		} else {
			bp := relayBufPool.Get().(*[]byte)
			bufPtr = bp
		}
		buf := *bufPtr

		tc.Conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		n, err := tc.Conn.Read(buf)
		if n > 0 {
			if tc.Forward != nil {
				atomic.AddInt64(&tc.Forward.BytesUp, int64(n))
			}

			if useP2P {
				// Single UDP packet, no fragmentation needed (buf ≤ 1200)
				c.sendOneUDPPacket(pc, idBytes, buf[:n])
			} else {
				// Relay: base64 + JSON over WebSocket
				encoded := base64.StdEncoding.EncodeToString(buf[:n])
				c.sendRelay(peerID, "tunnel_data", TunnelData{TunnelID: tc.TunnelID, Data: encoded})
			}
		}

		if useP2P {
			p2pBufPool.Put(bufPtr)
		} else {
			relayBufPool.Put(bufPtr)
		}

		if err != nil {
			return
		}
	}
}

// sendOneUDPPacket sends exactly one UDP packet (no fragmentation).
// Packet: [0x00][8-byte tunnelID][payload or encrypted payload]
func (c *Client) sendOneUDPPacket(pc *PeerConn, idBytes []byte, data []byte) {
	hasCrypto := pc.Crypto != nil && pc.Crypto.Encrypted

	if hasCrypto {
		encrypted, err := pc.Crypto.Encrypt(data)
		if err != nil {
			// Encryption failed, send plaintext
			pkt := make([]byte, 9+len(data))
			pkt[0] = 0x00
			copy(pkt[1:9], idBytes)
			copy(pkt[9:], data)
			c.udpConn.WriteToUDP(pkt, pc.UDPAddr)
			return
		}
		pkt := make([]byte, 9+len(encrypted))
		pkt[0] = 0x00
		copy(pkt[1:9], idBytes)
		copy(pkt[9:], encrypted)
		c.udpConn.WriteToUDP(pkt, pc.UDPAddr)
	} else {
		pkt := make([]byte, 9+len(data))
		pkt[0] = 0x00
		copy(pkt[1:9], idBytes)
		copy(pkt[9:], data)
		c.udpConn.WriteToUDP(pkt, pc.UDPAddr)
	}
}

// sendUDPDirect is the method-compatible wrapper.
func (c *Client) sendUDPDirect(pc *PeerConn, tunnelID string, data []byte) bool {
	idBytes := tunnelIDToBytes(tunnelID)
	// For data > 1200, we must fragment
	const maxPayload = 1200
	for offset := 0; offset < len(data); offset += maxPayload {
		end := offset + maxPayload
		if end > len(data) {
			end = len(data)
		}
		c.sendOneUDPPacket(pc, idBytes, data[offset:end])
	}
	return true
}

func (c *Client) handleTunnelOpened(msg Message) {
	var info TunnelClose
	if err := json.Unmarshal(msg.Payload, &info); err != nil {
		return
	}
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
	c.tunnelsMu.RLock()
	tc, ok := c.tunnels[td.TunnelID]
	c.tunnelsMu.RUnlock()
	if !ok {
		return
	}

	data, err := base64.StdEncoding.DecodeString(td.Data)
	if err != nil {
		return
	}

	if tc.Forward != nil {
		atomic.AddInt64(&tc.Forward.BytesDown, int64(len(data)))
	}
	tc.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := tc.Conn.Write(data); err != nil {
		c.closeTunnel(td.TunnelID)
	}
}

func (c *Client) handleCloseTunnel(msg Message) {
	var info TunnelClose
	json.Unmarshal(msg.Payload, &info)
	c.closeTunnel(info.TunnelID)
}

func (c *Client) handleTunnelRejected(msg Message) {
	var info TunnelRejected
	json.Unmarshal(msg.Payload, &info)
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
