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

// Buffer pool for relay mode (large reads, high throughput)
var relayBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 64*1024)
		return &b
	},
}

// directFramePool reuses frame buffers: 12-byte header + 64KB data
var directFramePool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 12+64*1024)
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

	// Snapshot direct TCP connection (check once, use for lifetime)
	// If it breaks mid-stream, we fall back to relay automatically.
	c.peerConnsMu.RLock()
	pc := c.peerConns[peerID]
	var directConn net.Conn
	if pc != nil {
		directConn = pc.DirectTCP
	}
	c.peerConnsMu.RUnlock()

	if directConn != nil {
		// Fast path: direct TCP with zero-copy framing
		c.tunnelReadDirect(tc, peerID, directConn, idBytes)
		return
	}

	// Slow path: WebSocket relay
	c.tunnelReadRelay(tc, peerID)
}

// tunnelReadDirect: high-performance direct TCP path.
// Single write per frame (header+data in one buffer), pooled buffers.
func (c *Client) tunnelReadDirect(tc *TunnelConn, peerID string, directConn net.Conn, idBytes []byte) {
	framePtr := directFramePool.Get().(*[]byte)
	defer directFramePool.Put(framePtr)
	frame := *framePtr

	// Pre-fill tunnel ID in frame header (never changes)
	copy(frame[:8], idBytes)

	for {
		select {
		case <-tc.Done:
			return
		case <-c.done:
			return
		default:
		}

		tc.Conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		// Read directly into frame buffer after the 12-byte header
		n, err := tc.Conn.Read(frame[12:])
		if n > 0 {
			if tc.Forward != nil {
				atomic.AddInt64(&tc.Forward.BytesUp, int64(n))
			}

			// Write length into header
			frame[8] = byte(n >> 24)
			frame[9] = byte(n >> 16)
			frame[10] = byte(n >> 8)
			frame[11] = byte(n)

			// Single write: header + data in one syscall
			directConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, werr := directConn.Write(frame[:12+n])
			if werr != nil {
				// Direct TCP broken — fall back to relay for remaining data
				c.peerConnsMu.Lock()
				if pc, ok := c.peerConns[peerID]; ok {
					pc.DirectTCP = nil
				}
				c.peerConnsMu.Unlock()
				directConn.Close()
				// Continue with relay path
				c.tunnelReadRelay(tc, peerID)
				return
			}
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
			encoded := base64.StdEncoding.EncodeToString(buf[:n])
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
	tc.Conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	tc.Conn.Write(data)
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
