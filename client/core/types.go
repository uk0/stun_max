package core

import (
	"encoding/json"
	"net"
	"sync"
	"time"
)

// ANSI color codes (for log formatting)
const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorCyan   = "\033[36m"
	ColorGray   = "\033[90m"
	ColorBold   = "\033[1m"
)

// STUN constants
const (
	StunMagicCookie    = 0x2112A442
	StunBindingRequest = 0x0001
	StunAttrXorMapped  = 0x0020
	StunHeaderSize     = 20
	StunTimeout        = 3 * time.Second
)

// Message matches the server's Message struct
type Message struct {
	Type    string          `json:"type"`
	From    string          `json:"from,omitempty"`
	To      string          `json:"to,omitempty"`
	Room    string          `json:"room,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// PeerInfo matches the server's PeerInfo struct
type PeerInfo struct {
	ID       string   `json:"id"`
	Status   string   `json:"status"`
	Name     string   `json:"name,omitempty"`
	Services []string `json:"services,omitempty"`
	Endpoint string   `json:"endpoint,omitempty"`
}

// PeerConn tracks per-peer connection state for hole punching.
type PeerConn struct {
	PeerID     string
	Mode       string       // "connecting", "direct", "relay"
	UDPAddr    *net.UDPAddr // peer's public UDP address
	UDPConn    *net.UDPConn // shared UDP socket
	DirectTCP  net.Conn     // direct TCP connection (after hole punch upgrade)
	LastPunch  time.Time
	PunchFails int          // consecutive punch failures
	Crypto     *PeerCrypto  // encryption state
}

// TunnelOpen is sent to request opening a tunnel to a peer's local port.
type TunnelOpen struct {
	TunnelID   string `json:"tunnel_id"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
}

// TunnelData carries base64-encoded TCP data through the signaling channel.
type TunnelData struct {
	TunnelID string `json:"tunnel_id"`
	Data     string `json:"data"` // base64 encoded
}

// TunnelClose signals that a tunnel should be torn down.
type TunnelClose struct {
	TunnelID string `json:"tunnel_id"`
}

// RelayEnvelope wraps tunnel messages inside relay_data payloads.
type RelayEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Forward represents a local listener forwarding to a remote peer's host:port.
type Forward struct {
	LocalPort  int
	RemoteHost string
	RemotePort int
	PeerID     string
	PeerName   string
	Listener   net.Listener
	Mu         sync.Mutex
	ConnCount  int
	Cancel     chan struct{}
	ForceRelay bool  // user forced relay mode (only when P2P has issues)
	BytesUp    int64 // atomic: bytes sent to peer
	BytesDown  int64 // atomic: bytes received from peer
	LastUp     int64 // atomic: snapshot for rate calc
	LastDown   int64 // atomic: snapshot for rate calc
}

// TunnelConn tracks a single tunnel connection (one TCP connection).
type TunnelConn struct {
	TunnelID string
	PeerID   string // which peer this tunnel connects to
	Conn     net.Conn
	Forward  *Forward
	Done     chan struct{}
}

// TunnelRejected is sent when a tunnel request is denied.
type TunnelRejected struct {
	TunnelID string `json:"tunnel_id"`
	Reason   string `json:"reason"`
}

// ReverseForwardOffer is sent by B to ask A to open a local listener
// that tunnels back to B's sourceHost:sourcePort.
type ReverseForwardOffer struct {
	OfferID    string `json:"offer_id"`
	SourceHost string `json:"source_host"` // B's local target host
	SourcePort int    `json:"source_port"` // B's local target port
	TargetPort int    `json:"target_port"` // requested listener port on A
}

// ReverseForwardAccept is sent by A to confirm the reverse forward is active.
type ReverseForwardAccept struct {
	OfferID    string `json:"offer_id"`
	TargetPort int    `json:"target_port"` // confirmed listener port on A
}

// ReverseForwardReject is sent by A when it cannot honour the offer.
type ReverseForwardReject struct {
	OfferID string `json:"offer_id"`
	Reason  string `json:"reason"`
}

// HopForwardRequest is sent by A to B asking B to bridge to C:host:port.
type HopForwardRequest struct {
	HopID        string `json:"hop_id"`
	TargetPeerID string `json:"target_peer_id"`
	TargetHost   string `json:"target_host"`
	TargetPort   int    `json:"target_port"`
}

// HopForwardAccept is sent by B back to A confirming the bridge is ready.
// InboundTunnelID is the tunnel ID A must use when sending tunnel_data to B.
type HopForwardAccept struct {
	HopID           string `json:"hop_id"`
	InboundTunnelID string `json:"inbound_tunnel_id"` // A uses this to send data to B
}

// HopForwardReject is sent by B when it cannot set up the bridge.
type HopForwardReject struct {
	HopID  string `json:"hop_id"`
	Reason string `json:"reason"`
}

// SpeedTestRequest initiates a speed test with a peer.
type SpeedTestRequest struct {
	TestID    string `json:"test_id"`
	Direction string `json:"direction"` // "upload" or "download"
	Size      int64  `json:"size"`
}

// SpeedTestData carries speed test payload chunks.
type SpeedTestData struct {
	TestID string `json:"test_id"`
	Data   string `json:"data"` // base64
	Seq    int    `json:"seq"`
}

// SpeedTestDone signals completion of one phase of a speed test.
type SpeedTestDone struct {
	TestID     string `json:"test_id"`
	Bytes      int64  `json:"bytes"`
	DurationMs int64  `json:"duration_ms"`
}

// SpeedTestResult is the final result emitted via the event channel.
type SpeedTestResult struct {
	TestID       string
	PeerID       string
	UploadMbps   float64
	DownloadMbps float64
	Done         bool
	Error        string
}

// FileOffer is sent to propose a file transfer to a peer.
type FileOffer struct {
	TransferID string `json:"transfer_id"`
	FileName   string `json:"file_name"`
	FileSize   int64  `json:"file_size"`
	FileHash   string `json:"file_hash"`
}

// FileAccept is sent to accept a pending file offer.
type FileAccept struct {
	TransferID string `json:"transfer_id"`
}

// FileReject is sent to reject a pending file offer.
type FileReject struct {
	TransferID string `json:"transfer_id"`
	Reason     string `json:"reason"`
}

// FileData carries a compressed, base64-encoded chunk of file data.
type FileData struct {
	TransferID string `json:"transfer_id"`
	Data       string `json:"data"`
	Seq        int    `json:"seq"`
	Offset     int64  `json:"offset"`
}

// FileDone signals that all chunks have been sent.
type FileDone struct {
	TransferID string `json:"transfer_id"`
	TotalBytes int64  `json:"total_bytes"`
}

// FileCancel signals that a transfer has been cancelled.
type FileCancel struct {
	TransferID string `json:"transfer_id"`
	Reason     string `json:"reason"`
}

// FileTransferInfo is a read-only snapshot of a file transfer for the GUI.
type FileTransferInfo struct {
	TransferID string
	PeerID     string
	PeerName   string
	FileName   string
	FileSize   int64
	BytesDone  int64
	Direction  string  // "send" or "receive"
	Progress   float64 // 0.0 to 1.0
	Speed      float64 // bytes/sec
	Status     string  // "pending", "active", "complete", "error"
}

// TunSetup is exchanged between peers to negotiate VPN.
type TunSetup struct {
	VirtualIP string   `json:"virtual_ip"`
	PeerIP    string   `json:"peer_ip"`
	Subnet    string   `json:"subnet"`
	Routes    []string `json:"routes,omitempty"` // subnets to route, e.g. ["10.88.51.0/24"]
}

// TunData carries a raw IP packet through the tunnel.
type TunData struct {
	Data string `json:"data"`
}

// TunTeardown signals VPN shutdown.
type TunTeardown struct{}

// TunInfo is a read-only snapshot of TUN state for the GUI.
type TunInfo struct {
	Enabled   bool
	VirtualIP string
	PeerIP    string
	Subnet    string
	Routes    []string
	PeerID    string
	PeerName  string
	BytesUp   int64
	BytesDown int64
	RateUp    float64
	RateDown  float64
}

// ClientConfig holds all configuration needed to create a Client.
type ClientConfig struct {
	ServerURL   string
	Room        string
	Password    string
	Name        string
	STUNServers []string
	NoSTUN      bool
	Verbose     bool
}

// ForwardInfo is a read-only snapshot of a Forward for the GUI.
type ForwardInfo struct {
	LocalPort  int
	RemoteHost string
	RemotePort int
	PeerID     string
	PeerName   string
	Mode       string // "P2P" or "RELAY"
	ForceRelay bool   // user manually forced relay
	ConnCount  int
	BytesUp    int64   // total bytes uploaded
	BytesDown  int64   // total bytes downloaded
	RateUp     float64 // bytes/sec upload
	RateDown   float64 // bytes/sec download
}

// StunInfo is a read-only snapshot of STUN state for the GUI.
type StunInfo struct {
	PublicAddr string
	Enabled    bool
	PeerConns  map[string]string // peerID -> mode
}
