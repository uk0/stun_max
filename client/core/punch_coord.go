package core

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Coordinated simultaneous hole punching — "black magic" for hostile NATs.
//
// Independent, uncoordinated punching rarely beats symmetric NAT / CGNAT: the two
// NAT mappings must be open at the same instant, and each side must aim at where
// the OTHER side's NAT will allocate its external port. We add a relay-signalled
// rendezvous so both peers punch within ~1 RTT of each other AND exchange their
// predicted external ports, so each side targets the peer's likely mapping instead
// of blind-scanning.
//
//   initiator (smaller ID) --offer{predicted_ports}--> responder
//   responder              --ready{predicted_ports}--> initiator
//   both run attemptHolePunch in an overlapping window; the targeted-spray phase
//   (stun.go) fires at PeerPredictedPorts.
//
// The handshake rides the relay signalling channel, so it works even in P2P-only
// rooms and before any E2E key is established (it is sent unencrypted, like the
// punch packets themselves).

// PunchSync is exchanged over the relay to coordinate a simultaneous punch.
type PunchSync struct {
	Phase          string `json:"phase"`           // "offer" | "ready"
	Nonce          string `json:"nonce"`           // correlates an offer/ready pair (diagnostics)
	PredictedPorts []int  `json:"predicted_ports"` // where MY NAT will likely map next
	PortModel      string `json:"port_model"`      // global_counter | sequential | random | ""
}

// coordThrottle bounds how often we send a coordination offer to a given peer.
const coordThrottle = 18 * time.Second

// natIsHard reports whether punching this peer is a hard case (symmetric on either
// side) and therefore worth the coordination handshake.
func (c *Client) natIsHard(peerNAT string) bool {
	return c.natType == NATSymmetric || peerNAT == NATSymmetric
}

// electInitiator returns true if we drive the coordination handshake. Using the
// lexicographically-smaller ID guarantees exactly one initiator per pair.
func (c *Client) electInitiator(peerID string) bool {
	return c.MyID < peerID
}

// portModelName returns the current NAT port-allocation model name ("" if unknown).
func (c *Client) portModelName() string {
	c.peerConnsMu.RLock()
	defer c.peerConnsMu.RUnlock()
	if c.portModel != nil {
		return c.portModel.Name
	}
	return ""
}

// myPredictedPorts computes the external ports our NAT is likely to use next, from
// the observed public port and the port-allocation model. Bounded to ~64 entries.
func (c *Client) myPredictedPorts() []int {
	base := 0
	if c.publicAddr != "" {
		if _, ps, err := net.SplitHostPort(c.publicAddr); err == nil {
			fmt.Sscanf(ps, "%d", &base)
		}
	}

	c.peerConnsMu.RLock()
	model := c.portModel
	c.peerConnsMu.RUnlock()

	seen := map[int]bool{}
	var ports []int
	add := func(p int) {
		if p > 1024 && p < 65535 && !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	add(base) // a cone NAT keeps this stable; share it as the primary candidate

	if model != nil {
		switch model.Name {
		case "global_counter":
			elapsed := time.Since(model.AnchorAt).Seconds()
			predicted := model.Anchor + int(model.DriftRate*elapsed)
			for d := -8; d <= 56 && len(ports) < 64; d++ {
				add(predicted + d)
			}
		case "sequential":
			step := model.Delta
			if step <= 0 {
				step = 1
			}
			for i := 1; i <= 40 && len(ports) < 64; i++ {
				add(model.Anchor + i*step)
			}
		default: // random — unpredictable; share the anchor neighbourhood only
			for d := -4; d <= 4; d++ {
				add(model.Anchor + d)
			}
		}
	}
	return ports
}

// maybeCoordinatePunch sends a coordination offer to a hard-NAT peer if we are the
// elected initiator and have not done so recently. Throttled per peer.
func (c *Client) maybeCoordinatePunch(peerID string) {
	c.peerConnsMu.RLock()
	pc := c.peerConns[peerID]
	var peerNAT string
	if pc != nil {
		peerNAT = pc.NATType
	}
	c.peerConnsMu.RUnlock()
	if pc == nil || !c.natIsHard(peerNAT) || !c.electInitiator(peerID) {
		return
	}

	c.peerConnsMu.Lock()
	if !pc.lastCoord.IsZero() && time.Since(pc.lastCoord) < coordThrottle {
		c.peerConnsMu.Unlock()
		return
	}
	pc.lastCoord = time.Now()
	c.peerConnsMu.Unlock()

	c.sendRelay(peerID, "punch_sync", PunchSync{
		Phase:          "offer",
		Nonce:          generateTunnelID(),
		PredictedPorts: c.myPredictedPorts(),
		PortModel:      c.portModelName(),
	})
	if c.verbose {
		c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
			"Punch coord: offer → %s (hard NAT, simultaneous open)", shortID(peerID))})
	}
}

// handlePunchSync processes a coordination message and fires a synchronized punch.
func (c *Client) handlePunchSync(msg Message) {
	if msg.From == "" || msg.From == c.MyID {
		return
	}
	var ps PunchSync
	if err := json.Unmarshal(msg.Payload, &ps); err != nil {
		return
	}

	// Record the peer's predicted ports so attemptHolePunch can target them.
	c.peerConnsMu.Lock()
	pc := c.peerConns[msg.From]
	if pc == nil {
		c.peerConnsMu.Unlock()
		return
	}
	if len(ps.PredictedPorts) > 0 {
		pc.PeerPredictedPorts = clampPorts(ps.PredictedPorts)
	}
	alreadyDirect := pc.Mode == "direct" && pc.directHealthy
	c.peerConnsMu.Unlock()

	if alreadyDirect {
		return // nothing to coordinate
	}

	switch ps.Phase {
	case "offer":
		// Responder: reply ready, then fire immediately for an overlapping window.
		c.sendRelay(msg.From, "punch_sync", PunchSync{
			Phase:          "ready",
			Nonce:          ps.Nonce,
			PredictedPorts: c.myPredictedPorts(),
			PortModel:      c.portModelName(),
		})
		if c.verbose {
			c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
				"Punch coord: ready → %s, firing simultaneous punch", shortID(msg.From))})
		}
		go c.attemptHolePunch(msg.From, false)
	case "ready":
		// Initiator: peer is primed, fire now.
		if c.verbose {
			c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
				"Punch coord: %s ready, firing simultaneous punch", shortID(msg.From))})
		}
		go c.attemptHolePunch(msg.From, false)
	}
}

// clampPorts drops out-of-range ports and caps the slice to a sane size, so a
// malicious or buggy peer can't make us spray the whole port space.
func clampPorts(in []int) []int {
	out := make([]int, 0, len(in))
	for _, p := range in {
		if p > 1024 && p < 65535 {
			out = append(out, p)
		}
		if len(out) >= 128 {
			break
		}
	}
	return out
}
