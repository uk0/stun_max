package core

import (
	"net"
	"strconv"
	"testing"
	"time"
)

func TestSplitIDTS(t *testing.T) {
	cases := []struct {
		in, id, ts string
		ok         bool
	}{
		{"abc123:456", "abc123", "456", true},
		{"deadbeef:1700000000000000000", "deadbeef", "1700000000000000000", true},
		{"noColon", "", "", false},
		{":leading", "", "", false},
		{"trailing:", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		id, ts, ok := splitIDTS([]byte(tc.in))
		if ok != tc.ok || id != tc.id || ts != tc.ts {
			t.Errorf("splitIDTS(%q) = (%q,%q,%v); want (%q,%q,%v)",
				tc.in, id, ts, ok, tc.id, tc.ts, tc.ok)
		}
	}
}

func TestUpdateRTT_EWMA(t *testing.T) {
	pc := &PeerConn{}

	pc.updateRTT(100 * time.Millisecond)
	if pc.DirectRTT != 100*time.Millisecond {
		t.Fatalf("first sample seeds EWMA: got %v want 100ms", pc.DirectRTT)
	}

	// alpha=0.25: 100ms*0.75 + 200ms*0.25 = 125ms
	pc.updateRTT(200 * time.Millisecond)
	if pc.DirectRTT != 125*time.Millisecond {
		t.Fatalf("EWMA fold: got %v want 125ms", pc.DirectRTT)
	}

	// Non-positive samples are ignored.
	prev := pc.DirectRTT
	pc.updateRTT(0)
	pc.updateRTT(-1 * time.Millisecond)
	if pc.DirectRTT != prev {
		t.Fatalf("non-positive sample mutated RTT: got %v want %v", pc.DirectRTT, prev)
	}
}

func TestHandlePong2_UpdatesRTTAndLiveness(t *testing.T) {
	c := newTestClient()
	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 5000}
	c.peerConns["peerA"] = &PeerConn{PeerID: "peerA", Mode: "direct", UDPAddr: addr}

	sent := time.Now().Add(-40 * time.Millisecond).UnixNano()
	c.handlePong2(addr, []byte("peerA:"+strconv.FormatInt(sent, 10)))

	c.peerConnsMu.RLock()
	pc := c.peerConns["peerA"]
	rtt := pc.DirectRTT
	age := directRecvAgeNano(pc, time.Now().UnixNano())
	c.peerConnsMu.RUnlock()

	if rtt < 30*time.Millisecond || rtt > 200*time.Millisecond {
		t.Fatalf("RTT out of plausible range: %v", rtt)
	}
	if age > int64(time.Second) {
		t.Fatalf("liveness was not refreshed: age=%dns", age)
	}
}

func TestHandlePong2_BogusEchoIgnored(t *testing.T) {
	c := newTestClient()
	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 5000}
	c.peerConns["peerA"] = &PeerConn{PeerID: "peerA", Mode: "direct", UDPAddr: addr}

	// A timestamp in the future yields a negative RTT and must be dropped.
	future := time.Now().Add(time.Hour).UnixNano()
	c.handlePong2(addr, []byte("peerA:"+strconv.FormatInt(future, 10)))

	if c.peerConns["peerA"].DirectRTT != 0 {
		t.Fatalf("bogus future echo updated RTT: %v", c.peerConns["peerA"].DirectRTT)
	}
}

func TestHandlePong2_UnknownAddrNoPanic(t *testing.T) {
	c := newTestClient()
	// No peer registered at this addr — must be a safe no-op.
	addr := &net.UDPAddr{IP: net.IPv4(9, 9, 9, 9), Port: 1234}
	c.handlePong2(addr, []byte("ghost:"+strconv.FormatInt(time.Now().UnixNano(), 10)))
}
