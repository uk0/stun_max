package ui

import (
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget/material"

	"stun_max/client/core"
)

// Screen identifies which screen is active.
type Screen int

const (
	ScreenConnect Screen = iota
	ScreenDashboard
)

// LogEntry is a single log line displayed in the UI.
type LogEntry struct {
	Time    string
	Level   string
	Message string
}

// App is the top-level application state.
type App struct {
	Window *app.Window
	Theme  *material.Theme
	Client *core.Client
	Screen Screen

	Connect   ConnectScreen
	Dashboard DashboardScreen

	Peers    []core.PeerInfo
	Forwards []core.ForwardInfo
	Logs     []LogEntry
	mu       sync.Mutex

	Error    string
	RoomName string

	// Saved forward rules (persist across stop/start)
	SavedForwards []SavedForward
}

// NewApp creates a new App with default state.
func NewApp() *App {
	return &App{
		Window: new(app.Window),
		Theme:  NewTheme(),
		Screen: ScreenConnect,
	}
}

// Run is the main event loop.
func (a *App) Run() error {
	a.Window.Option(
		app.Title("STUN Max"),
		app.Size(unit.Dp(900), unit.Dp(650)),
		app.MinSize(unit.Dp(700), unit.Dp(500)),
	)

	var ops op.Ops
	for {
		switch e := a.Window.Event().(type) {
		case app.DestroyEvent:
			if a.Client != nil {
				a.Client.Disconnect()
			}
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			a.layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

func (a *App) layout(gtx layout.Context) layout.Dimensions {
	// Fill background
	paint.FillShape(gtx.Ops, BgColor, clip.Rect{Max: gtx.Constraints.Max}.Op())

	switch a.Screen {
	case ScreenConnect:
		return a.Connect.Layout(gtx, a.Theme, a)
	case ScreenDashboard:
		return a.Dashboard.Layout(gtx, a.Theme, a)
	}
	return layout.Dimensions{Size: gtx.Constraints.Max}
}

// StartEventConsumer reads events from the core client and updates UI state.
func (a *App) StartEventConsumer() {
	go func() {
		for evt := range a.Client.Events() {
			a.handleEvent(evt)
			a.Window.Invalidate()
		}
	}()
	// Periodic refresh for forward traffic stats (1s interval)
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if a.Client == nil {
					return
				}
				a.mu.Lock()
				a.Forwards = a.Client.Forwards()
				a.mu.Unlock()
				a.Window.Invalidate()
			case <-a.Client.Done():
				return
			}
		}
	}()
}

func (a *App) handleEvent(evt core.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch evt.Type {
	case core.EventPeerListUpdated:
		if a.Client != nil {
			a.Peers = a.Client.Peers()
			a.Forwards = a.Client.Forwards()
		}
	case core.EventPeerJoined:
		if pe, ok := evt.Data.(core.PeerEvent); ok {
			a.addLog("info", "Peer joined: "+pe.Name+" ("+pe.ID[:8]+")")
		}
	case core.EventPeerLeft:
		if pe, ok := evt.Data.(core.PeerEvent); ok {
			a.addLog("info", "Peer left: "+pe.Name)
		}
	case core.EventForwardStarted:
		if a.Client != nil {
			a.Forwards = a.Client.Forwards()
		}
	case core.EventForwardStopped:
		if a.Client != nil {
			a.Forwards = a.Client.Forwards()
		}
	case core.EventTunnelRejected:
		if le, ok := evt.Data.(core.LogEvent); ok {
			a.addLog("error", le.Message)
		}
	case core.EventHolePunchSuccess:
		if le, ok := evt.Data.(core.LogEvent); ok {
			a.addLog("info", le.Message)
		}
	case core.EventStunDiscovered:
		if le, ok := evt.Data.(core.LogEvent); ok {
			a.addLog("info", le.Message)
		}
	case core.EventSpeedTestResult:
		if r, ok := evt.Data.(core.SpeedTestResult); ok {
			a.Dashboard.SpeedTest.handleResult(r)
		}
	case core.EventSpeedTestProgress:
		if p, ok := evt.Data.(core.SpeedTestProgressEvent); ok {
			a.Dashboard.SpeedTest.handleProgress(p)
		}
	case core.EventLog:
		if le, ok := evt.Data.(core.LogEvent); ok {
			a.addLog(le.Level, le.Message)
		}
	case core.EventError:
		if le, ok := evt.Data.(core.LogEvent); ok {
			a.addLog("error", le.Message)
			a.Error = le.Message
		}
	}
}

func (a *App) addLog(level, msg string) {
	entry := LogEntry{
		Time:    time.Now().Format("15:04:05"),
		Level:   level,
		Message: msg,
	}
	a.Logs = append(a.Logs, entry)
	if len(a.Logs) > 500 {
		a.Logs = a.Logs[len(a.Logs)-500:]
	}
}

// DoConnect initiates a connection in a background goroutine.
func (a *App) DoConnect(cfg core.ClientConfig) {
	go func() {
		client := core.NewClient(cfg)
		if err := client.Connect(); err != nil {
			a.mu.Lock()
			a.Error = "Connection failed: " + err.Error()
			a.Connect.Connecting = false
			a.mu.Unlock()
			a.Window.Invalidate()
			return
		}
		if err := client.JoinRoom(); err != nil {
			client.Disconnect()
			a.mu.Lock()
			a.Error = "Join failed: " + err.Error()
			a.Connect.Connecting = false
			a.mu.Unlock()
			a.Window.Invalidate()
			return
		}

		a.mu.Lock()
		a.Client = client
		a.Screen = ScreenDashboard
		a.RoomName = cfg.Room
		a.Error = ""
		a.Connect.Connecting = false
		a.mu.Unlock()

		// Save config
		saved := &SavedConfig{
			ServerURL: cfg.ServerURL,
			Room:      cfg.Room,
			Password:  cfg.Password,
			Name:      cfg.Name,
			STUNServers: cfg.STUNServers,
			NoSTUN:    cfg.NoSTUN,
		}
		// Preserve saved forwards
		if prev := LoadConfig(); prev != nil {
			saved.Forwards = prev.Forwards
			saved.Autostart = prev.Autostart
		}
		SaveConfig(saved)

		// Load saved forwards
		a.mu.Lock()
		if saved.Forwards != nil {
			a.SavedForwards = saved.Forwards
		}
		a.mu.Unlock()

		a.StartEventConsumer()

		if !cfg.NoSTUN {
			servers := cfg.STUNServers
			if len(servers) == 0 {
				servers = []string{"stun.cloudflare.com:3478", "stun.miwifi.com:3478"}
			}
			client.DiscoverSTUN(servers)
		}

		// Auto-start saved enabled forwards (after a short delay for peer discovery)
		go func() {
			time.Sleep(3 * time.Second)
			a.mu.Lock()
			fwds := make([]SavedForward, len(a.SavedForwards))
			copy(fwds, a.SavedForwards)
			a.mu.Unlock()
			for _, sf := range fwds {
				if sf.Enabled {
					client.StartForward(sf.PeerName, sf.RemoteHost, sf.RemotePort, sf.LocalPort)
				}
			}
		}()

		a.Window.Invalidate()
	}()
}

// DoDisconnect tears down the client and returns to the connect screen.
func (a *App) DoDisconnect() {
	if a.Client != nil {
		a.Client.Disconnect()
		a.Client = nil
	}
	a.mu.Lock()
	a.Screen = ScreenConnect
	a.Peers = nil
	a.Forwards = nil
	a.mu.Unlock()
}

// SaveForwardsToConfig persists the current forward rules to disk.
func (a *App) SaveForwardsToConfig() {
	cfg := LoadConfig()
	if cfg == nil {
		cfg = &SavedConfig{}
	}
	a.mu.Lock()
	cfg.Forwards = make([]SavedForward, len(a.SavedForwards))
	copy(cfg.Forwards, a.SavedForwards)
	a.mu.Unlock()
	SaveConfig(cfg)
}

// AddSavedForward adds a forward rule to the saved list.
func (a *App) AddSavedForward(sf SavedForward) {
	a.mu.Lock()
	// Check if already exists (same local port)
	for i, f := range a.SavedForwards {
		if f.LocalPort == sf.LocalPort {
			a.SavedForwards[i] = sf
			a.mu.Unlock()
			a.SaveForwardsToConfig()
			return
		}
	}
	a.SavedForwards = append(a.SavedForwards, sf)
	a.mu.Unlock()
	a.SaveForwardsToConfig()
}

// RemoveSavedForward removes a forward rule from the saved list.
func (a *App) RemoveSavedForward(localPort int) {
	a.mu.Lock()
	for i, f := range a.SavedForwards {
		if f.LocalPort == localPort {
			a.SavedForwards = append(a.SavedForwards[:i], a.SavedForwards[i+1:]...)
			break
		}
	}
	a.mu.Unlock()
	a.SaveForwardsToConfig()
}

// SetSavedForwardEnabled enables/disables a saved forward (stop/start).
func (a *App) SetSavedForwardEnabled(localPort int, enabled bool) {
	a.mu.Lock()
	for i, f := range a.SavedForwards {
		if f.LocalPort == localPort {
			a.SavedForwards[i].Enabled = enabled
			break
		}
	}
	a.mu.Unlock()
	a.SaveForwardsToConfig()
}

