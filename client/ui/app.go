package ui

import (
	"fmt"
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
	a := &App{
		Window: new(app.Window),
		Theme:  NewTheme(),
		Screen: ScreenConnect,
	}
	// Always load config into connect form first
	a.Connect.init()

	// Restore persisted VirtualIP
	if cfg := LoadConfig(); cfg != nil && cfg.VirtualIP != "" {
		core.SetVirtualIP(cfg.VirtualIP)
	}

	// Try auto-connect if config exists and auto_connect is enabled
	if cfg := LoadConfig(); cfg != nil && cfg.AutoConnect && cfg.ServerURL != "" && cfg.Room != "" {
		go func() {
			time.Sleep(300 * time.Millisecond)
			a.mu.Lock()
			a.Connect.Connecting = true
			a.mu.Unlock()
			a.Window.Invalidate()

			a.DoConnect(core.ClientConfig{
				ServerURL:   cfg.ServerURL,
				Room:        cfg.Room,
				Password:    cfg.Password,
				Name:        cfg.Name,
				STUNServers: cfg.STUNServers,
				NoSTUN:      cfg.NoSTUN,
			})
		}()
	}
	return a
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
	case core.EventFileOffer:
		if fo, ok := evt.Data.(core.FileOfferEvent); ok {
			a.Dashboard.Files.handleOffer(fo)
			a.addLog("info", fmt.Sprintf("File offer from %s: %s", fo.PeerName, fo.FileName))
		}
	case core.EventFileProgress:
		if fp, ok := evt.Data.(core.FileProgressEvent); ok {
			a.Dashboard.Files.handleProgress(fp)
		}
	case core.EventFileComplete:
		if fc, ok := evt.Data.(core.FileCompleteEvent); ok {
			a.Dashboard.Files.handleComplete(fc)
			a.addLog("info", fmt.Sprintf("File %s complete: %s", fc.Direction, fc.FileName))
		}
	case core.EventFileError:
		if fe, ok := evt.Data.(core.FileErrorEvent); ok {
			a.Dashboard.Files.handleError(fe)
			a.addLog("error", fmt.Sprintf("File transfer error: %s", fe.Error))
		}
	case core.EventTunStarted:
		if le, ok := evt.Data.(core.LogEvent); ok {
			a.addLog("info", le.Message)
		}
	case core.EventTunStopped:
		if le, ok := evt.Data.(core.LogEvent); ok {
			a.addLog("info", le.Message)
		}
	case core.EventTunError:
		if le, ok := evt.Data.(core.LogEvent); ok {
			a.addLog("error", le.Message)
		}
	case core.EventDisconnected:
		if le, ok := evt.Data.(core.LogEvent); ok {
			a.addLog("warn", le.Message)
		}
	case core.EventReconnecting:
		if le, ok := evt.Data.(core.LogEvent); ok {
			a.addLog("info", le.Message)
			a.Error = le.Message
		}
	case core.EventReconnected:
		if le, ok := evt.Data.(core.LogEvent); ok {
			a.addLog("info", le.Message)
			a.Error = ""
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

		// Start event consumer BEFORE joining so we can catch error/peer_list
		a.mu.Lock()
		a.Client = client
		a.mu.Unlock()

		// Channel to wait for join result
		joinResult := make(chan bool, 1)
		go func() {
			for evt := range client.Events() {
				a.handleEvent(evt)
				a.Window.Invalidate()

				switch evt.Type {
				case core.EventPeerListUpdated:
					// Got peer list = join succeeded
					select {
					case joinResult <- true:
					default:
					}
				case core.EventError:
					// Server rejected join
					select {
					case joinResult <- false:
					default:
					}
				}
			}
		}()

		if err := client.JoinRoom(); err != nil {
			client.Disconnect()
			a.mu.Lock()
			a.Client = nil
			a.Error = "Join failed: " + err.Error()
			a.Connect.Connecting = false
			a.mu.Unlock()
			a.Window.Invalidate()
			return
		}

		// Wait up to 5 seconds for join confirmation
		var joinOK bool
		select {
		case joinOK = <-joinResult:
		case <-time.After(5 * time.Second):
			joinOK = true // timeout = assume success (no error received)
		}

		if !joinOK {
			client.Disconnect()
			a.mu.Lock()
			a.Client = nil
			a.Connect.Connecting = false
			// a.Error already set by handleEvent
			a.mu.Unlock()
			a.Window.Invalidate()
			return
		}

		// Join succeeded — switch to dashboard
		a.mu.Lock()
		a.Screen = ScreenDashboard
		a.RoomName = cfg.Room
		a.Error = ""
		a.Connect.Connecting = false
		a.mu.Unlock()

		// Save config
		saved := &SavedConfig{
			ServerURL:   cfg.ServerURL,
			Room:        cfg.Room,
			Password:    cfg.Password,
			Name:        cfg.Name,
			STUNServers: cfg.STUNServers,
			NoSTUN:      cfg.NoSTUN,
			AutoConnect: true,
		}
		if prev := LoadConfig(); prev != nil {
			saved.Forwards = prev.Forwards
			saved.Autostart = prev.Autostart
			saved.AllowForward = prev.AllowForward
			saved.LocalOnly = prev.LocalOnly
			saved.VirtualIP = prev.VirtualIP
			saved.VPNPeer = prev.VPNPeer
			saved.VPNRoutes = prev.VPNRoutes
			saved.VPNExitIP = prev.VPNExitIP
			saved.VPNAutoStart = prev.VPNAutoStart
		}
		SaveConfig(saved)

		// Apply saved settings to core client
		if saved.AllowForward != nil {
			client.SetAllowForward(*saved.AllowForward)
		}
		if saved.LocalOnly != nil {
			client.SetLocalOnly(*saved.LocalOnly)
		}

		// Load saved forwards
		a.mu.Lock()
		if saved.Forwards != nil {
			a.SavedForwards = saved.Forwards
		}
		a.mu.Unlock()

		// Periodic refresh for forward traffic stats
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

		if !cfg.NoSTUN {
			servers := cfg.STUNServers
			if len(servers) == 0 {
				servers = []string{"stun.cloudflare.com:3478", "stun.miwifi.com:3478"}
			}
			client.DiscoverSTUN(servers)
		}

		// Auto-start saved enabled forwards
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

		// Auto-start VPN if configured
		go func() {
			vpnCfg := LoadConfig()
			if vpnCfg != nil && vpnCfg.VPNAutoStart && vpnCfg.VPNPeer != "" {
				time.Sleep(5 * time.Second)
				client.StartTun(vpnCfg.VPNPeer, vpnCfg.VPNRoutes, vpnCfg.VPNExitIP)
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

	// Reload saved config into connect form
	a.Connect.ReloadConfig()
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

