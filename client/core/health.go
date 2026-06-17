package core

import (
	"net"
	"strconv"
	"sync/atomic"
	"time"
)

// Connection health layer.
//
// We probe the direct (P2P/UDP) path with a timestamp-echo ping so we can measure
// round-trip time and liveness WITHOUT keeping any per-probe state: the sender embeds
// its own clock reading, the responder echoes it verbatim, and the sender derives
// RTT = now - echoed_ts on receipt. The PONG round-trip is exactly the right signal
// for the *outbound* transport decision — it confirms both directions of the 5-tuple
// work, which inbound-only liveness can't.
//
// This drives the seamless path switcher (pathswitch.go): a direct path that stops
// echoing is declared dead and live flows fall back to relay; a path that resumes
// echoing is upgraded back to direct. Because the data plane runs over gVisor TCP
// (port forwarding / VPN) or RUTP (legacy tunnels), the switch is lossless and
// in-order — the reliable layer simply retransmits the gap over the new path.
//
// Wire format (ASCII, matches the existing PUNCH:/KEY: prefix style):
//   A -> B : "PING2:" + A.id + ":" + decimal(unixNano)
//   B -> A : "PONG2:" + B.id + ":" + decimal(echoed unixNano)
// Legacy "PING" keepalives are still accepted (ignored) for back-compat.

var (
	prefixPing2 = []byte("PING2:")
	prefixPong2 = []byte("PONG2:")
)

const (
	// rttAlpha is the EWMA weight for new RTT samples.
	rttAlpha = 0.25
	// directLivenessWindow: a direct path with no PONG within this window is
	// considered dead. ~3 keepalive intervals (keepalive probes every 5s).
	directLivenessWindow = 16 * time.Second
	// directPromoteSettle debounces relay→direct promotion to avoid flapping.
	directPromoteSettle = 3 * time.Second
)

// updateRTT folds a new RTT sample into the peer's EWMA. Caller must hold peerConnsMu.
func (pc *PeerConn) updateRTT(sample time.Duration) {
	if sample <= 0 {
		return
	}
	if pc.DirectRTT <= 0 {
		pc.DirectRTT = sample
		return
	}
	pc.DirectRTT = time.Duration(float64(pc.DirectRTT)*(1-rttAlpha) + float64(sample)*rttAlpha)
}

// markDirectRecvNano records a direct-path liveness timestamp on pc (lock-free).
func markDirectRecvNano(pc *PeerConn) {
	atomic.StoreInt64(&pc.directLastRecvNano, time.Now().UnixNano())
}

// directRecvAgeNano returns nanoseconds since the last direct packet (lock-free).
func directRecvAgeNano(pc *PeerConn, nowNano int64) int64 {
	return nowNano - atomic.LoadInt64(&pc.directLastRecvNano)
}

// touchDirectRecvByAddr refreshes direct-path liveness for whichever peer owns the
// given UDP address. Called on every inbound UDP packet so that *any* direct
// traffic — data, PUNCH, even an old peer that never echoes PONG2 — keeps the path
// alive. Lock-free store under a shared read lock; cheap enough for the hot path.
func (c *Client) touchDirectRecvByAddr(addr *net.UDPAddr) {
	if addr == nil {
		return
	}
	nano := time.Now().UnixNano()
	c.peerConnsMu.RLock()
	for _, pc := range c.peerConns {
		if pc.UDPAddr != nil && pc.UDPAddr.Port == addr.Port && pc.UDPAddr.IP.Equal(addr.IP) {
			atomic.StoreInt64(&pc.directLastRecvNano, nano)
			break
		}
	}
	c.peerConnsMu.RUnlock()
}

// sendDirectPing sends a PING2 probe to a peer over the shared UDP socket.
func (c *Client) sendDirectPing(addr *net.UDPAddr) {
	if addr == nil {
		return
	}
	c.connMu.Lock()
	udp := c.udpConn
	c.connMu.Unlock()
	if udp == nil {
		return
	}
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	pkt := append([]byte("PING2:"+c.MyID+":"), ts...)
	udp.WriteToUDP(pkt, addr)
}

// handlePing2 replies to a PING2 probe, echoing the sender's timestamp verbatim.
// The responder gains no liveness confidence (it only learns the inbound direction
// works); only the prober that receives the PONG does. payload = "<senderID>:<tsNano>".
func (c *Client) handlePing2(addr *net.UDPAddr, payload []byte) {
	_, ts, ok := splitIDTS(payload)
	if !ok {
		return
	}
	c.connMu.Lock()
	udp := c.udpConn
	c.connMu.Unlock()
	if udp == nil {
		return
	}
	pong := append([]byte("PONG2:"+c.MyID+":"), ts...)
	udp.WriteToUDP(pong, addr)
}

// handlePong2 processes a PONG2 reply and updates the peer's direct RTT + liveness.
// payload = "<responderID>:<echoedTsNano>".
func (c *Client) handlePong2(addr *net.UDPAddr, payload []byte) {
	_, tsStr, ok := splitIDTS(payload)
	if !ok {
		return
	}
	tsNano, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return
	}
	rtt := time.Duration(time.Now().UnixNano() - tsNano)
	if rtt < 0 || rtt > 30*time.Second {
		return // clock skew / bogus echo
	}

	peerID := c.findPeerByUDPAddr(addr)
	if peerID == "" {
		return
	}
	c.peerConnsMu.Lock()
	if pc, ok := c.peerConns[peerID]; ok {
		markDirectRecvNano(pc)
		pc.updateRTT(rtt)
	}
	c.peerConnsMu.Unlock()
}

// splitIDTS splits "<id>:<ts>" payloads. Peer IDs are colon-free hex, so a single
// split on the first ':' is unambiguous.
func splitIDTS(payload []byte) (id string, ts string, ok bool) {
	for i, b := range payload {
		if b == ':' {
			if i == 0 || i == len(payload)-1 {
				return "", "", false
			}
			return string(payload[:i]), string(payload[i+1:]), true
		}
	}
	return "", "", false
}
