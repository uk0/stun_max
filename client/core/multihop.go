package core

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// HopBridge bridges two tunnels: inbound (A→B) and outbound (B→C).
// B holds this struct; data from A's tunnel is forwarded to C's tunnel and vice versa.
type HopBridge struct {
	HopID            string
	InboundTunnelID  string // A uses this tunnel ID when sending tunnel_data to B
	OutboundTunnelID string // B uses this tunnel ID when sending tunnel_data to C
	OriginPeerID     string // A's peer ID
	TargetPeerID     string // C's peer ID
	// Fields used on A's side for the local listener
	LocalPort  int
	ViaPeerID  string
	RemoteHost string
	RemotePort int
	Done       chan struct{}
}

// StartHopForward sets up a multi-hop forward: local port → B → C:host:remotePort.
// A sends hop_forward to B; B bridges the tunnel to C.
// Once B accepts, A creates a local listener and routes connections through B.
func (c *Client) StartHopForward(viaPeerID, targetPeerID, host string, remotePort, localPort int) error {
	viaID, err := c.resolvePeerID(viaPeerID)
	if err != nil {
		return fmt.Errorf("via peer: %w", err)
	}
	targetID, err := c.resolvePeerID(targetPeerID)
	if err != nil {
		return fmt.Errorf("target peer: %w", err)
	}

	hopID := generateTunnelID()

	// Store pending hop on A's side so handleHopForwardAccept can create the listener
	c.hopsMu.Lock()
	c.hops[hopID] = &HopBridge{
		HopID:        hopID,
		OriginPeerID: c.MyID,
		TargetPeerID: targetID,
		ViaPeerID:    viaID,
		LocalPort:    localPort,
		RemoteHost:   host,
		RemotePort:   remotePort,
		Done:         make(chan struct{}),
	}
	c.hopsMu.Unlock()

	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
		"Hop forward: :%d → %s → %s:%s:%d", localPort, shortID(viaID), shortID(targetID), host, remotePort,
	)})

	return c.sendRelay(viaID, "hop_forward", HopForwardRequest{
		HopID:        hopID,
		TargetPeerID: targetID,
		TargetHost:   host,
		TargetPort:   remotePort,
	})
}

// handleHopForward is called on B when A sends a hop_forward request.
// B creates two tunnel IDs (inbound for A→B, outbound for B→C), sends open_tunnel
// to C, and tells A the inbound tunnel ID to use.
func (c *Client) handleHopForward(msg Message) {
	var req HopForwardRequest
	if err := json.Unmarshal(msg.Payload, &req); err != nil {
		return
	}

	c.acMu.RLock()
	allowed := c.allowForward
	c.acMu.RUnlock()
	if !allowed {
		c.sendRelay(msg.From, "hop_forward_reject", HopForwardReject{
			HopID:  req.HopID,
			Reason: "forwarding disabled",
		})
		return
	}

	// B creates two distinct tunnel IDs to avoid ambiguity in handleTunnelData
	inboundTunnelID := generateTunnelID()  // A→B direction
	outboundTunnelID := generateTunnelID() // B→C direction

	bridge := &HopBridge{
		HopID:            req.HopID,
		InboundTunnelID:  inboundTunnelID,
		OutboundTunnelID: outboundTunnelID,
		OriginPeerID:     msg.From,
		TargetPeerID:     req.TargetPeerID,
		Done:             make(chan struct{}),
	}

	c.hopsMu.Lock()
	c.hops[req.HopID] = bridge
	// Register both IDs so handleTunnelData can find the bridge from either direction
	c.hopBridgeByTunnel[inboundTunnelID] = bridge
	c.hopBridgeByTunnel[outboundTunnelID] = bridge
	c.hopsMu.Unlock()

	// Send open_tunnel to C
	if err := c.sendRelay(req.TargetPeerID, "open_tunnel", TunnelOpen{
		TunnelID:   outboundTunnelID,
		TargetHost: req.TargetHost,
		TargetPort: req.TargetPort,
	}); err != nil {
		c.hopsMu.Lock()
		delete(c.hops, req.HopID)
		delete(c.hopBridgeByTunnel, inboundTunnelID)
		delete(c.hopBridgeByTunnel, outboundTunnelID)
		c.hopsMu.Unlock()
		c.sendRelay(msg.From, "hop_forward_reject", HopForwardReject{
			HopID:  req.HopID,
			Reason: "failed to reach target peer",
		})
		return
	}

	// Tell A the inbound tunnel ID it should use when sending tunnel_data to B
	c.sendRelay(msg.From, "hop_forward_accept", HopForwardAccept{
		HopID:           req.HopID,
		InboundTunnelID: inboundTunnelID,
	})

	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
		"Hop bridge: %s → %s (in:%s out:%s)",
		shortID(msg.From), shortID(req.TargetPeerID),
		inboundTunnelID[:8], outboundTunnelID[:8],
	)})
}

// handleHopForwardAccept is called on A when B confirms the bridge.
// A stores the inbound tunnel ID and starts a local listener.
// Note: A does NOT register in hopBridgeByTunnel — A's data path uses c.tunnels normally.
func (c *Client) handleHopForwardAccept(msg Message) {
	var accept HopForwardAccept
	if err := json.Unmarshal(msg.Payload, &accept); err != nil {
		return
	}

	c.hopsMu.Lock()
	bridge, ok := c.hops[accept.HopID]
	if ok {
		bridge.InboundTunnelID = accept.InboundTunnelID
		// Do NOT register in hopBridgeByTunnel on A's side:
		// A writes incoming tunnel_data to the local TCP conn via c.tunnels (normal path).
	}
	c.hopsMu.Unlock()

	if !ok {
		return
	}

	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
		"Hop forward accepted by %s (tunnel %s)", shortID(msg.From), accept.InboundTunnelID[:8],
	)})

	go c.hopAcceptLoop(bridge, msg.From)
}

// hopAcceptLoop listens on bridge.LocalPort and for each connection creates a
// TunnelConn with the pre-assigned inbound tunnel ID, then starts the relay read loop.
// No open_tunnel is sent — B already has the bridge to C established.
func (c *Client) hopAcceptLoop(bridge *HopBridge, viaPeerID string) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", bridge.LocalPort))
	if err != nil {
		c.emit(EventLog, LogEvent{Level: "error", Message: fmt.Sprintf(
			"Hop forward: cannot listen on :%d: %v", bridge.LocalPort, err,
		)})
		return
	}
	defer listener.Close()

	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
		"Hop forward listening on :%d → %s → %s:%d",
		bridge.LocalPort, shortID(viaPeerID), bridge.RemoteHost, bridge.RemotePort,
	)})

	for {
		select {
		case <-bridge.Done:
			return
		case <-c.done:
			return
		default:
		}

		listener.(*net.TCPListener).SetDeadline(time.Now().Add(1 * time.Second))
		conn, err := listener.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		optimizeTCP(conn)

		// Use the pre-assigned inbound tunnel ID so B can bridge to C
		tc := &TunnelConn{
			TunnelID: bridge.InboundTunnelID,
			PeerID:   viaPeerID,
			Conn:     conn,
			Done:     make(chan struct{}),
		}
		c.tunnelsMu.Lock()
		c.tunnels[bridge.InboundTunnelID] = tc
		c.tunnelsMu.Unlock()

		c.wg.Add(1)
		go c.tunnelReadLoop(tc, viaPeerID)
	}
}

// handleHopForwardReject is called on A when B rejects the hop request.
func (c *Client) handleHopForwardReject(msg Message) {
	var reject HopForwardReject
	if err := json.Unmarshal(msg.Payload, &reject); err != nil {
		return
	}

	c.hopsMu.Lock()
	bridge, ok := c.hops[reject.HopID]
	if ok {
		delete(c.hops, reject.HopID)
		select {
		case <-bridge.Done:
		default:
			close(bridge.Done)
		}
	}
	c.hopsMu.Unlock()

	c.emit(EventLog, LogEvent{Level: "error", Message: fmt.Sprintf(
		"Hop forward rejected by %s: %s", shortID(msg.From), reject.Reason,
	)})
}
