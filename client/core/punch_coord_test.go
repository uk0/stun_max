package core

import (
	"encoding/json"
	"testing"
	"time"
)

func TestElectInitiator(t *testing.T) {
	c := newTestClient() // MyID = "self"
	if !c.electInitiator("zzzz") {
		t.Errorf("self < zzzz: should be initiator")
	}
	if c.electInitiator("aaaa") {
		t.Errorf("self > aaaa: should not be initiator")
	}
}

func TestMyPredictedPorts_Cone(t *testing.T) {
	c := newTestClient()
	c.publicAddr = "203.0.113.7:40001"
	ports := c.myPredictedPorts()
	if len(ports) != 1 || ports[0] != 40001 {
		t.Fatalf("cone NAT predicts only the stable public port; got %v", ports)
	}
}

func TestMyPredictedPorts_Sequential(t *testing.T) {
	c := newTestClient()
	c.publicAddr = "203.0.113.7:40001"
	c.portModel = &portModel{Name: "sequential", Delta: 2, Anchor: 40001, AnchorAt: time.Now()}
	ports := c.myPredictedPorts()

	has := map[int]bool{}
	for _, p := range ports {
		has[p] = true
	}
	if !has[40003] || !has[40005] {
		t.Fatalf("sequential prediction missing expected ports; got %v", ports)
	}
	if len(ports) > 64 {
		t.Fatalf("prediction set unbounded: %d entries", len(ports))
	}
}

func TestClampPorts(t *testing.T) {
	out := clampPorts([]int{0, 1024, 1025, 65534, 65535, 70000, 8080})
	if len(out) != 3 {
		t.Fatalf("clampPorts kept %v, want 3 valid ports", out)
	}
	for _, p := range out {
		if p <= 1024 || p >= 65535 {
			t.Fatalf("clampPorts kept invalid port %d", p)
		}
	}

	big := make([]int, 500)
	for i := range big {
		big[i] = 2000 + i
	}
	if got := clampPorts(big); len(got) != 128 {
		t.Fatalf("clampPorts cap: got %d want 128", len(got))
	}
}

func TestHandlePunchSync_OfferStoresPeerPorts(t *testing.T) {
	c := newTestClient()
	c.peerConns["peerZ"] = &PeerConn{PeerID: "peerZ", Mode: "connecting", NATType: NATSymmetric}

	payload, _ := json.Marshal(PunchSync{
		Phase:          "offer",
		Nonce:          "n1",
		PredictedPorts: []int{40010, 40012, 99999}, // 99999 must be dropped
	})
	c.handlePunchSync(Message{Type: "punch_sync", From: "peerZ", Payload: payload})

	c.peerConnsMu.RLock()
	got := c.peerConns["peerZ"].PeerPredictedPorts
	c.peerConnsMu.RUnlock()
	if len(got) != 2 || got[0] != 40010 || got[1] != 40012 {
		t.Fatalf("expected clamped peer ports [40010 40012]; got %v", got)
	}
}

func TestHandlePunchSync_IgnoresSelfAndUnknown(t *testing.T) {
	c := newTestClient()
	payload, _ := json.Marshal(PunchSync{Phase: "offer", PredictedPorts: []int{40010}})
	// Must be safe no-ops (no panic, nothing stored).
	c.handlePunchSync(Message{From: "self", Payload: payload})
	c.handlePunchSync(Message{From: "ghost", Payload: payload})
}

func TestMaybeCoordinatePunch_ElectionAndThrottle(t *testing.T) {
	c := newTestClient() // MyID "self"

	// Soft (cone) peer: no coordination.
	c.peerConns["zsoft"] = &PeerConn{PeerID: "zsoft", NATType: NATFullCone}
	c.maybeCoordinatePunch("zsoft")
	if !c.peerConns["zsoft"].lastCoord.IsZero() {
		t.Fatalf("non-hard peer must not trigger coordination")
	}

	// Hard peer where we are the elected initiator (self < zhard).
	c.peerConns["zhard"] = &PeerConn{PeerID: "zhard", NATType: NATSymmetric}
	c.maybeCoordinatePunch("zhard")
	first := c.peerConns["zhard"].lastCoord
	if first.IsZero() {
		t.Fatalf("hard peer + initiator must send a coordination offer")
	}
	// Throttled: a second immediate call must not re-offer.
	c.maybeCoordinatePunch("zhard")
	if !c.peerConns["zhard"].lastCoord.Equal(first) {
		t.Fatalf("coordination offers must be throttled")
	}

	// Hard peer where we are NOT the initiator (self > aaaa).
	c.peerConns["aaaa"] = &PeerConn{PeerID: "aaaa", NATType: NATSymmetric}
	c.maybeCoordinatePunch("aaaa")
	if !c.peerConns["aaaa"].lastCoord.IsZero() {
		t.Fatalf("non-initiator must not send a coordination offer")
	}
}
