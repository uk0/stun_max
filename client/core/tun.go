package core

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// tunIface abstracts the TUN device across platforms.
type tunIface interface {
	io.ReadWriteCloser
	Name() string
}

// natEntry tracks a single SNAT connection for reverse translation.
type natEntry struct {
	originalSrcIP net.IP
	lastSeen      time.Time
}

// snatDebugCounter limits debug logging to first N SNAT operations.
var snatDebugCounter int32

// TunDevice holds the state of an active VPN tunnel.
type TunDevice struct {
	iface      tunIface
	ifName     string
	virtualIP  net.IP
	peerIP     net.IP
	subnet     *net.IPNet
	routes     []string
	exitIP     net.IP       // B's real LAN IP (exit gateway)
	snatIP     net.IP       // phantom IP for SNAT (in target subnet)
	routeNets  []*net.IPNet // parsed route subnets for fast lookup
	natTable   sync.Map     // NAT tracking entries
	proxy      *subnetProxy    // legacy proxy for ICMP (kept for raw ICMP socket)
	nsProxy    *netstackProxy  // gVisor netstack proxy for TCP/UDP
	serverHost string       // server hostname for route protection
	peerID     string
	peerName   string
	bytesUp   int64
	bytesDown int64
	lastUp    int64
	lastDown  int64
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
}

// Virtual IP allocation: derived from MAC address for stability.
// Each client gets a deterministic IP in 10.7.0.2-253 based on hardware identity.
// VirtualIP can be overridden via config persistence.
var cachedVirtualIP string

// SetVirtualIP allows persisted IP to be restored on startup.
func SetVirtualIP(ip string) { cachedVirtualIP = ip }

// GetVirtualIP returns the current client's stable virtual IP.
func GetVirtualIP() string {
	if cachedVirtualIP != "" {
		return cachedVirtualIP
	}
	cachedVirtualIP = deriveVirtualIP()
	return cachedVirtualIP
}

// deriveVirtualIP generates a deterministic IP from the primary MAC address.
func deriveVirtualIP() string {
	mac := getPrimaryMAC()
	if mac == nil {
		// Fallback: use hostname hash
		return "10.7.0.100"
	}
	h := sha256.Sum256(mac)
	// Map to 2-253 range (avoid .0, .1, .254, .255)
	octet := int(h[0])%252 + 2
	return fmt.Sprintf("10.7.0.%d", octet)
}

// getPrimaryMAC returns the MAC address of the first active non-loopback interface.
func getPrimaryMAC() net.HardwareAddr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(iface.HardwareAddr) >= 6 {
			return iface.HardwareAddr
		}
	}
	return nil
}

// extractHost extracts the hostname from a WebSocket URL (ws://host:port/path).
func extractHost(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// StartTun initiates a TUN VPN with the given peer.
// routes: subnets to route through the peer, e.g. ["10.88.51.0/24"]
// exitIP: optional exit gateway IP on the peer side (auto-detected if empty)
func (c *Client) StartTun(peerID string, routes []string, exitIP string) error {
	fullID, err := c.resolvePeerID(peerID)
	if err != nil {
		return err
	}

	c.tunMu.Lock()
	if c.tunDevice != nil {
		c.tunMu.Unlock()
		return fmt.Errorf("VPN already active (stop first)")
	}
	c.tunMu.Unlock()

	myIP := GetVirtualIP()
	subnet := "10.7.0.0/24"

	// Notify caller to persist the virtual IP (via event)
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("VPN using virtual IP: %s (MAC-derived)", myIP)})

	// Prepare ack channel to receive B's virtual IP
	c.tunAckCh = make(chan string, 1)
	atomic.StoreInt32(&tunDirectMode, 0) // reset transport mode tracking

	err = c.sendRelay(fullID, "tun_setup", TunSetup{
		PeerIP:  myIP,
		Subnet:  subnet,
		Routes:  routes,
		ExitIP:  exitIP,
	})
	if err != nil {
		return fmt.Errorf("send tun_setup: %w", err)
	}

	// Wait for B's tun_ack with its virtual IP (timeout 10s)
	var peerIP string
	select {
	case peerIP = <-c.tunAckCh:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("VPN setup timeout: peer did not respond")
	case <-c.done:
		return fmt.Errorf("client disconnected")
	}

	// Create local TUN device
	dev, err := c.createTunDevice(myIP, peerIP, subnet, fullID)
	if err != nil {
		return err
	}
	dev.routes = routes

	// Protect server route BEFORE adding TUN routes
	if serverHost := extractHost(c.Config.ServerURL); serverHost != "" {
		dev.serverHost = serverHost
		protectServerRoute(serverHost)
	}

	// A side: add subnet routes through TUN to B
	for _, route := range routes {
		addRoute(dev.ifName, route, peerIP)
	}

	c.tunMu.Lock()
	c.tunDevice = dev
	c.tunMu.Unlock()

	c.wg.Add(1)
	go c.tunReadLoop(dev)

	peerName := shortID(fullID)
	c.peersMu.RLock()
	for _, p := range c.peers {
		if p.ID == fullID && p.Name != "" {
			peerName = p.Name
			break
		}
	}
	c.peersMu.RUnlock()
	dev.peerName = peerName

	c.emit(EventTunStarted, LogEvent{Level: "info", Message: fmt.Sprintf("VPN started: %s <-> %s (peer: %s)", myIP, peerIP, peerName)})
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("TUN VPN active: local=%s peer=%s subnet=%s", myIP, peerIP, subnet)})
	c.ReportFeatures()
	return nil
}

// StopTun tears down the active TUN VPN.
func (c *Client) StopTun() error {
	c.tunMu.Lock()
	dev := c.tunDevice
	c.tunDevice = nil
	c.tunMu.Unlock()

	if dev == nil {
		return fmt.Errorf("no active VPN")
	}

	c.sendRelay(dev.peerID, "tun_teardown", TunTeardown{})

	if dev.nsProxy != nil {
		dev.nsProxy.Close()
	}
	if dev.proxy != nil {
		dev.proxy.Close()
	}
	if dev.iface != nil {
		dev.iface.Close()
	}
	for _, route := range dev.routes {
		removeRoute(dev.ifName, route)
	}
	cleanupSNATRoute(dev.ifName, dev.snatIP)
	disableNAT(dev.ifName)
	removeTunInterface(dev.ifName)
	removeServerRoute(dev.serverHost)

	c.emit(EventTunStopped, LogEvent{Level: "info", Message: "VPN stopped"})
	c.ReportFeatures()
	return nil
}

func (c *Client) createTunDevice(localIP, peerIP, subnet, peerID string) (*TunDevice, error) {
	iface, err := createPlatformTun()
	if err != nil {
		return nil, fmt.Errorf("TUN device creation failed (need root/admin): %w", err)
	}

	ifName := iface.Name()
	if err := configureTunInterface(ifName, localIP, peerIP); err != nil {
		iface.Close()
		return nil, fmt.Errorf("TUN interface config failed: %w", err)
	}

	_, ipNet, _ := net.ParseCIDR(subnet)

	return &TunDevice{
		iface:     iface,
		ifName:    ifName,
		virtualIP: net.ParseIP(localIP),
		peerIP:    net.ParseIP(peerIP),
		subnet:    ipNet,
		peerID:    peerID,
		done:      make(chan struct{}),
	}, nil
}

// handleTunSetup processes an incoming tun_setup from a peer (B side).
func (c *Client) handleTunSetup(msg Message) {
	var setup TunSetup
	if err := json.Unmarshal(msg.Payload, &setup); err != nil {
		c.emit(EventTunError, LogEvent{Level: "error", Message: "Invalid tun_setup: " + err.Error()})
		return
	}

	c.tunMu.Lock()
	if old := c.tunDevice; old != nil {
		// If old VPN peer is gone or it's the same peer reconnecting, tear down old session
		c.tunDevice = nil
		c.tunMu.Unlock()
		c.emit(EventLog, LogEvent{Level: "info", Message: "Replacing stale VPN session"})
		old.closeOnce.Do(func() { close(old.done) })
		if old.proxy != nil {
			old.proxy.Close()
		}
		if old.iface != nil {
			old.iface.Close()
		}
		for _, route := range old.routes {
			removeRoute(old.ifName, route)
		}
		cleanupSNATRoute(old.ifName, old.snatIP)
		disableNAT(old.ifName)
		removeTunInterface(old.ifName)
		removeServerRoute(old.serverHost)
	} else {
		c.tunMu.Unlock()
	}

	// B side: use own MAC-derived IP, peer's IP comes from setup.PeerIP
	myIP := GetVirtualIP()
	peerIP := setup.PeerIP

	// Collision check: if both sides derived the same IP, offset B by 1
	if myIP == peerIP {
		ip := net.ParseIP(myIP).To4()
		if ip != nil {
			next := int(ip[3]) + 1
			if next > 253 {
				next = 2
			}
			myIP = fmt.Sprintf("10.7.0.%d", next)
		}
	}

	dev, err := c.createTunDevice(myIP, peerIP, setup.Subnet, msg.From)
	if err != nil {
		c.emit(EventTunError, LogEvent{Level: "error", Message: "TUN setup failed: " + err.Error()})
		return
	}
	dev.routes = setup.Routes

	peerName := shortID(msg.From)
	c.peersMu.RLock()
	for _, p := range c.peers {
		if p.ID == msg.From && p.Name != "" {
			peerName = p.Name
			break
		}
	}
	c.peersMu.RUnlock()
	dev.peerName = peerName

	c.tunMu.Lock()
	c.tunDevice = dev
	c.tunMu.Unlock()

	c.wg.Add(1)
	go c.tunReadLoop(dev)

	// Send tun_ack IMMEDIATELY so A doesn't timeout
	c.sendRelay(msg.From, "tun_ack", TunAck{VirtualIP: myIP})
	c.emit(EventTunStarted, LogEvent{Level: "info", Message: fmt.Sprintf("VPN accepted: %s <-> %s (peer: %s)", myIP, peerIP, peerName)})

	// Do slow setup in background (forwarding, NAT, proxy, server route protection)
	go func() {
		if serverHost := extractHost(c.Config.ServerURL); serverHost != "" {
			dev.serverHost = serverHost
			protectServerRoute(serverHost)
		}

		if len(setup.Routes) > 0 {
			atomic.StoreInt32(&snatDebugCounter, 0)
			enableIPForwarding()
			enableNAT(dev.ifName)

			for _, r := range setup.Routes {
				_, ipNet, err := net.ParseCIDR(r)
				if err == nil {
					dev.routeNets = append(dev.routeNets, ipNet)
				}
			}

			if setup.ExitIP != "" {
				dev.exitIP = net.ParseIP(setup.ExitIP)
			}
			if dev.exitIP == nil && len(setup.Routes) > 0 {
				dev.exitIP = detectExitIP(setup.Routes[0])
			}

			if dev.exitIP != nil && len(setup.Routes) > 0 {
				dev.snatIP = pickSNATIP(setup.Routes[0], dev.exitIP)
				if dev.snatIP != nil {
					setupSNATRoute(dev.ifName, dev.snatIP)
				}
			}

			dev.proxy = newSubnetProxy(dev, c) // ICMP only
			nsp, nsErr := newNetstackProxy(dev, c)
			if nsErr != nil {
				c.emit(EventLog, LogEvent{Level: "warn", Message: "netstack proxy failed: " + nsErr.Error()})
			} else {
				dev.nsProxy = nsp
			}
			c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
				"Subnet proxy ready: exitIP=%s routes=%v netstack=%v", dev.exitIP, setup.Routes, dev.nsProxy != nil)})

			if out := checkForwardingStatus(); out != "" {
				c.emit(EventLog, LogEvent{Level: "info", Message: "Forwarding status: " + out})
			}
		}
		c.ReportFeatures()
	}()
}

// handleTunAck processes B's response with its virtual IP.
func (c *Client) handleTunAck(msg Message) {
	var ack TunAck
	if err := json.Unmarshal(msg.Payload, &ack); err != nil {
		return
	}
	if c.tunAckCh != nil {
		select {
		case c.tunAckCh <- ack.VirtualIP:
		default:
		}
	}
}

// handleTunData processes incoming TUN data from a peer.
// On B side with routes: applies SNAT before writing to TUN.
func (c *Client) handleTunData(msg Message) {
	c.tunMu.RLock()
	dev := c.tunDevice
	c.tunMu.RUnlock()

	if dev == nil {
		return
	}

	var td TunData
	if err := json.Unmarshal(msg.Payload, &td); err != nil {
		return
	}

	compressed, err := base64.StdEncoding.DecodeString(td.Data)
	if err != nil {
		return
	}

	raw, err := Decompress(compressed)
	if err != nil {
		return
	}

	// Clamp TCP MSS in incoming SYN packets
	raw = clampMSS(raw)

	atomic.AddInt64(&dev.bytesDown, int64(len(raw)))

	// Try gVisor netstack proxy first (TCP/UDP with proper congestion control)
	if dev.nsProxy != nil && len(raw) >= 20 {
		if dev.nsProxy.handlePacket(raw) {
			return
		}
	}

	// Fallback: legacy proxy for ICMP
	if dev.proxy != nil && len(raw) >= 20 {
		if dev.proxy.handlePacket(raw) {
			return
		}
	}

	// Fall back to SNAT+TUN for unhandled protocols
	if dev.snatIP != nil && len(raw) >= 20 {
		raw = dev.applySNAT(raw)
	}

	dev.mu.Lock()
	defer dev.mu.Unlock()
	if dev.iface != nil {
		dev.iface.Write(raw)
	}
}

// handleTunDataDirect processes VPN data received via direct TCP (P2P).
// Same logic as handleTunData but data is already decompressed.
func (c *Client) handleTunDataDirect(raw []byte, conn net.Conn) {
	c.tunMu.RLock()
	dev := c.tunDevice
	c.tunMu.RUnlock()

	if dev == nil {
		return
	}

	// Clamp TCP MSS in incoming SYN packets
	raw = clampMSS(raw)

	atomic.AddInt64(&dev.bytesDown, int64(len(raw)))

	// Try gVisor netstack proxy first
	if dev.nsProxy != nil && len(raw) >= 20 {
		if dev.nsProxy.handlePacket(raw) {
			return
		}
	}

	// Fallback: legacy proxy for ICMP
	if dev.proxy != nil && len(raw) >= 20 {
		if dev.proxy.handlePacket(raw) {
			return
		}
	}

	// Fall back to SNAT+TUN for TCP
	if dev.snatIP != nil && len(raw) >= 20 {
		raw = dev.applySNAT(raw)
	}

	dev.mu.Lock()
	defer dev.mu.Unlock()
	if dev.iface != nil {
		dev.iface.Write(raw)
	}
}

// applySNAT rewrites source IP from peer's virtual IP to our SNAT IP
// for packets destined to routed subnets. This makes the packet appear
// to come from an IP in the target subnet, so replies route back through TUN.
func (dev *TunDevice) applySNAT(pkt []byte) []byte {
	if len(pkt) < 20 {
		return pkt
	}
	// Only handle IPv4
	if pkt[0]>>4 != 4 {
		return pkt
	}

	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl {
		return pkt
	}

	dstIP := net.IP(pkt[16:20])

	// Check if destination is in a routed subnet
	inRoute := false
	for _, rn := range dev.routeNets {
		if rn.Contains(dstIP) {
			inRoute = true
			break
		}
	}
	if !inRoute {
		return pkt
	}

	// Save original source IP for reverse NAT
	origSrc := make(net.IP, 4)
	copy(origSrc, pkt[12:16])

	proto := pkt[9]
	var natKey string

	// Build NAT key based on protocol
	switch proto {
	case 6, 17: // TCP, UDP — use ports
		if len(pkt) >= ihl+4 {
			srcPort := binary.BigEndian.Uint16(pkt[ihl : ihl+2])
			dstPort := binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
			natKey = fmt.Sprintf("%d:%s:%d:%d", proto, dstIP, dstPort, srcPort)
		}
	case 1: // ICMP — use ID field for echo request/reply
		if len(pkt) >= ihl+6 {
			icmpType := pkt[ihl]
			if icmpType == 8 { // Echo Request
				icmpID := binary.BigEndian.Uint16(pkt[ihl+4 : ihl+6])
				natKey = fmt.Sprintf("icmp:%s:%d", dstIP, icmpID)
			}
		}
	}

	// Store NAT entry for reverse translation
	if natKey != "" {
		dev.natTable.Store(natKey, &natEntry{
			originalSrcIP: origSrc,
			lastSeen:      time.Now(),
		})
	}

	// Rewrite source IP to SNAT IP
	copy(pkt[12:16], dev.snatIP.To4())

	// Recalculate IP header checksum
	pkt[10] = 0
	pkt[11] = 0
	cksum := ipHeaderChecksum(pkt[:ihl])
	binary.BigEndian.PutUint16(pkt[10:12], cksum)

	// Fix TCP/UDP checksum (pseudo-header includes source IP)
	if proto == 6 && len(pkt) >= ihl+18 { // TCP
		fixTransportChecksum(pkt, ihl, origSrc, dev.snatIP.To4(), true)
	} else if proto == 17 && len(pkt) >= ihl+8 { // UDP
		fixTransportChecksum(pkt, ihl, origSrc, dev.snatIP.To4(), false)
	}
	// ICMP checksum doesn't include IP pseudo-header, no fix needed

	return pkt
}

// handleTunTeardown processes a tun_teardown from a peer.
func (c *Client) handleTunTeardown(msg Message) {
	c.tunMu.Lock()
	dev := c.tunDevice
	if dev != nil && dev.peerID == msg.From {
		c.tunDevice = nil
	} else {
		c.tunMu.Unlock()
		return
	}
	c.tunMu.Unlock()

	dev.closeOnce.Do(func() { close(dev.done) })
	if dev.nsProxy != nil {
		dev.nsProxy.Close()
	}
	if dev.proxy != nil {
		dev.proxy.Close()
	}
	if dev.iface != nil {
		dev.iface.Close()
	}
	cleanupSNATRoute(dev.ifName, dev.snatIP)
	removeTunInterface(dev.ifName)
	removeServerRoute(dev.serverHost)

	c.emit(EventTunStopped, LogEvent{Level: "info", Message: "VPN stopped by peer"})
}

// tunReadLoop reads IP packets from the TUN device and sends them to the peer.
// Transport priority: UDP P2P > WebSocket relay
var tunDirectMode int32 // 0=unknown, 1=p2p, 2=relay

func (c *Client) tunReadLoop(dev *TunDevice) {
	defer c.wg.Done()

	buf := make([]byte, 65536)
	errCount := 0
	for {
		select {
		case <-dev.done:
			return
		case <-c.done:
			return
		default:
		}

		n, err := dev.iface.Read(buf)
		if err != nil {
			select {
			case <-dev.done:
				return
			case <-c.done:
				return
			default:
			}
			errCount++
			if errCount >= 10 {
				c.emit(EventTunError, LogEvent{Level: "error", Message: "TUN read: too many errors, stopping"})
				return
			}
			c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf("TUN read error (%d/10): %v", errCount, err)})
			time.Sleep(time.Duration(errCount*100) * time.Millisecond)
			continue
		}
		errCount = 0 // reset on success
		if n == 0 {
			continue
		}

		packet := buf[:n]

		// Clamp TCP MSS in SYN packets to prevent fragmentation
		packet = clampMSS(packet)

		// Apply reverse SNAT if B side with routes
		if dev.snatIP != nil && len(packet) >= 20 {
			packet = dev.applyReverseSNAT(packet)
		}

		atomic.AddInt64(&dev.bytesUp, int64(len(packet)))

		compressed := tunCompress(packet)

		// UDP P2P (primary — fast, no server overhead)
		if c.tunSendUDP(dev.peerID, compressed) {
			if atomic.CompareAndSwapInt32(&tunDirectMode, 0, 1) || atomic.CompareAndSwapInt32(&tunDirectMode, 2, 1) {
				c.emit(EventLog, LogEvent{Level: "info", Message: "VPN data: using P2P UDP"})
			}
			continue
		}

		// Relay fallback (slower, through server)
		if atomic.CompareAndSwapInt32(&tunDirectMode, 0, 2) || atomic.CompareAndSwapInt32(&tunDirectMode, 1, 2) {
			c.emit(EventLog, LogEvent{Level: "warn", Message: "VPN data: using server relay (no P2P)"})
		}
		encoded := base64.StdEncoding.EncodeToString(compressed)
		c.sendRelay(dev.peerID, "tun_data", TunData{Data: encoded})
	}
}

// tunMSS is the maximum segment size for TCP through the TUN tunnel.
// 1500 (MTU) - 20 (IP) - 20 (TCP) - 100 (tunnel overhead: compression header + UDP/relay framing)
const tunMSS = 1360

// clampMSS rewrites the TCP MSS option in SYN packets to prevent fragmentation.
// Only modifies SYN or SYN-ACK packets that have MSS > tunMSS.
func clampMSS(pkt []byte) []byte {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return pkt
	}
	ihl := int(pkt[0]&0x0f) * 4
	if pkt[9] != 6 || len(pkt) < ihl+20 { // not TCP
		return pkt
	}
	flags := pkt[ihl+13]
	if flags&0x02 == 0 { // not SYN
		return pkt
	}
	dataOff := int(pkt[ihl+12]>>4) * 4
	if dataOff <= 20 { // no TCP options
		return pkt
	}

	// Walk TCP options looking for MSS (kind=2, len=4)
	opts := pkt[ihl+20 : ihl+dataOff]
	for i := 0; i < len(opts); {
		kind := opts[i]
		if kind == 0 { // End of options
			break
		}
		if kind == 1 { // NOP
			i++
			continue
		}
		if i+1 >= len(opts) {
			break
		}
		optLen := int(opts[i+1])
		if optLen < 2 || i+optLen > len(opts) {
			break
		}
		if kind == 2 && optLen == 4 { // MSS option
			mss := binary.BigEndian.Uint16(opts[i+2 : i+4])
			if mss > tunMSS {
				binary.BigEndian.PutUint16(opts[i+2:i+4], tunMSS)
				// Recalculate TCP checksum from scratch
				csumOff := ihl + 16
				pkt[csumOff] = 0
				pkt[csumOff+1] = 0
				tcpSeg := pkt[ihl:]
				srcIP := pkt[12:16]
				dstIP := pkt[16:20]
				newCsum := tcpChecksumCalc(tcpSeg, srcIP, dstIP)
				binary.BigEndian.PutUint16(pkt[csumOff:csumOff+2], newCsum)
			}
			break
		}
		i += optLen
	}
	return pkt
}

// tcpChecksumCalc computes TCP checksum with pseudo-header (used after MSS clamp).
func tcpChecksumCalc(tcpSeg, srcIP, dstIP []byte) uint16 {
	pLen := len(tcpSeg)
	pseudo := make([]byte, 12+pLen)
	copy(pseudo[0:4], srcIP)
	copy(pseudo[4:8], dstIP)
	pseudo[9] = 6
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(pLen))
	copy(pseudo[12:], tcpSeg)
	pseudo[12+16] = 0
	pseudo[12+17] = 0
	var sum uint32
	for i := 0; i < len(pseudo)-1; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(pseudo[i : i+2]))
	}
	if len(pseudo)%2 == 1 {
		sum += uint32(pseudo[len(pseudo)-1]) << 8
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}

// tunCompress compresses a TUN packet, but skips deflate for traffic that is
// likely already compressed (HTTPS bulk, RTP/QUIC media, etc.) to save CPU.
func tunCompress(pkt []byte) []byte {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return Compress(pkt)
	}
	ihl := int(pkt[0]&0x0f) * 4
	proto := pkt[9]

	// UDP: skip compression for QUIC (443), RTP range (5004-5100, even ports),
	// and high-port media (>= 16384, common for WebRTC/RTP)
	if proto == 17 && len(pkt) >= ihl+4 {
		dstPort := binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
		if dstPort == 443 || // QUIC
			(dstPort >= 5004 && dstPort <= 5100 && dstPort%2 == 0) || // RTP
			dstPort >= 16384 { // WebRTC media
			return compressRaw(pkt)
		}
	}

	// Large TCP packets (>512 bytes payload) on port 443 are likely TLS encrypted data
	if proto == 6 && len(pkt) >= ihl+20 {
		dstPort := binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
		srcPort := binary.BigEndian.Uint16(pkt[ihl : ihl+2])
		dataOff := int(pkt[ihl+12]>>4) * 4
		payloadLen := len(pkt) - ihl - dataOff
		if payloadLen > 512 && (dstPort == 443 || srcPort == 443) {
			return compressRaw(pkt)
		}
	}

	return Compress(pkt)
}

// compressRaw wraps data with the 0x00 (uncompressed) header, skipping deflate.
func compressRaw(data []byte) []byte {
	out := make([]byte, 1+len(data))
	out[0] = 0x00
	copy(out[1:], data)
	return out
}

// tunSendUDP sends VPN data over the UDP P2P connection if available.
// Used when Direct TCP fails but UDP hole punch succeeded.
// Format: "VPN:" + compressed data (simple prefix to distinguish from PUNCH/KEY messages)
func (c *Client) tunSendUDP(peerID string, compressed []byte) bool {
	c.peerConnsMu.RLock()
	pc := c.peerConns[peerID]
	c.peerConnsMu.RUnlock()

	if pc == nil || pc.Mode != "direct" || pc.UDPAddr == nil {
		return false
	}

	c.connMu.Lock()
	udp := c.udpConn
	c.connMu.Unlock()
	if udp == nil {
		return false
	}

	// Prefix with "VPN:" to distinguish from other UDP messages
	msg := make([]byte, 4+len(compressed))
	copy(msg[:4], []byte("VPN:"))
	copy(msg[4:], compressed)

	_, err := udp.WriteToUDP(msg, pc.UDPAddr)
	return err == nil
}

// applyReverseSNAT checks if a packet from TUN is a reply to a SNAT'd connection.
// If so, rewrites destination IP from SNAT IP back to the peer's virtual IP.
func (dev *TunDevice) applyReverseSNAT(pkt []byte) []byte {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return pkt // pass through non-IPv4
	}

	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl {
		return pkt
	}

	dstIP := net.IP(pkt[16:20])

	// Only process packets destined for our SNAT IP
	if !dstIP.Equal(dev.snatIP) {
		// Normal traffic (peer-to-peer or other) — pass through
		return pkt
	}

	srcIP := net.IP(pkt[12:16])
	proto := pkt[9]
	var natKey string

	// Build NAT key matching the original direction
	switch proto {
	case 6, 17: // TCP, UDP
		if len(pkt) >= ihl+4 {
			srcPort := binary.BigEndian.Uint16(pkt[ihl : ihl+2])
			dstPort := binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
			// Original key was: proto:dstIP:dstPort:srcPort
			// In reply: src=originalDst, srcPort=originalDstPort, dstPort=originalSrcPort
			natKey = fmt.Sprintf("%d:%s:%d:%d", proto, srcIP, srcPort, dstPort)
		}
	case 1: // ICMP
		if len(pkt) >= ihl+6 {
			icmpType := pkt[ihl]
			if icmpType == 0 { // Echo Reply
				icmpID := binary.BigEndian.Uint16(pkt[ihl+4 : ihl+6])
				natKey = fmt.Sprintf("icmp:%s:%d", srcIP, icmpID)
			}
		}
	}

	if natKey == "" {
		return pkt
	}

	val, ok := dev.natTable.Load(natKey)
	if !ok {
		return pkt // no NAT entry, pass through
	}

	entry := val.(*natEntry)
	entry.lastSeen = time.Now()

	// Rewrite destination IP from SNAT IP back to original source (peer's virtual IP)
	origDst := entry.originalSrcIP.To4()
	copy(pkt[16:20], origDst)

	// Recalculate IP header checksum
	pkt[10] = 0
	pkt[11] = 0
	cksum := ipHeaderChecksum(pkt[:ihl])
	binary.BigEndian.PutUint16(pkt[10:12], cksum)

	// Fix TCP/UDP checksum
	if proto == 6 && len(pkt) >= ihl+18 {
		fixTransportChecksum(pkt, ihl, dev.snatIP.To4(), origDst, true)
	} else if proto == 17 && len(pkt) >= ihl+8 {
		fixTransportChecksum(pkt, ihl, dev.snatIP.To4(), origDst, false)
	}
	// ICMP checksum doesn't include IP pseudo-header, no fix needed

	return pkt
}

// TunStatus returns a snapshot of the current TUN VPN state.
func (c *Client) TunStatus() TunInfo {
	c.tunMu.RLock()
	dev := c.tunDevice
	c.tunMu.RUnlock()

	if dev == nil {
		return TunInfo{}
	}

	bytesUp := atomic.LoadInt64(&dev.bytesUp)
	bytesDown := atomic.LoadInt64(&dev.bytesDown)
	lastUp := atomic.LoadInt64(&dev.lastUp)
	lastDown := atomic.LoadInt64(&dev.lastDown)

	rateUp := float64(bytesUp - lastUp)
	rateDown := float64(bytesDown - lastDown)
	atomic.StoreInt64(&dev.lastUp, bytesUp)
	atomic.StoreInt64(&dev.lastDown, bytesDown)

	subnetStr := ""
	if dev.subnet != nil {
		subnetStr = dev.subnet.String()
	}
	exitStr := ""
	if dev.exitIP != nil {
		exitStr = dev.exitIP.String()
	}
	snatStr := ""
	if dev.snatIP != nil {
		snatStr = dev.snatIP.String()
	}

	return TunInfo{
		Enabled:   true,
		VirtualIP: dev.virtualIP.String(),
		PeerIP:    dev.peerIP.String(),
		Subnet:    subnetStr,
		Routes:    dev.routes,
		ExitIP:    exitStr,
		SNATIP:    snatStr,
		PeerID:    dev.peerID,
		PeerName:  dev.peerName,
		BytesUp:   bytesUp,
		BytesDown: bytesDown,
		RateUp:    rateUp,
		RateDown:  rateDown,
	}
}

// tunCleanup is called during Disconnect to stop any active TUN.
func (c *Client) tunCleanup() {
	c.tunMu.Lock()
	dev := c.tunDevice
	c.tunDevice = nil
	c.tunMu.Unlock()

	if dev == nil {
		return
	}

	c.sendRelay(dev.peerID, "tun_teardown", TunTeardown{})

	dev.closeOnce.Do(func() { close(dev.done) })
	if dev.nsProxy != nil {
		dev.nsProxy.Close()
	}
	if dev.proxy != nil {
		dev.proxy.Close()
	}
	if dev.iface != nil {
		dev.iface.Close()
	}
	for _, route := range dev.routes {
		removeRoute(dev.ifName, route)
	}
	cleanupSNATRoute(dev.ifName, dev.snatIP)
	disableNAT(dev.ifName)
	removeTunInterface(dev.ifName)
	removeServerRoute(dev.serverHost)
}

// ipHeaderChecksum computes the IP header checksum.
func ipHeaderChecksum(header []byte) uint16 {
	var sum uint32
	for i := 0; i < len(header)-1; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(header[i : i+2]))
	}
	if len(header)%2 == 1 {
		sum += uint32(header[len(header)-1]) << 8
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}

// fixTransportChecksum incrementally updates TCP/UDP checksum after IP address change.
// Uses RFC 1624 incremental checksum update.
func fixTransportChecksum(pkt []byte, ihl int, oldIP, newIP net.IP, isTCP bool) {
	var csumOff int
	if isTCP {
		csumOff = ihl + 16 // TCP checksum at offset 16 in TCP header
	} else {
		csumOff = ihl + 6 // UDP checksum at offset 6 in UDP header
	}
	if len(pkt) < csumOff+2 {
		return
	}

	oldCsum := binary.BigEndian.Uint16(pkt[csumOff : csumOff+2])
	if !isTCP && oldCsum == 0 {
		return // UDP checksum 0 means not computed
	}

	// Incremental update: subtract old IP words, add new IP words
	csum := uint32(^oldCsum)
	for i := 0; i < 4; i += 2 {
		csum -= uint32(binary.BigEndian.Uint16(oldIP[i : i+2]))
		csum += uint32(binary.BigEndian.Uint16(newIP[i : i+2]))
	}
	// Fold carry
	for csum > 0xffff {
		csum = (csum >> 16) + (csum & 0xffff)
	}
	binary.BigEndian.PutUint16(pkt[csumOff:csumOff+2], ^uint16(csum))
}

// cleanupNATTable removes stale NAT entries (called periodically or on stop).
func (dev *TunDevice) cleanupNATTable() {
	cutoff := time.Now().Add(-5 * time.Minute)
	dev.natTable.Range(func(key, value any) bool {
		if entry, ok := value.(*natEntry); ok {
			if entry.lastSeen.Before(cutoff) {
				dev.natTable.Delete(key)
			}
		}
		return true
	})
}
