package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true }, // CLI clients need this
}

var (
	hub          *Hub
	relayManager *RelayManager
	authToken    string
	sessions     sync.Map
	loginLimiter = newRateLimiter(5, time.Minute) // 5 attempts per minute per IP
	wsLimiter    = newRateLimiter(20, time.Minute) // 20 new WS connections per minute per IP
)

// --- Simple rate limiter ---

type rateLimiter struct {
	counts map[string][]time.Time
	max    int
	window time.Duration
	mu     sync.Mutex
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{counts: make(map[string][]time.Time), max: max, window: window}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-rl.window)
	// Clean old entries
	var valid []time.Time
	for _, t := range rl.counts[key] {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	if len(valid) >= rl.max {
		rl.counts[key] = valid
		return false
	}
	rl.counts[key] = append(valid, now)
	return true
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func generateToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.Split(fwd, ",")[0]
	}
	return strings.Split(r.RemoteAddr, ":")[0]
}

// --- Session auth with expiry ---

const sessionMaxAge = 24 * time.Hour

func createSession() string {
	token := generateToken(32)
	sessions.Store(token, time.Now())
	return token
}

func validSession(token string) bool {
	if token == "" {
		return false
	}
	v, ok := sessions.Load(token)
	if !ok {
		return false
	}
	created := v.(time.Time)
	if time.Since(created) > sessionMaxAge {
		sessions.Delete(token)
		return false
	}
	return true
}

func getSessionToken(r *http.Request) string {
	if c, err := r.Cookie("stun_max_token"); err == nil {
		return c.Value
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validSession(getSessionToken(r)) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// --- Handlers ---

func serveWs(w http.ResponseWriter, r *http.Request) {
	if !wsLimiter.allow(clientIP(r)) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	clientID := generateID()
	client := &Client{
		hub:  hub,
		conn: conn,
		send: make(chan []byte, 1024),
		id:   clientID,
		status: "connecting",
	}

	hub.register <- client

	welcomePayload, _ := json.Marshal(map[string]string{"id": clientID})
	welcome := Message{Type: "welcome", Payload: json.RawMessage(welcomePayload)}
	welcomeData, _ := json.Marshal(welcome)
	client.send <- welcomeData

	go client.writePump()
	go client.readPump()
}

func apiLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	ip := clientIP(r)
	if !loginLimiter.allow(ip) {
		log.Printf("Login rate limited: %s", ip)
		http.Error(w, `{"error":"too many attempts, try later"}`, http.StatusTooManyRequests)
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	if req.Password != authToken {
		log.Printf("Login failed from %s", ip)
		http.Error(w, `{"error":"invalid password"}`, http.StatusUnauthorized)
		return
	}

	token := createSession()
	http.SetCookie(w, &http.Cookie{
		Name:     "stun_max_token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   86400,
		SameSite: http.SameSiteLaxMode,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": token})
}

func apiRooms(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rooms := hub.getRoomsInfo()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rooms)

	case http.MethodPost:
		var req struct {
			Name     string `json:"name"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" || len(req.Name) > 128 {
			http.Error(w, `{"error":"invalid room name"}`, http.StatusBadRequest)
			return
		}

		passHash := ""
		if req.Password != "" {
			h := sha256.Sum256([]byte(req.Password))
			passHash = hex.EncodeToString(h[:])
		}

		hub.getOrCreateRoom(req.Name, passHash)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"name": req.Name, "protected": req.Password != ""})

	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, `{"error":"room name required"}`, http.StatusBadRequest)
			return
		}
		deleted := hub.deleteRoom(name)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"deleted": deleted})

	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func apiAuthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func apiBan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Room     string `json:"room"`
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Room == "" || req.ClientID == "" {
		http.Error(w, `{"error":"room and client_id required"}`, http.StatusBadRequest)
		return
	}
	ok := hub.banClient(req.Room, req.ClientID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": ok})
}

func apiUnban(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Room     string `json:"room"`
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Room == "" || req.ClientID == "" {
		http.Error(w, `{"error":"room and client_id required"}`, http.StatusBadRequest)
		return
	}
	ok := hub.unbanClient(req.Room, req.ClientID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": ok})
}

func apiStats(w http.ResponseWriter, r *http.Request) {
	stats := hub.getStats()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func serveStatic(webDir string) http.HandlerFunc {
	fs := http.FileServer(http.Dir(webDir))
	return func(w http.ResponseWriter, r *http.Request) {
		fs.ServeHTTP(w, r)
	}
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	webPass := flag.String("web-password", "", "dashboard password (empty=auto-generate)")
	webDir := flag.String("web-dir", "../web", "web static files path")
	flag.Parse()

	hub = newHub()
	go hub.run()
	relayManager = newRelayManager()

	if *webPass != "" {
		authToken = *webPass
	} else {
		authToken = generateToken(16)
	}

	fmt.Println("═══════════════════════════════════════")
	fmt.Println("  STUN Max Server")
	fmt.Println("═══════════════════════════════════════")
	fmt.Printf("  Listen:     %s\n", *addr)
	fmt.Printf("  Password:   %s\n", authToken)
	fmt.Println("═══════════════════════════════════════")

	http.HandleFunc("/ws", serveWs)
	http.HandleFunc("/api/login", apiLogin)
	http.HandleFunc("/api/rooms", requireAuth(apiRooms))
	http.HandleFunc("/api/rooms/ban", requireAuth(apiBan))
	http.HandleFunc("/api/rooms/unban", requireAuth(apiUnban))
	http.HandleFunc("/api/auth", requireAuth(apiAuthCheck))
	http.HandleFunc("/api/stats", requireAuth(apiStats))
	http.HandleFunc("/", serveStatic(*webDir))

	log.Printf("Server starting on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
