package main

import (
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Message types for signaling
type Message struct {
	Type    string          `json:"type"`
	From    string          `json:"from,omitempty"`
	To      string          `json:"to,omitempty"`
	Room    string          `json:"room,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// PeerInfo represents a peer's status visible to others
type PeerInfo struct {
	ID       string   `json:"id"`
	Status   string   `json:"status"`             // "connecting", "direct", "relay", "disconnected"
	Name     string   `json:"name,omitempty"`     // friendly name from CLI
	Services []string `json:"services,omitempty"` // advertised host:port list
	Endpoint string   `json:"endpoint,omitempty"` // STUN-discovered public UDP endpoint
}

// RoomInfo is the API representation of a room for the web dashboard
type RoomInfo struct {
	Name         string     `json:"name"`
	Protected    bool       `json:"protected"`
	Peers        []PeerInfo `json:"peers"`
	BytesRelayed int64      `json:"bytes_relayed"`
	CreatedAt    string     `json:"created_at,omitempty"`
	Blacklist    []string   `json:"blacklist,omitempty"`
}

// Room holds clients in a room, keyed by name:passwordHash
type Room struct {
	Name         string // display name
	PasswordHash string // sha256 hex of password, empty = no password
	Key          string // internal key: "name" or "name:hash"
	Clients      map[string]*Client
	Blacklist    map[string]bool // banned client IDs
	mu           sync.RWMutex
	BytesRelayed int64     // atomic counter
	CreatedAt    time.Time
}

// Hub manages all rooms and clients
type Hub struct {
	rooms      map[string]*Room
	mu         sync.RWMutex
	register   chan *Client
	unregister chan *Client
}

func newHub() *Hub {
	return &Hub{
		rooms:      make(map[string]*Room),
		register:   make(chan *Client, 64),
		unregister: make(chan *Client, 64),
	}
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			log.Printf("Client registered: %s", client.id)
		case client := <-h.unregister:
			h.removeClientFromRoom(client)
			log.Printf("Client unregistered: %s", client.id)
		}
	}
}

// roomKey builds the internal room key from name + passwordHash
func roomKey(name, passwordHash string) string {
	if passwordHash == "" {
		return name
	}
	return name + ":" + passwordHash
}

// findRoomByName checks if a room with the given name exists and password matches.
// Returns the room if found and password correct, or (nil, reason) if not.
func (h *Hub) findRoomByName(name, passwordHash string) (*Room, string) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// Look for any room with this display name
	found := false
	for _, room := range h.rooms {
		if room.Name == name {
			found = true
			// Check password matches
			if room.PasswordHash == passwordHash {
				return room, ""
			}
		}
	}
	if found {
		return nil, "incorrect room password"
	}
	return nil, "room does not exist"
}

func (h *Hub) getOrCreateRoom(name, passwordHash string) *Room {
	key := roomKey(name, passwordHash)

	h.mu.Lock()
	defer h.mu.Unlock()

	if room, ok := h.rooms[key]; ok {
		return room
	}
	room := &Room{
		Name:         name,
		PasswordHash: passwordHash,
		Key:          key,
		Clients:      make(map[string]*Client),
		Blacklist:    make(map[string]bool),
		CreatedAt:    time.Now(),
	}
	h.rooms[key] = room
	log.Printf("Room created: %s (protected: %v)", name, passwordHash != "")
	return room
}

func (h *Hub) removeClientFromRoom(client *Client) {
	if client.roomKey == "" {
		return
	}

	h.mu.RLock()
	room, ok := h.rooms[client.roomKey]
	h.mu.RUnlock()

	if !ok {
		return
	}

	room.mu.Lock()
	delete(room.Clients, client.id)
	empty := len(room.Clients) == 0
	room.mu.Unlock()

	if empty {
		h.mu.Lock()
		delete(h.rooms, client.roomKey)
		h.mu.Unlock()
		log.Printf("Room removed (empty): %s", client.roomKey)
	} else {
		room.broadcastPeerList()
	}

	client.room = ""
	client.roomKey = ""
}

// findClient looks up a client by ID across all rooms
func (h *Hub) findClient(id string) *Client {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, room := range h.rooms {
		room.mu.RLock()
		c, ok := room.Clients[id]
		room.mu.RUnlock()
		if ok {
			return c
		}
	}
	return nil
}

func (r *Room) broadcast(msg []byte, exclude string) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for id, client := range r.Clients {
		if id == exclude {
			continue
		}
		select {
		case client.send <- msg:
		default:
			log.Printf("Client %s send buffer full, dropping message", id)
		}
	}
}

func (r *Room) broadcastPeerList() {
	peers := r.getPeerList()

	payload, err := json.Marshal(peers)
	if err != nil {
		log.Printf("Error marshaling peer list: %v", err)
		return
	}

	msg := Message{
		Type:    "peer_list",
		Payload: json.RawMessage(payload),
	}

	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling peer_list message: %v", err)
		return
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, client := range r.Clients {
		select {
		case client.send <- data:
		default:
			log.Printf("Client %s send buffer full, dropping peer_list", client.id)
		}
	}
}

func (r *Room) getPeerList() []PeerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	peers := make([]PeerInfo, 0, len(r.Clients))
	for _, c := range r.Clients {
		peers = append(peers, PeerInfo{
			ID:       c.id,
			Status:   c.status,
			Name:     c.name,
			Services: c.services,
			Endpoint: c.endpoint,
		})
	}
	return peers
}

// getRoomsInfo returns a snapshot of all rooms for the web dashboard API
func (h *Hub) getRoomsInfo() []RoomInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result := make([]RoomInfo, 0, len(h.rooms))
	for _, room := range h.rooms {
		room.mu.RLock()
		banned := make([]string, 0, len(room.Blacklist))
		for id := range room.Blacklist {
			banned = append(banned, id)
		}
		room.mu.RUnlock()

		result = append(result, RoomInfo{
			Name:         room.Name,
			Protected:    room.PasswordHash != "",
			Peers:        room.getPeerList(),
			BytesRelayed: atomic.LoadInt64(&room.BytesRelayed),
			CreatedAt:    room.CreatedAt.UTC().Format(time.RFC3339),
			Blacklist:    banned,
		})
	}
	return result
}

// deleteRoom removes a room by display name (disconnects all clients in it)
func (h *Hub) deleteRoom(name string) bool {
	h.mu.Lock()
	var toDelete []string
	for key, room := range h.rooms {
		if room.Name == name {
			toDelete = append(toDelete, key)
			room.mu.Lock()
			for _, c := range room.Clients {
				close(c.send)
			}
			room.mu.Unlock()
		}
	}
	for _, key := range toDelete {
		delete(h.rooms, key)
	}
	h.mu.Unlock()

	if len(toDelete) > 0 {
		log.Printf("Room deleted via dashboard: %s (%d keys)", name, len(toDelete))
	}
	return len(toDelete) > 0
}

// IsBanned checks if a client ID is banned from this room
func (r *Room) IsBanned(clientID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.Blacklist[clientID]
}

// banClient adds a client to the room blacklist and disconnects them if present
func (h *Hub) banClient(roomName, clientID string) bool {
	h.mu.RLock()
	var targetRoom *Room
	for _, room := range h.rooms {
		if room.Name == roomName {
			targetRoom = room
			break
		}
	}
	h.mu.RUnlock()

	if targetRoom == nil {
		return false
	}

	targetRoom.mu.Lock()
	targetRoom.Blacklist[clientID] = true
	client, connected := targetRoom.Clients[clientID]
	targetRoom.mu.Unlock()

	log.Printf("Client %s banned from room %s", clientID, roomName)

	if connected {
		// Send banned error then close
		errPayload, _ := json.Marshal(map[string]string{"error": "banned from this room"})
		errMsg := Message{Type: "error", Payload: json.RawMessage(errPayload)}
		data, _ := json.Marshal(errMsg)
		select {
		case client.send <- data:
		default:
		}
		close(client.send)
	}
	return true
}

// unbanClient removes a client from the room blacklist
func (h *Hub) unbanClient(roomName, clientID string) bool {
	h.mu.RLock()
	var targetRoom *Room
	for _, room := range h.rooms {
		if room.Name == roomName {
			targetRoom = room
			break
		}
	}
	h.mu.RUnlock()

	if targetRoom == nil {
		return false
	}

	targetRoom.mu.Lock()
	delete(targetRoom.Blacklist, clientID)
	targetRoom.mu.Unlock()

	log.Printf("Client %s unbanned from room %s", clientID, roomName)
	return true
}
