package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
)

// RelaySession represents a relay connection between two peers
type RelaySession struct {
	PeerA string
	PeerB string
	Room  string
}

// RelayManager handles relay sessions
type RelayManager struct {
	sessions map[string]*RelaySession
	mu       sync.RWMutex
}

func newRelayManager() *RelayManager {
	return &RelayManager{
		sessions: make(map[string]*RelaySession),
	}
}

// sessionKey returns a canonical key for a pair of peers (order-independent)
func sessionKey(peerA, peerB string) string {
	if peerA < peerB {
		return fmt.Sprintf("%s:%s", peerA, peerB)
	}
	return fmt.Sprintf("%s:%s", peerB, peerA)
}

func (rm *RelayManager) createSession(peerA, peerB, room string) string {
	key := sessionKey(peerA, peerB)

	rm.mu.Lock()
	defer rm.mu.Unlock()

	if _, exists := rm.sessions[key]; exists {
		return key
	}

	rm.sessions[key] = &RelaySession{
		PeerA: peerA,
		PeerB: peerB,
		Room:  room,
	}
	log.Printf("Relay session created: %s (room: %s)", key, room)
	return key
}

func (rm *RelayManager) removeSession(sessionID string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if _, ok := rm.sessions[sessionID]; ok {
		delete(rm.sessions, sessionID)
		log.Printf("Relay session removed: %s", sessionID)
	}
}

func (rm *RelayManager) relayData(from string, to string, data json.RawMessage, hub *Hub) {
	sender := hub.findClient(from)
	target := hub.findClient(to)
	if target == nil || sender == nil {
		return
	}

	// Security: only allow relay between clients in the same room
	if sender.roomKey == "" || sender.roomKey != target.roomKey {
		log.Printf("Relay blocked: %s and %s not in same room", from, to)
		return
	}

	dataLen := int64(len(data))
	hub.mu.RLock()
	if room, ok := hub.rooms[target.roomKey]; ok {
		atomic.AddInt64(&room.BytesRelayed, dataLen)
	}
	hub.mu.RUnlock()

	msg := Message{
		Type:    "relay_data",
		From:    from,
		To:      to,
		Payload: data,
	}

	encoded, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Relay: error marshaling data from %s to %s: %v", from, to, err)
		return
	}

	select {
	case target.send <- encoded:
	default:
		log.Printf("Relay: target %s send buffer full, dropping relay_data from %s", to, from)
	}
}
