package core

// EventType identifies the kind of event emitted by the Client.
type EventType int

const (
	EventConnected EventType = iota
	EventDisconnected
	EventJoinedRoom
	EventPeerListUpdated
	EventPeerJoined
	EventPeerLeft
	EventForwardStarted
	EventForwardStopped
	EventTunnelOpened
	EventTunnelClosed
	EventTunnelRejected
	EventStunDiscovered
	EventHolePunchSuccess
	EventSpeedTestProgress
	EventSpeedTestResult
	EventLog
	EventError
)

// Event is the unit of communication from Client to the GUI layer.
type Event struct {
	Type EventType
	Data interface{}
}

// LogEvent carries a log message with severity level.
type LogEvent struct {
	Level   string // "info", "warn", "error"
	Message string
}

// PeerEvent describes a peer join/leave/update.
type PeerEvent struct {
	ID     string
	Name   string
	Status string
}

// ForwardEvent describes a forward start/stop.
type ForwardEvent struct {
	LocalPort  int
	RemoteHost string
	RemotePort int
	PeerName   string
	Mode       string
}

// SpeedTestProgressEvent reports progress during a speed test.
type SpeedTestProgressEvent struct {
	TestID   string
	PeerID   string
	Phase    string  // "upload" or "download"
	Progress float64 // 0.0 to 1.0
}
