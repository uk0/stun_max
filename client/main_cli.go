//go:build cli

package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"stun_max/client/core"

	"github.com/chzyer/readline"
)

const (
	cReset  = "\033[0m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cCyan   = "\033[36m"
	cGray   = "\033[90m"
	cBold   = "\033[1m"
)

var client *core.Client

func notify(color, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("\r%s[*] %s%s\n", color, msg, cReset)
}

func main() {
	serverURL := flag.String("server", "ws://localhost:8080/ws", "WebSocket server URL")
	room := flag.String("room", "", "Room name to join")
	password := flag.String("password", "", "Room password")
	name := flag.String("name", "", "Display name")
	stunServer := flag.String("stun", "stun.cloudflare.com:3478", "STUN servers (comma-separated)")
	noStun := flag.Bool("no-stun", false, "Disable STUN (relay-only)")
	verbose := flag.Bool("v", false, "Verbose logging")
	flag.Parse()

	if *room == "" {
		fmt.Fprintf(os.Stderr, "Usage: stun_max-cli --server ws://host/ws --room <room> --password <pass> --name <name>\n")
		os.Exit(1)
	}
	if *name == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "client"
		}
		name = &h
	}

	cfg := core.ClientConfig{
		ServerURL: *serverURL,
		Room:      *room,
		Password:  *password,
		Name:      *name,
		NoSTUN:    *noStun,
		Verbose:   *verbose,
	}
	if *stunServer != "" {
		cfg.STUNServers = strings.Split(*stunServer, ",")
	}

	client = core.NewClient(cfg)

	// Event consumer
	go consumeEvents()

	fmt.Printf("%sConnecting to %s ...%s\n", cCyan, *serverURL, cReset)
	if err := client.Connect(); err != nil {
		fmt.Fprintf(os.Stderr, "Connection failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%sConnected! ID: %s%s\n", cGreen, client.MyID, cReset)

	if err := client.JoinRoom(); err != nil {
		fmt.Fprintf(os.Stderr, "Join failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%sJoined room %q as %q%s\n", cGreen, *room, *name, cReset)

	// STUN
	if !*noStun {
		servers := cfg.STUNServers
		if len(servers) == 0 {
			servers = []string{"stun.cloudflare.com:3478", "stun.miwifi.com:3478"}
		}
		client.DiscoverSTUN(servers)
	}

	// Signal handler
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			fmt.Println("\nShutting down...")
			client.Disconnect()
		case <-client.Done():
		}
	}()

	fmt.Println("Type 'help' for commands. Tab to autocomplete.")
	runCLI()

	client.WaitDone(3 * time.Second)
	fmt.Printf("%sDisconnected.%s\n", cGray, cReset)
}

func consumeEvents() {
	for evt := range client.Events() {
		switch evt.Type {
		case core.EventDisconnected:
			if le, ok := evt.Data.(core.LogEvent); ok {
				notify(cYellow, "%s", le.Message)
			}
		case core.EventReconnecting:
			if le, ok := evt.Data.(core.LogEvent); ok {
				notify(cCyan, "%s", le.Message)
			}
		case core.EventReconnected:
			if le, ok := evt.Data.(core.LogEvent); ok {
				notify(cGreen, "%s", le.Message)
			}
		case core.EventPeerJoined:
			if pe, ok := evt.Data.(core.PeerEvent); ok {
				notify(cGreen, "Peer joined: %s (%s)", pe.Name, shortID(pe.ID))
			}
		case core.EventPeerLeft:
			if pe, ok := evt.Data.(core.PeerEvent); ok {
				notify(cYellow, "Peer left: %s", pe.Name)
			}
		case core.EventHolePunchSuccess:
			if pe, ok := evt.Data.(core.PeerEvent); ok {
				notify(cGreen, "P2P direct with %s", shortID(pe.ID))
			}
		case core.EventTunnelRejected:
			if le, ok := evt.Data.(core.LogEvent); ok {
				notify(cRed, "%s", le.Message)
			}
		case core.EventReverseForwardStarted:
			if fe, ok := evt.Data.(core.ForwardEvent); ok {
				notify(cGreen, "Reverse forward active: peer exposed :%d -> %s:%d via %s", fe.LocalPort, fe.RemoteHost, fe.RemotePort, fe.PeerName)
			}
		case core.EventReverseForwardStopped:
			if fe, ok := evt.Data.(core.ForwardEvent); ok {
				notify(cYellow, "Reverse forward stopped: :%d", fe.LocalPort)
			}
		case core.EventFileOffer:
			if fo, ok := evt.Data.(core.FileOfferEvent); ok {
				notify(cCyan, "Incoming file from %s: %s (%s)", fo.PeerName, fo.FileName, fmtFileBytes(fo.FileSize))
				// Auto-accept in CLI mode
				home, _ := os.UserHomeDir()
				saveDir := filepath.Join(home, "Downloads", "StunMax")
				savePath := filepath.Join(saveDir, fo.FileName)
				if err := client.AcceptFile(fo.TransferID, savePath); err != nil {
					notify(cRed, "Auto-accept failed: %v", err)
				} else {
					notify(cGreen, "Auto-accepted, saving to %s", savePath)
				}
			}
		case core.EventFileProgress:
			if fp, ok := evt.Data.(core.FileProgressEvent); ok {
				notify(cCyan, "Transfer %s: %.0f%% (%s/s)", fp.TransferID[:8], fp.Progress*100, fmtFileBytes(int64(fp.Speed)))
			}
		case core.EventFileComplete:
			if fc, ok := evt.Data.(core.FileCompleteEvent); ok {
				if fc.Direction == "send" {
					notify(cGreen, "File sent: %s", fc.FileName)
				} else {
					notify(cGreen, "File received: %s", fc.FileName)
				}
			}
		case core.EventFileError:
			if fe, ok := evt.Data.(core.FileErrorEvent); ok {
				notify(cRed, "File transfer error [%s]: %s", fe.TransferID[:8], fe.Error)
			}
		case core.EventTunStarted:
			if le, ok := evt.Data.(core.LogEvent); ok {
				notify(cGreen, "%s", le.Message)
			}
		case core.EventTunStopped:
			if le, ok := evt.Data.(core.LogEvent); ok {
				notify(cYellow, "%s", le.Message)
			}
		case core.EventTunError:
			if le, ok := evt.Data.(core.LogEvent); ok {
				notify(cRed, "VPN error: %s", le.Message)
			}
		case core.EventLog:
			if le, ok := evt.Data.(core.LogEvent); ok {
				switch le.Level {
				case "error":
					notify(cRed, "%s", le.Message)
				case "warn":
					notify(cYellow, "%s", le.Message)
				default:
					notify(cCyan, "%s", le.Message)
				}
			}
		case core.EventError:
			if le, ok := evt.Data.(core.LogEvent); ok {
				notify(cRed, "ERROR: %s", le.Message)
			}
		}
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// peerCompleter returns peer IDs and names for tab completion.
func peerCompleter() []readline.PrefixCompleterInterface {
	if client == nil {
		return nil
	}
	peers := client.Peers()
	var items []readline.PrefixCompleterInterface
	for _, p := range peers {
		if p.ID == client.MyID {
			continue
		}
		items = append(items, readline.PcItem(p.ID))
		if p.Name != "" {
			items = append(items, readline.PcItem(p.Name))
		}
	}
	return items
}

func runCLI() {
	completer := readline.NewPrefixCompleter(
		readline.PcItem("peers"),
		readline.PcItem("forwards"),
		readline.PcItem("forward",
			readline.PcItemDynamic(func(line string) []string {
				if client == nil {
					return nil
				}
				peers := client.Peers()
				var names []string
				for _, p := range peers {
					if p.ID == client.MyID {
						continue
					}
					if p.Name != "" {
						names = append(names, p.Name)
					}
					names = append(names, p.ID)
				}
				return names
			}),
		),
		readline.PcItem("expose",
			readline.PcItemDynamic(func(line string) []string {
				if client == nil {
					return nil
				}
				peers := client.Peers()
				var names []string
				for _, p := range peers {
					if p.ID == client.MyID {
						continue
					}
					if p.Name != "" {
						names = append(names, p.Name)
					}
					names = append(names, p.ID)
				}
				return names
			}),
		),
		readline.PcItem("unforward",
			readline.PcItemDynamic(func(line string) []string {
				if client == nil {
					return nil
				}
				fwds := client.Forwards()
				var ports []string
				for _, f := range fwds {
					ports = append(ports, strconv.Itoa(f.LocalPort))
				}
				return ports
			}),
		),
		readline.PcItem("stun"),
		readline.PcItem("speedtest",
			readline.PcItemDynamic(func(line string) []string {
				if client == nil {
					return nil
				}
				peers := client.Peers()
				var names []string
				for _, p := range peers {
					if p.ID == client.MyID {
						continue
					}
					if p.Name != "" {
						names = append(names, p.Name)
					} else {
						names = append(names, p.ID)
					}
				}
				return names
			}),
		),
		readline.PcItem("send",
			readline.PcItemDynamic(func(line string) []string {
				if client == nil {
					return nil
				}
				peers := client.Peers()
				var names []string
				for _, p := range peers {
					if p.ID == client.MyID {
						continue
					}
					if p.Name != "" {
						names = append(names, p.Name)
					}
					names = append(names, p.ID)
				}
				return names
			}),
		),
		readline.PcItem("transfers"),
		readline.PcItem("hop",
			readline.PcItemDynamic(func(line string) []string {
				if client == nil {
					return nil
				}
				peers := client.Peers()
				var names []string
				for _, p := range peers {
					if p.ID == client.MyID {
						continue
					}
					if p.Name != "" {
						names = append(names, p.Name)
					}
					names = append(names, p.ID)
				}
				return names
			}),
		),
		readline.PcItem("vpn",
			readline.PcItem("stop"),
			readline.PcItem("status"),
			readline.PcItemDynamic(func(line string) []string {
				if client == nil {
					return nil
				}
				peers := client.Peers()
				var names []string
				for _, p := range peers {
					if p.ID == client.MyID {
						continue
					}
					if p.Name != "" {
						names = append(names, p.Name)
					}
					names = append(names, p.ID)
				}
				return names
			}),
		),
		readline.PcItem("help"),
		readline.PcItem("quit"),
	)

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "> ",
		AutoComplete:    completer,
		InterruptPrompt: "^C",
		EOFPrompt:       "quit",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "readline init failed: %v\n", err)
		return
	}
	defer rl.Close()

	for {
		select {
		case <-client.Done():
			return
		default:
		}

		line, err := rl.Readline()
		if err != nil {
			// EOF or interrupt
			client.Disconnect()
			return
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		cmd := strings.ToLower(parts[0])

		switch cmd {
		case "peers":
			printPeers()
		case "forwards", "tunnels":
			printForwards()
		case "forward":
			cmdForward(parts[1:])
		case "expose":
			cmdExpose(parts[1:])
		case "unforward":
			cmdUnforward(parts[1:])
		case "stun":
			cmdStun()
		case "speedtest":
			cmdSpeedTest(parts[1:])
		case "send":
			cmdSendFile(parts[1:])
		case "transfers":
			cmdTransfers()
		case "hop":
			cmdHop(parts[1:])
		case "vpn":
			cmdVPN(parts[1:])
		case "help":
			printHelp()
		case "quit", "exit":
			fmt.Println("Shutting down...")
			client.Disconnect()
			return
		default:
			fmt.Printf("%sUnknown command: %s (type 'help')%s\n", cRed, cmd, cReset)
		}
	}
}

func printPeers() {
	peers := client.Peers()
	fmt.Printf("\n%sPeers in room:%s\n", cBold, cReset)
	fmt.Printf("  %-16s %-14s %-10s %-12s\n", "ID", "NAME", "MODE", "STATUS")
	for _, p := range peers {
		id := p.ID
		if len(id) > 14 {
			id = id[:14]
		}
		nameDisplay := p.Name
		if nameDisplay == "" {
			nameDisplay = "-"
		}
		if len(nameDisplay) > 12 {
			nameDisplay = nameDisplay[:12]
		}

		mode := "-"
		statusLabel := p.Status
		statusColor := cGray

		if p.ID == client.MyID {
			statusLabel = "YOU"
			statusColor = cCyan
		} else {
			mode = client.PeerMode(p.ID)
			switch mode {
			case "direct":
				mode = "P2P"
				statusColor = cGreen
			case "connecting":
				statusColor = cCyan
			default:
				mode = "RELAY"
				statusColor = cYellow
			}
		}

		fmt.Printf("  %-16s %-14s %-10s %s%-12s%s\n", id, nameDisplay, mode, statusColor, statusLabel, cReset)
	}
	fmt.Println()
}

func printForwards() {
	fwds := client.Forwards()
	if len(fwds) == 0 {
		fmt.Printf("%sNo active forwards.%s\n", cGray, cReset)
		return
	}
	fmt.Printf("\n%sActive forwards:%s\n", cBold, cReset)
	fmt.Printf("  %-12s %-24s %-14s %-8s %-10s %-20s\n", "LOCAL", "REMOTE", "PEER", "MODE", "CONNS", "TRAFFIC")
	for _, f := range fwds {
		traffic := fmt.Sprintf("↑%s ↓%s", fmtBytes(f.BytesUp), fmtBytes(f.BytesDown))
		rate := ""
		if f.RateUp > 0 || f.RateDown > 0 {
			rate = fmt.Sprintf(" (%s/s↑ %s/s↓)", fmtBytes(int64(f.RateUp)), fmtBytes(int64(f.RateDown)))
		}
		fmt.Printf("  %-12s %-24s %-14s %-8s %-10d %s%s\n",
			fmt.Sprintf(":%d", f.LocalPort),
			fmt.Sprintf("%s:%d", f.RemoteHost, f.RemotePort),
			f.PeerName, f.Mode, f.ConnCount,
			traffic, rate,
		)
	}
	fmt.Println()
}

func cmdForward(args []string) {
	if len(args) < 2 {
		fmt.Printf("%sUsage: forward <peer> <host:port> [local_port]%s\n", cRed, cReset)
		return
	}

	hostPort := args[1]
	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		fmt.Printf("%sInvalid host:port: %s%s\n", cRed, hostPort, cReset)
		return
	}
	remotePort, err := strconv.Atoi(portStr)
	if err != nil || remotePort <= 0 || remotePort > 65535 {
		fmt.Printf("%sInvalid port: %s%s\n", cRed, portStr, cReset)
		return
	}

	localPort := remotePort
	if len(args) >= 3 {
		lp, err := strconv.Atoi(args[2])
		if err != nil || lp <= 0 || lp > 65535 {
			fmt.Printf("%sInvalid local port: %s%s\n", cRed, args[2], cReset)
			return
		}
		localPort = lp
	}

	if err := client.StartForward(args[0], host, remotePort, localPort); err != nil {
		fmt.Printf("%s%v%s\n", cRed, err, cReset)
	}
}

func cmdExpose(args []string) {
	if len(args) < 2 {
		fmt.Printf("%sUsage: expose <peer> <host:port> [remote_port]%s\n", cRed, cReset)
		return
	}

	hostPort := args[1]
	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		fmt.Printf("%sInvalid host:port: %s%s\n", cRed, hostPort, cReset)
		return
	}
	srcPort, err := strconv.Atoi(portStr)
	if err != nil || srcPort <= 0 || srcPort > 65535 {
		fmt.Printf("%sInvalid port: %s%s\n", cRed, portStr, cReset)
		return
	}

	remotePort := srcPort
	if len(args) >= 3 {
		rp, err := strconv.Atoi(args[2])
		if err != nil || rp <= 0 || rp > 65535 {
			fmt.Printf("%sInvalid remote port: %s%s\n", cRed, args[2], cReset)
			return
		}
		remotePort = rp
	}

	if err := client.ExposePort(args[0], host, srcPort, remotePort); err != nil {
		fmt.Printf("%s%v%s\n", cRed, err, cReset)
	}
}

func cmdUnforward(args []string) {
	if len(args) < 1 {
		fmt.Printf("%sUsage: unforward <local_port>%s\n", cRed, cReset)
		return
	}
	port, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Printf("%sInvalid port: %s%s\n", cRed, args[0], cReset)
		return
	}
	if err := client.StopForward(port); err != nil {
		fmt.Printf("%s%v%s\n", cRed, err, cReset)
	}
}

func cmdStun() {
	info := client.StunStatus()
	fmt.Printf("\n%sSTUN Status:%s\n", cBold, cReset)
	if !info.Enabled {
		fmt.Printf("  STUN: %sdisabled%s\n", cYellow, cReset)
	} else {
		fmt.Printf("  Public: %s%s%s\n", cGreen, info.PublicAddr, cReset)
	}
	for id, mode := range info.PeerConns {
		color := cYellow
		if mode == "direct" {
			color = cGreen
		}
		fmt.Printf("  %s: %s%s%s\n", shortID(id), color, mode, cReset)
	}
	fmt.Println()
}

func cmdSpeedTest(args []string) {
	if len(args) < 1 {
		fmt.Printf("%sUsage: speedtest <peer>%s\n", cRed, cReset)
		return
	}
	testID, err := client.StartSpeedTest(args[0])
	if err != nil {
		fmt.Printf("%s%v%s\n", cRed, err, cReset)
		return
	}
	fmt.Printf("%sSpeed test started: %s%s\n", cCyan, testID[:8], cReset)
}

func cmdHop(args []string) {
	// Usage: hop <via_peer> <target_peer> <host:port> [local_port]
	if len(args) < 3 {
		fmt.Printf("%sUsage: hop <via_peer> <target_peer> <host:port> [local_port]%s\n", cRed, cReset)
		return
	}

	viaPeer := args[0]
	targetPeer := args[1]
	hostPort := args[2]

	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		fmt.Printf("%sInvalid host:port: %s%s\n", cRed, hostPort, cReset)
		return
	}
	remotePort, err := strconv.Atoi(portStr)
	if err != nil || remotePort <= 0 || remotePort > 65535 {
		fmt.Printf("%sInvalid port: %s%s\n", cRed, portStr, cReset)
		return
	}

	localPort := remotePort
	if len(args) >= 4 {
		lp, err := strconv.Atoi(args[3])
		if err != nil || lp <= 0 || lp > 65535 {
			fmt.Printf("%sInvalid local port: %s%s\n", cRed, args[3], cReset)
			return
		}
		localPort = lp
	}

	if err := client.StartHopForward(viaPeer, targetPeer, host, remotePort, localPort); err != nil {
		fmt.Printf("%s%v%s\n", cRed, err, cReset)
	}
}

func cmdVPN(args []string) {
	if len(args) == 0 {
		fmt.Printf("%sUsage: vpn <peer> | vpn stop | vpn status%s\n", cRed, cReset)
		return
	}

	subcmd := strings.ToLower(args[0])
	switch subcmd {
	case "stop":
		if err := client.StopTun(); err != nil {
			fmt.Printf("%s%v%s\n", cRed, err, cReset)
		}
	case "status":
		info := client.TunStatus()
		if !info.Enabled {
			fmt.Printf("%sNo active VPN.%s\n", cGray, cReset)
			return
		}
		fmt.Printf("\n%sVPN Status:%s\n", cBold, cReset)
		fmt.Printf("  Local IP:  %s%s%s\n", cGreen, info.VirtualIP, cReset)
		fmt.Printf("  Peer IP:   %s%s%s\n", cGreen, info.PeerIP, cReset)
		fmt.Printf("  Subnet:    %s\n", info.Subnet)
		fmt.Printf("  Peer:      %s (%s)\n", info.PeerName, shortID(info.PeerID))
		fmt.Printf("  Traffic:   ↑%s ↓%s\n", fmtBytes(info.BytesUp), fmtBytes(info.BytesDown))
		if info.RateUp > 0 || info.RateDown > 0 {
			fmt.Printf("  Rate:      ↑%s/s ↓%s/s\n", fmtBytes(int64(info.RateUp)), fmtBytes(int64(info.RateDown)))
		}
		fmt.Println()
	default:
		// Treat as peer ID
		if err := client.StartTun(subcmd); err != nil {
			fmt.Printf("%s%v%s\n", cRed, err, cReset)
		}
	}
}

func printHelp() {
	fmt.Println()
	fmt.Printf("%sCommands:%s\n", cBold, cReset)
	fmt.Println("  peers                                      List peers in room")
	fmt.Println("  forward <peer> <host:port> [local]         Forward remote port to local")
	fmt.Println("  hop <via> <target> <host:port> [local]     Multi-hop: local → via → target:host:port")
	fmt.Println("  expose <peer> <host:port> [remote]         Expose your local port to peer (reverse forward)")
	fmt.Println("  unforward <local_port>                     Stop a forward")
	fmt.Println("  forwards                                   List active forwards")
	fmt.Println("  stun                                       Show STUN/P2P status")
	fmt.Println("  speedtest <peer>                           Run speed test")
	fmt.Println("  send <peer> <filepath>                     Send file to peer")
	fmt.Println("  transfers                                  List active file transfers")
	fmt.Println("  vpn <peer>                                 Start TUN VPN to peer (needs root)")
	fmt.Println("  vpn stop                                   Stop active VPN")
	fmt.Println("  vpn status                                 Show VPN status")
	fmt.Println("  help                                       Show this help")
	fmt.Println("  quit                                       Disconnect")
	fmt.Println()
}

func fmtBytes(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%dB", b)
	}
	if b < 1024*1024 {
		return fmt.Sprintf("%.1fK", float64(b)/1024)
	}
	if b < 1024*1024*1024 {
		return fmt.Sprintf("%.1fM", float64(b)/(1024*1024))
	}
	return fmt.Sprintf("%.2fG", float64(b)/(1024*1024*1024))
}

func cmdSendFile(args []string) {
	if len(args) < 2 {
		fmt.Printf("%sUsage: send <peer> <filepath>%s\n", cRed, cReset)
		return
	}
	peer := args[0]
	filePath := strings.Join(args[1:], " ") // support paths with spaces
	tid, err := client.SendFile(peer, filePath)
	if err != nil {
		fmt.Printf("%s%v%s\n", cRed, err, cReset)
		return
	}
	fmt.Printf("%sFile offer sent (transfer %s)%s\n", cCyan, tid[:8], cReset)
}

func cmdTransfers() {
	transfers := client.FileTransfers()
	if len(transfers) == 0 {
		fmt.Printf("%sNo active file transfers.%s\n", cGray, cReset)
		return
	}
	fmt.Printf("\n%sFile transfers:%s\n", cBold, cReset)
	fmt.Printf("  %-10s %-8s %-20s %-14s %-10s %-10s %-12s\n", "ID", "DIR", "FILE", "PEER", "PROGRESS", "SPEED", "STATUS")
	for _, t := range transfers {
		tid := t.TransferID
		if len(tid) > 8 {
			tid = tid[:8]
		}
		fileName := t.FileName
		if len(fileName) > 18 {
			fileName = fileName[:18] + ".."
		}
		peerName := t.PeerName
		if len(peerName) > 12 {
			peerName = peerName[:12]
		}
		progress := fmt.Sprintf("%.0f%%", t.Progress*100)
		speed := ""
		if t.Speed > 0 {
			speed = fmtFileBytes(int64(t.Speed)) + "/s"
		}
		statusColor := cGray
		switch t.Status {
		case "active":
			statusColor = cCyan
		case "complete":
			statusColor = cGreen
		case "error":
			statusColor = cRed
		case "pending":
			statusColor = cYellow
		}
		fmt.Printf("  %-10s %-8s %-20s %-14s %-10s %-10s %s%-12s%s\n",
			tid, t.Direction, fileName, peerName, progress, speed, statusColor, t.Status, cReset)
	}
	fmt.Println()
}

func fmtFileBytes(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%dB", b)
	}
	if b < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(b)/1024)
	}
	if b < 1024*1024*1024 {
		return fmt.Sprintf("%.1fMB", float64(b)/(1024*1024))
	}
	return fmt.Sprintf("%.2fGB", float64(b)/(1024*1024*1024))
}
