package core

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient builds a minimal Client sufficient for exercising the health /
// path-switching logic without sockets or a server.
func newTestClient() *Client {
	return &Client{
		peerConns: make(map[string]*PeerConn),
		events:    make(chan Event, 64),
		MyID:      "self",
	}
}

func TestRefreshPeerTransports_PromoteAndDemote(t *testing.T) {
	c := newTestClient()
	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 4000}
	pc := &PeerConn{PeerID: "p", Mode: "direct", UDPAddr: addr}
	markDirectRecvNano(pc) // path is fresh
	c.peerConns["p"] = pc

	// lastSwitch is zero → promotion is immediate.
	c.refreshPeerTransports()
	if !c.peerDirectUsable("p") {
		t.Fatalf("expected direct path to be promoted when fresh")
	}

	// Path goes silent past the liveness window → demote on the next tick.
	atomic.StoreInt64(&pc.directLastRecvNano,
		time.Now().Add(-directLivenessWindow-time.Second).UnixNano())
	c.refreshPeerTransports()
	if c.peerDirectUsable("p") {
		t.Fatalf("expected direct path to be demoted to relay when stale")
	}
}

func TestRefreshPeerTransports_PromoteDebounced(t *testing.T) {
	c := newTestClient()
	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 4000}
	pc := &PeerConn{PeerID: "p", Mode: "direct", UDPAddr: addr, directHealthy: false}
	markDirectRecvNano(pc)
	pc.lastSwitch = time.Now() // just switched → promotion must wait
	c.peerConns["p"] = pc

	c.refreshPeerTransports()
	if c.peerDirectUsable("p") {
		t.Fatalf("promotion should be debounced immediately after a switch")
	}

	// Once the settle window has elapsed, promotion is allowed.
	pc.lastSwitch = time.Now().Add(-directPromoteSettle - time.Second)
	c.refreshPeerTransports()
	if !c.peerDirectUsable("p") {
		t.Fatalf("promotion should succeed after the settle window")
	}
}

func TestRefreshPeerTransports_RelayModeNeverDirect(t *testing.T) {
	c := newTestClient()
	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 4000}
	// Mode is "relay" (punch gave up) — even a fresh recv must not promote.
	pc := &PeerConn{PeerID: "p", Mode: "relay", UDPAddr: addr}
	markDirectRecvNano(pc)
	c.peerConns["p"] = pc

	c.refreshPeerTransports()
	if c.peerDirectUsable("p") {
		t.Fatalf("a relay-mode peer must never be marked direct-usable")
	}
}

func TestDirectEndpoint_RequiresHealthy(t *testing.T) {
	c := newTestClient()
	addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 4000}
	pc := &PeerConn{PeerID: "p", Mode: "direct", UDPAddr: addr, directHealthy: false}
	c.peerConns["p"] = pc

	if _, ok := c.directEndpoint("p"); ok {
		t.Fatalf("an unhealthy direct path must not be usable")
	}
	pc.directHealthy = true
	if got, ok := c.directEndpoint("p"); !ok || got != addr {
		t.Fatalf("a healthy direct path should return its addr")
	}
	if _, ok := c.directEndpoint("unknown"); ok {
		t.Fatalf("an unknown peer must not be usable")
	}

	// No UDP address → not usable even if flagged healthy.
	pc.UDPAddr = nil
	if _, ok := c.directEndpoint("p"); ok {
		t.Fatalf("missing UDPAddr must not be usable")
	}
}
