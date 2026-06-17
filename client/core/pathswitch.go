package core

import (
	"fmt"
	"net"
	"time"
)

// Seamless relay <-> NAT(direct) path switching.
//
// Every data-plane send site (forward netstack, VPN, file, speedtest, legacy tunnel)
// asks ONE question before choosing UDP vs relay: is the peer's direct path healthy
// right now? That question is answered by `directHealthy`, a sticky per-peer flag
// maintained here with hysteresis so the answer can flip *at any time* without
// flapping:
//
//   - DEMOTE (direct -> relay): immediate. The moment the direct path stops echoing
//     PONG2 within directLivenessWindow, we stop trusting it. Outbound data switches
//     to relay on the very next packet; the reliable layer (gVisor TCP / RUTP) simply
//     retransmits the in-flight gap over relay — lossless, in-order.
//   - PROMOTE (relay -> direct): debounced by directPromoteSettle. Once the path
//     echoes again (punch revived, NAT remapped), we wait a beat to confirm stability
//     then upgrade live flows back to P2P.
//
// `Mode` ("connecting"/"direct"/"relay") remains the punch state machine's business;
// `directHealthy` is the finer-grained "is the punched path actually carrying traffic"
// gate that the data plane consults. This is what makes the switch seamless and
// continuous rather than a one-shot decision at tunnel-open time.

// directEndpoint returns the peer's UDP address iff its direct path is healthy and
// usable right now, else (nil, false). Locks internally — safe for callers NOT
// already holding peerConnsMu.
func (c *Client) directEndpoint(peerID string) (*net.UDPAddr, bool) {
	c.peerConnsMu.RLock()
	defer c.peerConnsMu.RUnlock()
	pc := c.peerConns[peerID]
	if pc == nil || pc.UDPAddr == nil || !pc.directHealthy {
		return nil, false
	}
	return pc.UDPAddr, true
}

// peerDirectUsable reports whether the direct path to a peer is healthy right now.
func (c *Client) peerDirectUsable(peerID string) bool {
	_, ok := c.directEndpoint(peerID)
	return ok
}

// refreshPeerTransports re-evaluates every peer's direct-path health and flips the
// sticky `directHealthy` flag with hysteresis. Called from the keepalive tick (every
// 5s) and after punch state changes. Emits a log line on each flip so the seamless
// failover is visible in the dashboard / CLI.
func (c *Client) refreshPeerTransports() {
	nowNano := time.Now().UnixNano()
	now := time.Now()

	type flip struct {
		id      string
		healthy bool
		rtt     time.Duration
	}
	var flips []flip

	c.peerConnsMu.Lock()
	for id, pc := range c.peerConns {
		rawDirect := pc.Mode == "direct" && pc.UDPAddr != nil &&
			directRecvAgeNano(pc, nowNano) < int64(directLivenessWindow)

		switch {
		case pc.directHealthy && !rawDirect:
			// Path went dead — demote immediately (safety first).
			pc.directHealthy = false
			pc.lastSwitch = now
			flips = append(flips, flip{id, false, pc.DirectRTT})

		case !pc.directHealthy && rawDirect:
			// Path is alive — promote, debounced to avoid flapping.
			if pc.lastSwitch.IsZero() || now.Sub(pc.lastSwitch) >= directPromoteSettle {
				pc.directHealthy = true
				pc.lastSwitch = now
				flips = append(flips, flip{id, true, pc.DirectRTT})
			}
		}
	}
	c.peerConnsMu.Unlock()

	for _, f := range flips {
		if f.healthy {
			c.emit(EventHolePunchSuccess, PeerEvent{ID: f.id, Status: "direct"})
			c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
				"Path %s → DIRECT (rtt %s): live traffic upgraded to P2P",
				shortID(f.id), f.rtt.Round(time.Millisecond))})
		} else {
			c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf(
				"Path %s → RELAY: direct path unresponsive, failing over seamlessly", shortID(f.id))})
		}
	}
}
