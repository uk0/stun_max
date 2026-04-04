# CHANGELOG

## 2026-04-04 - Multi-VPN + Route Append + UI Improvements

### Multi-VPN Support
- **Multiple simultaneous VPN connections** — connect to different peers at the same time, each with independent TUN device, virtual IP (10.7.{N}.X), and routes
- **Per-peer data structure** — `tunDevices map[peerID]*TunDevice` replaces single pointer, per-peer ack channels and transport mode tracking
- **Route append** — adding a new subnet to an existing peer VPN appends the route instead of rejecting, B side notified automatically
- **Per-peer stop** — `StopTunPeer(peerID)` stops specific VPN, `StopTun()` stops all

### GUI VPN Panel
- **VPN list** — each active VPN shown as card with role badge, peer name, stats, and stop button
- **Role badge** — blue `OUT` (initiator/主动) or gray `IN` (responder/被动) per VPN entry
- **Stop button alignment** — pushed to right edge with flexed spacer
- **Stop All button** — appears when any VPN is active
- **Start always visible** — can add new VPN connections without stopping existing ones
- **5-column stats** — Local IP, Peer IP, Routes, Traffic (total), Speed (real-time ↑/s ↓/s)

### CLI VPN Commands
- `vpn <peer> <subnet1> [subnet2...]` — start VPN with multiple subnets
- `vpn <peer> <new-subnet>` — append route to existing VPN
- `vpn stop <peer>` — stop specific peer's VPN
- `vpn stop` — stop all VPNs
- `vpn status` — show all active VPNs with numbered list

### Internal
- Virtual IP allocation: `10.7.{N}.X` where N = VPN index, X = MAC-derived
- UDP packet routing by peer address for multi-device support
- `TunStatusAll() []TunInfo` API for GUI/CLI

## 2026-04-03 - gVisor Netstack + SpeedTest P2P + TUN VPN Improvements

### Architecture: gVisor Userspace TCP/IP Stack
- **TUN VPN Proxy**: Replaced hand-rolled TCP state machine with gVisor netstack (`tun_netstack.go`). TCP connections through VPN now have proper congestion control, SACK, retransmission, and window scaling — same stack used by Tailscale and tun2socks.
- **Port Forwarding**: Replaced RUTP-based tunnel transport with gVisor netstack (`forward_netstack.go`). Each peer pair gets a shared gVisor stack. A side uses `DialTCP`, B side uses TCP forwarder + `io.Copy` bridge. Eliminates RUTP bugs, dedup hash collisions, and memory leaks.
- **Legacy ICMP proxy** retained for raw ICMP socket (gVisor doesn't handle raw ICMP well).

### SpeedTest
- **P2P Transport Fix**: Reduced chunk size from 32KB to 1KB for P2P UDP mode (fits single UDP packet after base64 encoding). Previously 32KB chunks exceeded UDP MTU causing silent packet drops.
- **Transport Mode**: Added P2P-only speed test button. Progress bar and results show transport used.
- **Download Phase Fix**: Fixed `handleSTBegin` overwriting existing test during download phase, causing infinite ping-pong loop and UI stuck at "running + upload 100%".

### TUN VPN Improvements
- **TCP MSS Clamping**: SYN/SYN-ACK packets clamped to MSS 1360 to prevent fragmentation through tunnel.
- **Smart Compression**: Skip deflate for QUIC (UDP 443), RTP, WebRTC, and HTTPS bulk data — saves CPU on already-compressed traffic.
- **Error Recovery**: TUN read loop retries with backoff instead of exiting on first error.
- **ICMP NAT Key Fix**: Include target IP in ICMP NAT key to prevent collision between different destinations with same ICMP ID.
- **TCP Proxy Hardening**: Random initial sequence numbers, MSS option in SYN-ACK, sequence validation on incoming data, write-before-ACK, IP identification field and DF flag.
- **UDP Proxy Timeout**: Increased from 30s to 120s for streaming compatibility.

### Forward Module (Port Forwarding)
- **gVisor Transport**: Forward connections now use gVisor netstack instead of RUTP. Virtual IP scheme 10.99.0.1 ↔ 10.99.0.2 per peer pair.
- **Signaling**: `open_tunnel` with `ns:` prefix triggers netstack path. A waits for B's `tunnel_opened` confirmation before dialing.
- **Traffic Counting**: `BytesUp`/`BytesDown` properly tracked via `io.CopyBuffer` return values.
- **FN: UDP Prefix**: Forward netstack packets use `FN:` prefix on P2P UDP, `fwd_data` on relay.

## 2026-04-03 - Security, Stability & Feature Enhancements

### Security
- **E2E Relay Encryption**: All relay traffic now encrypted with X25519+AES-256-GCM (same keys as P2P). Server can no longer read relay data.
- **TLS Support**: Server supports `--tls-cert` and `--tls-key` flags for HTTPS/WSS
- Key exchange messages remain unencrypted (they're already secure — X25519 public keys)

### Stability
- **WebSocket Keepalive**: Client sends ping every 30s, handles pong with 120s timeout. Server pongWait increased to 120s.
- **Peer Leave Debounce**: 5-second delay before confirming peer departure, prevents false "peer left" during brief network hiccups
- **VPN Session Recovery**: Stale VPN sessions auto-cleaned when peer reconnects; peer ID updated on reconnect

### VPN Improvements
- **MAC-Based Virtual IP**: TUN IP derived from MAC address hash (deterministic, stable across restarts)
- **IP Persistence**: Virtual IP saved to config.json, restored on next launch
- **Userspace Subnet Proxy**: ICMP/UDP/TCP proxied through Go network stack (no kernel NAT dependency)
- **Exit IP Configuration**: Configurable exit gateway IP for subnet routing
- **tun_ack Protocol**: B side confirms VPN with its IP, A waits for ack before creating TUN

### Server Dashboard
- **Feature Tracking**: Clients report active features (VPN, forwards) to server
- **Dashboard Features Column**: Shows per-client feature badges (VPN, forwards, routes)

### UI
- **Peer Dropdown Selector**: All peer input fields replaced with dropdown selectors showing name + connection mode

## 2026-03-31 - natcheck: NAT Diagnostic Tool

### New Tool
- `tools/natcheck/` - 独立二进制，检测网络 NAT 类型和打洞可行性
  - Test 1: 多 STUN 服务器探测，同 socket 端口映射一致性
  - Test 2: 不同 socket 端口分配模式（sequential / port-preserving / random）
  - Test 3: Hairpin NAT 回环测试
  - Test 4: NAT Binding 存活时间
  - NAT 分类: Open / Full Cone / Restricted Cone / Port Restricted / Symmetric / Blocked
  - 打洞成功率评估: High / Medium / Low / None
  - 与各类 NAT 的兼容性矩阵
  - 运行: `go run ./tools/natcheck/` 或 `go build -o natcheck ./tools/natcheck/`


### New Features
- **STUN UDP Hole Punching**: built-in STUN client (no external deps), discovers public endpoint via stun.l.google.com
- **P2P Direct Mode**: after STUN discovery, peers exchange endpoints and attempt UDP hole punch (5 packets over 2s)
- **Auto Fallback**: if hole punch fails within timeout, auto fallback to WS relay; tunnel data seamlessly switches path
- **Periodic Retry**: relay peers re-attempt hole punch every 30s in background
- **UDP Tunnel Transport**: direct peers send raw tunnel data over UDP (no base64 overhead), much faster than relay
- **Dashboard P2P/Relay Display**: each peer shows ⚡ P2P (green) or 🔄 RELAY (orange) with STUN endpoint address
- **CLI `stun` command**: show STUN status and per-peer connection details
- **CLI flags**: `--stun` (custom STUN server), `--no-stun` (relay-only mode)
- **Peer name matching**: resolve peers by name prefix in addition to ID prefix

### Server Changes
- `stun_info` message type: broadcast to room or forward to specific peer
- PeerInfo includes `endpoint` field for STUN-discovered address
- Dashboard API exposes endpoint data

## 2026-03-31 - v2: CLI Client + TCP Tunnel + Web Dashboard

### Breaking Changes
- Web UI is now a server admin dashboard (not a P2P client)
- Client functionality moved to Go CLI tool

### New Features
- **CLI Client** (`client/main.go`): join room, list peers, port forward tunnel
  - `forward <peer_id> <host:port> [local_port]` - tunnel remote port to local
  - `unforward <local_port>` - stop forwarding
  - `peers` - list room peers with status
  - Peer ID prefix matching (type first few chars)
  - ANSI colored output, real-time peer join/leave notifications
- **TCP Tunnel Protocol**: open_tunnel/tunnel_data/close_tunnel over relay_data envelope
  - Bidirectional TCP forwarding through WebSocket relay
  - Multiple concurrent tunnels per peer
  - Base64 encoded binary data transport
- **Web Dashboard**: password-protected admin panel showing all rooms/peers/status
  - `--web-password` flag enables dashboard with HTTP basic auth
  - `/api/rooms` JSON API for room/peer data
  - Auto-refresh every 3s
- **Peer Names**: CLI clients advertise friendly names (--name flag, defaults to hostname)

### Architecture
- `server/` - Signal server + relay + web dashboard (Go)
- `client/` - CLI client with tunnel capability (Go)
- `web/` - Admin dashboard (HTML/JS/CSS)

## 2026-03-31 - v1: Initial Release

### Features
- WebRTC P2P hole punching via STUN
- Auto fallback to server relay (5s timeout)
- Room + password grouping (SHA-256)
- WebSocket signaling server (Go + gorilla/websocket)
