package main

import (
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 256 * 1024 // 256KB for tunnel data
)

// Client represents a connected WebSocket peer
type Client struct {
	hub      *Hub
	conn     *websocket.Conn
	send     chan []byte
	id       string
	room     string   // display room name
	roomKey  string   // internal key: "name" or "name:hash"
	status   string   // "connecting", "direct", "relay"
	name     string   // friendly name from CLI
	services []string // advertised host:port list
	endpoint string   // STUN-discovered public UDP endpoint
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Client %s read error: %v", c.id, err)
			}
			break
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("Client %s invalid message: %v", c.id, err)
			continue
		}

		msg.From = c.id
		c.handleMessage(msg)
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(msg)

			// Drain any queued messages into the same write
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write([]byte("\n"))
				w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) handleMessage(msg Message) {
	switch msg.Type {
	case "join":
		c.handleJoin(msg)

	case "leave":
		c.handleLeave()

	case "offer", "answer", "ice-candidate":
		c.forwardToTarget(msg)

	case "stun_info":
		// Store sender's STUN endpoint
		if msg.Payload != nil {
			var info struct {
				Addr string `json:"addr"`
			}
			json.Unmarshal(msg.Payload, &info)
			if info.Addr != "" {
				c.endpoint = info.Addr
			}
		}
		if msg.To == "" {
			c.broadcastToRoom(msg)
		} else {
			c.forwardToTarget(msg)
		}

	case "open_tunnel", "tunnel_data", "close_tunnel", "tunnel_opened", "tunnel_error", "tunnel_rejected":
		c.forwardToTarget(msg)

	case "status_update":
		c.handleStatusUpdate(msg)

	case "relay_data":
		c.handleRelayData(msg)

	default:
		log.Printf("Client %s unknown message type: %s", c.id, msg.Type)
	}
}

func (c *Client) handleJoin(msg Message) {
	roomName := msg.Room
	var passwordHash string

	if msg.Payload != nil {
		var payload struct {
			Room         string   `json:"room"`
			PasswordHash string   `json:"password_hash"`
			Name         string   `json:"name"`
			Services     []string `json:"services"`
		}
		json.Unmarshal(msg.Payload, &payload)
		if payload.Room != "" {
			roomName = payload.Room
		}
		passwordHash = payload.PasswordHash
		if payload.Name != "" {
			c.name = payload.Name
		}
		if payload.Services != nil {
			c.services = payload.Services
		}
	}

	if roomName == "" {
		c.sendError("room name required")
		return
	}

	// Rate limit join attempts
	if !joinLimiter.allow(c.id) {
		c.sendError("too many join attempts, please wait. Repeated attempts may result in IP ban.")
		log.Printf("Client %s join rate limited", c.id)
		return
	}

	if c.roomKey != "" {
		c.handleLeave()
	}

	// Rooms must be created via dashboard — no auto-create
	room, reason := c.hub.findRoomByName(roomName, passwordHash)
	if room == nil {
		c.sendError(reason + ". Repeated failed attempts may result in IP ban.")
		log.Printf("Client %s join rejected: %s (room: %s)", c.id, reason, roomName)
		return
	}

	if room.IsBanned(c.id) {
		c.sendError("you are banned from this room")
		log.Printf("Client %s banned from room %s", c.id, roomName)
		return
	}

	c.room = roomName
	c.roomKey = room.Key
	c.status = "connecting"

	room.mu.Lock()
	room.Clients[c.id] = c
	room.mu.Unlock()

	log.Printf("Client %s (%s) joined room %s", c.id, c.name, roomName)
	room.broadcastPeerList()
}

func (c *Client) sendError(msg string) {
	payload, _ := json.Marshal(map[string]string{"error": msg})
	m := Message{Type: "error", Payload: json.RawMessage(payload)}
	data, _ := json.Marshal(m)
	select {
	case c.send <- data:
	default:
	}
}

func (c *Client) handleLeave() {
	if c.roomKey == "" {
		return
	}
	c.hub.removeClientFromRoom(c)
}

func (c *Client) forwardToTarget(msg Message) {
	if msg.To == "" {
		log.Printf("Client %s forwarding %s with no target", c.id, msg.Type)
		return
	}

	target := c.hub.findClient(msg.To)
	if target == nil {
		log.Printf("Client %s target %s not found for %s", c.id, msg.To, msg.Type)
		return
	}

	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling forward message: %v", err)
		return
	}

	select {
	case target.send <- data:
	default:
		log.Printf("Target %s send buffer full, dropping %s", msg.To, msg.Type)
	}
}

func (c *Client) handleStatusUpdate(msg Message) {
	if msg.Payload == nil {
		return
	}

	// Client may send status as raw string "direct" or object {"status":"direct"}
	var status string
	if err := json.Unmarshal(msg.Payload, &status); err != nil {
		var payload struct {
			Status string `json:"status"`
		}
		if err2 := json.Unmarshal(msg.Payload, &payload); err2 != nil {
			log.Printf("Client %s invalid status_update payload: %v", c.id, err)
			return
		}
		status = payload.Status
	}

	switch status {
	case "connecting", "direct", "relay":
		c.status = status
	default:
		log.Printf("Client %s unknown status: %s", c.id, status)
		return
	}

	if c.roomKey == "" {
		return
	}

	c.hub.mu.RLock()
	room, ok := c.hub.rooms[c.roomKey]
	c.hub.mu.RUnlock()

	if ok {
		room.broadcastPeerList()
	}
}

func (c *Client) handleRelayData(msg Message) {
	if msg.To == "" {
		log.Printf("Client %s relay_data with no target", c.id)
		return
	}
	relayManager.relayData(c.id, msg.To, msg.Payload, c.hub)
}

func (c *Client) broadcastToRoom(msg Message) {
	if c.roomKey == "" {
		return
	}
	c.hub.mu.RLock()
	room, ok := c.hub.rooms[c.roomKey]
	c.hub.mu.RUnlock()
	if !ok {
		return
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	room.broadcast(data, c.id)
}
