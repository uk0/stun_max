<p align="center">
  <img src="img/logo.png" width="128" alt="STUN Max Logo">
</p>

<h1 align="center">STUN Max</h1>

<p align="center">
  P2P TCP tunnel with STUN hole punching and automatic server relay fallback.<br>
  Cross-platform GUI + CLI. Zero configuration networking.
</p>

---

## Features

- **P2P Direct Connection** — STUN hole punch → Direct TCP upgrade, data never touches the server
- **Auto Relay Fallback** — If P2P fails after 5 attempts, seamlessly falls back to server relay
- **LAN Auto-Detection** — Same public IP peers connect via local address (zero latency)
- **Port Forwarding** — Map any remote peer's `host:port` to your localhost
- **Auto Reconnect** — Network changes trigger automatic reconnect (3s interval, infinite retry)
- **Room-Based Access** — Password-protected rooms, created via admin dashboard only
- **GUI + CLI** — Gio UI desktop app (Windows/Mac) + readline CLI with tab completion
- **NAT Diagnostic** — Same server binary: `stun_max-server natcheck` detects NAT type and punch success probability
- **Windows Tools** — One-click RDP enable (localhost-only firewall), auto-login, admin autostart
- **Config Persistence** — Connection, forwards, STUN servers saved and restored across restarts
- **Blacklist** — Ban clients per room from the admin dashboard
- **Traffic Stats** — Real-time upload/download speed and total bytes per forward
- **Self-Hosted STUN** — Lightweight STUN server included for China/restricted networks

## Architecture

```
┌──────────┐    1. UDP hole punch       ┌──────────┐
│ Client A │◄ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─  ►│ Client B │
│ (GUI/CLI)│    2. Direct TCP upgrade   │ (GUI/CLI)│
│          │◄══════════════════════════►│          │
└────┬─────┘    (reliable, fast)        └────┬─────┘
     │                                       │
     │   WebSocket (signaling + relay)       │
     └───────────────┬───────────────────────┘
                     │
              ┌──────┴──────┐
              │   Server    │
              │ Signal+Relay│
              │ + Dashboard │
              └─────────────┘
```

**Connection flow:**

1. Both clients connect to signal server via WebSocket
2. STUN discovery finds public IP:port (supports custom/self-hosted STUN)
3. UDP hole punch with Birthday Attack + port prediction
4. Direct TCP upgrade over the punched hole (reliable, ordered)
5. Tunnel data flows P2P — server not in the data path
6. If punch fails 5 times → auto relay, background retry continues
7. If P2P later succeeds → auto upgrade back from relay

## Screenshots

| Dashboard | GUI Client |
|-----------|------------|
| ![Dashboard](img/img_2.png) | ![Client](img/img_1.png) |

## Quick Start

### 1. Deploy Server

```bash
make

# Upload to your server
scp build/stun_max-server-linux-amd64 root@SERVER:/usr/local/bin/stun_max-server
```

Create a systemd unit (HTTP dashboard + optional embedded STUN on UDP 3478 in one process):

```bash
cat > /etc/systemd/system/stun-max.service << 'EOF'
[Unit]
Description=STUN Max Signal Server (+ optional STUN)
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/stun_max-server --addr :8080 --stun-addr :3478
Restart=always
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now stun-max
```

Omit `--stun-addr :3478` if you only need the WebSocket dashboard and use public STUN servers from clients.

Get the auto-generated dashboard password:

```bash
journalctl -u stun-max | grep Password
```

**Firewall:** Open TCP `8080` and UDP `3478`.

### 2. Create a Room

Open `http://SERVER:8080`, login, create a room with name + password.

Rooms persist until explicitly deleted — they survive client disconnects and server restarts.

### 3. Connect

**GUI (Windows/Mac):**

Run `stun_max-client-windows-amd64.exe` or `stun_max-client-darwin-arm64`, fill in server URL, room, password, name → Connect.

**CLI:**

```bash
./stun_max-cli --server ws://SERVER:8080/ws --room myroom --password secret --name laptop

# NAT diagnostic (no signaling server; same as stun_max-server natcheck)
./stun_max-cli natcheck -v
```

### 4. Forward Ports

```bash
# Forward peer's port to local
> forward peer-name 127.0.0.1:3389
> forward peer-name 192.168.1.100:8080 9090

# Manage
> forwards          # list with traffic stats
> unforward 3389    # stop
```

## Build

```bash
make                                          # all platforms; make help 查看目标
go build ./server/                            # HTTP server; add -stun-addr for embedded STUN; subcommand natcheck
go build ./client/                            # GUI client
go build -tags cli ./client/                  # CLI client
```

Server CLI:

- `stun_max-server [flags]` — signal server + dashboard (`-stun-addr :3478` enables STUN in the same binary).
- `stun_max-server natcheck [-servers …] [-v]` — NAT diagnostic (standalone; exits when done).
- `stun_max-server help` — usage for the HTTP mode.

CLI client (`go build -tags cli ./client/`):

- `stun_max-cli natcheck [-servers …] [-v]` — same NAT diagnostic as above (no `--server` / room).

## CLI Commands

| Command | Description |
|---------|-------------|
| `peers` | List peers with P2P/RELAY mode |
| `forward <peer> <host:port> [local]` | Forward remote port |
| `unforward <port>` | Stop forward |
| `forwards` | List forwards with traffic stats |
| `stun` | STUN/P2P connection details |
| `speedtest <peer>` | Bandwidth test |
| `help` | All commands |
| `quit` | Disconnect |

Tab completion for commands, peer names, and ports.

## GUI Tabs

| Tab | Description |
|-----|-------------|
| **Peers** | Peer list with P2P/RELAY badges, STUN endpoints, self mode display |
| **Forwards** | Create/stop/delete forwards, live traffic (bytes + speed), relay fallback |
| **Speed Test** | Upload/download bandwidth test between peers |
| **Tools** | Windows RDP enable (localhost-only), password management |
| **Settings** | Forward control, STUN server selector, autostart, auto-connect, auto-login |
| **Logs** | Scrollable event log |

## Security

| Feature | Detail |
|---------|--------|
| Room isolation | Relay verifies sender and receiver in same room |
| Room auth | Dashboard-only creation, SHA-256 password hash |
| Rate limiting | Login 5/min, WebSocket 20/min, Join 10/min per IP/client |
| Connection limit | Global max (default 5000, `--max-connections`) |
| Session expiry | Dashboard tokens expire after 24 hours |
| Blacklist | Ban/unban clients per room |
| Forward control | Per-client allow/deny + local-only mode |
| RDP firewall | Port 3389 restricted to 127.0.0.1 only |
| IP validation | X-Forwarded-For not trusted |

## Server Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--addr` | `:8080` | Listen address |
| `--web-password` | (random) | Dashboard password |
| `--max-connections` | `5000` | Max WebSocket connections |

## Client Flags (CLI)

Run `stun_max-cli natcheck -h` for NAT diagnostic flags (`-servers`, `-v`). Normal session flags apply only when connecting to a room (not used with the `natcheck` subcommand).

| Flag | Default | Description |
|------|---------|-------------|
| `--server` | `ws://localhost:8080/ws` | Server URL |
| `--room` | (required) | Room name |
| `--password` | | Room password |
| `--name` | (hostname) | Display name |
| `--stun` | `stun.cloudflare.com:3478` | STUN servers (comma-separated) |
| `--no-stun` | `false` | Relay only |
| `-v` | `false` | Verbose |

## Project Structure

```
server/              Signal + relay + dashboard
  main.go            HTTP/WS, auth, rate limiting, connection limits
  hub.go             Rooms, peers, blacklist
  client.go          Message routing, join validation
  relay.go           Data relay with room isolation
  stats.go           Traffic statistics

client/core/         Networking (shared by GUI + CLI)
  client.go          Connection, reconnect, signaling
  tunnel.go          Port forwarding, Direct TCP + relay
  stun.go            STUN discovery, hole punch, TCP upgrade
  crypto.go          X25519 + AES-256-GCM key exchange
  speedtest.go       Bandwidth testing
  types.go           Protocol types
  events.go          Event system

client/ui/           Gio UI desktop app
  app.go             Window, events, auto-connect
  connect.go         Login screen
  dashboard.go       Tab navigation
  peers.go           Peer list
  forwards.go        Forward management
  speedtest.go       Speed test
  tools.go           Windows RDP
  settings.go        All settings + STUN selector
  config.go          Config persistence
  autostart_*.go     Windows Task Scheduler / registry

server/web/          Admin dashboard (HTML/JS/CSS, go:embed)
server/stun.go       Embedded STUN server (-stun-addr)
server/natcheck.go   Delegates to internal/natcheck (server subcommand)

internal/natcheck/   NAT diagnostic (shared: `stun_max-server natcheck`, `stun_max-cli natcheck`)
```

## License

MIT
