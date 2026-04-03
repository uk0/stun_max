package natcheck

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// STUN constants
const (
	stunMagicCookie    uint32 = 0x2112A442
	stunBindingRequest uint16 = 0x0001
	stunAttrXorMapped  uint16 = 0x0020
	stunAttrMapped     uint16 = 0x0001
	stunHeaderSize            = 20
	stunTimeout               = 3 * time.Second
)

// ANSI colors
const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
	gray   = "\033[90m"
)

// NAT types (RFC 3489 classification)
const (
	NATOpen              = "Open Internet"
	NATFullCone          = "Full Cone NAT"
	NATRestrictedCone    = "Restricted Cone NAT"
	NATPortRestricted    = "Port Restricted Cone NAT"
	NATSymmetric         = "Symmetric NAT"
	NATSymmetricFirewall = "Symmetric UDP Firewall"
	NATBlocked           = "UDP Blocked"
)

// STUNResult holds the result of a single STUN query
type STUNResult struct {
	Server     string
	PublicAddr string
	PublicIP   string
	PublicPort int
	LocalPort  int
	Latency    time.Duration
	Error      error
}

// NATReport is the final diagnostic report
type NATReport struct {
	LocalIP       string
	Results       []STUNResult
	NATType       string
	PortConsistent bool
	IPConsistent   bool
	HairpinOK      bool
	HolePunchProb  string // "High", "Medium", "Low", "None"
}

// Default STUN servers to test against
var defaultSTUNServers = []string{
	"stun.cloudflare.com:3478",
	"stun.cloudflare.com:3478",
	"stun.miwifi.com:3478",
	"stun.chat.bilibili.com:3478",
	"stun.l.google.com:19302",
}

// Run executes the NAT diagnostic CLI (parses flags from os.Args).
func Run() {
	servers := flag.String("servers", "", "comma-separated STUN servers (default: built-in list)")
	verbose := flag.Bool("v", false, "verbose output")
	flag.Parse()

	stunServers := defaultSTUNServers
	if *servers != "" {
		stunServers = strings.Split(*servers, ",")
		for i := range stunServers {
			stunServers[i] = strings.TrimSpace(stunServers[i])
		}
	}

	printBanner()

	report := runDiagnostics(stunServers, *verbose)
	printReport(report)
}

func printBanner() {
	fmt.Printf("\n%s%s  STUN Max - NAT Diagnostic Tool%s\n", bold, cyan, reset)
	fmt.Printf("%s  Detecting NAT type and hole punch feasibility%s\n\n", gray, reset)
}

// --- STUN Protocol Implementation ---

func buildSTUNRequest() ([]byte, []byte) {
	req := make([]byte, stunHeaderSize)
	binary.BigEndian.PutUint16(req[0:2], stunBindingRequest)
	binary.BigEndian.PutUint16(req[2:4], 0)
	binary.BigEndian.PutUint32(req[4:8], stunMagicCookie)
	txID := make([]byte, 12)
	rand.Read(txID)
	copy(req[8:20], txID)
	return req, txID
}

func parseSTUNResponse(resp []byte, txID []byte) (string, int, error) {
	if len(resp) < stunHeaderSize {
		return "", 0, fmt.Errorf("response too short: %d bytes", len(resp))
	}
	msgType := binary.BigEndian.Uint16(resp[0:2])
	if msgType != 0x0101 {
		return "", 0, fmt.Errorf("unexpected message type: 0x%04x", msgType)
	}
	if !bytes.Equal(resp[8:20], txID) {
		return "", 0, fmt.Errorf("transaction ID mismatch")
	}

	msgLen := int(binary.BigEndian.Uint16(resp[2:4]))
	if stunHeaderSize+msgLen > len(resp) {
		return "", 0, fmt.Errorf("truncated response")
	}
	attrs := resp[stunHeaderSize : stunHeaderSize+msgLen]

	// Try XOR-MAPPED-ADDRESS first, then MAPPED-ADDRESS
	ip, port, err := findAddress(attrs, stunAttrXorMapped, true)
	if err != nil {
		ip, port, err = findAddress(attrs, stunAttrMapped, false)
	}
	return ip, port, err
}

func findAddress(attrs []byte, targetType uint16, xor bool) (string, int, error) {
	offset := 0
	for offset+4 <= len(attrs) {
		attrType := binary.BigEndian.Uint16(attrs[offset : offset+2])
		attrLen := int(binary.BigEndian.Uint16(attrs[offset+2 : offset+4]))
		offset += 4
		if offset+attrLen > len(attrs) {
			break
		}
		if attrType == targetType {
			return decodeAddress(attrs[offset:offset+attrLen], xor)
		}
		offset += attrLen
		if attrLen%4 != 0 {
			offset += 4 - (attrLen % 4)
		}
	}
	return "", 0, fmt.Errorf("address attribute 0x%04x not found", targetType)
}

func decodeAddress(data []byte, xor bool) (string, int, error) {
	if len(data) < 8 {
		return "", 0, fmt.Errorf("address data too short")
	}
	family := data[1]
	if family != 0x01 {
		return "", 0, fmt.Errorf("unsupported family: 0x%02x", family)
	}

	rawPort := binary.BigEndian.Uint16(data[2:4])
	rawIP := binary.BigEndian.Uint32(data[4:8])

	var port uint16
	var ip uint32
	if xor {
		port = rawPort ^ uint16(stunMagicCookie>>16)
		ip = rawIP ^ stunMagicCookie
	} else {
		port = rawPort
		ip = rawIP
	}

	ipStr := fmt.Sprintf("%d.%d.%d.%d", byte(ip>>24), byte(ip>>16), byte(ip>>8), byte(ip))
	return ipStr, int(port), nil
}

// --- STUN Query Functions ---

// querySTUN sends a STUN request from a specific local UDP conn
func querySTUN(conn *net.UDPConn, server string) STUNResult {
	start := time.Now()
	result := STUNResult{Server: server}

	serverAddr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		result.Error = fmt.Errorf("resolve: %w", err)
		return result
	}

	req, txID := buildSTUNRequest()

	conn.SetWriteDeadline(time.Now().Add(stunTimeout))
	if _, err := conn.WriteToUDP(req, serverAddr); err != nil {
		result.Error = fmt.Errorf("send: %w", err)
		return result
	}

	conn.SetReadDeadline(time.Now().Add(stunTimeout))
	buf := make([]byte, 1024)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		result.Error = fmt.Errorf("recv: %w", err)
		return result
	}
	result.Latency = time.Since(start)

	ip, port, err := parseSTUNResponse(buf[:n], txID)
	if err != nil {
		result.Error = err
		return result
	}

	result.PublicIP = ip
	result.PublicPort = port
	result.PublicAddr = fmt.Sprintf("%s:%d", ip, port)
	result.LocalPort = conn.LocalAddr().(*net.UDPAddr).Port
	return result
}

// querySTUNFresh creates a new UDP socket and queries a STUN server
func querySTUNFresh(server string) STUNResult {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return STUNResult{Server: server, Error: fmt.Errorf("listen: %w", err)}
	}
	defer conn.Close()
	return querySTUN(conn, server)
}

// --- Diagnostic Tests ---

func runDiagnostics(stunServers []string, verbose bool) NATReport {
	report := NATReport{}

	// Detect local IP
	report.LocalIP = getLocalIP()
	printStep("Local IP", report.LocalIP)

	// Test 1: Basic STUN reachability from same socket (port mapping consistency)
	fmt.Printf("\n%s%s[Test 1] STUN Reachability & Port Mapping%s\n", bold, cyan, reset)

	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		fmt.Printf("  %sFailed to create UDP socket: %v%s\n", red, err, reset)
		report.NATType = NATBlocked
		report.HolePunchProb = "None"
		return report
	}
	localPort := conn.LocalAddr().(*net.UDPAddr).Port
	fmt.Printf("  Local UDP port: %d\n", localPort)

	// Query multiple STUN servers from the SAME socket
	var sameSocketResults []STUNResult
	for _, server := range stunServers {
		r := querySTUN(conn, server)
		r.LocalPort = localPort
		sameSocketResults = append(sameSocketResults, r)
		if r.Error != nil {
			if verbose {
				fmt.Printf("  %s✗ %-35s %s%s\n", red, server, r.Error, reset)
			}
		} else {
			fmt.Printf("  %s✓ %-35s → %s  (%s)%s\n", green, server, r.PublicAddr, r.Latency.Round(time.Millisecond), reset)
		}
	}
	conn.Close()

	// Filter successful results
	var okResults []STUNResult
	for _, r := range sameSocketResults {
		if r.Error == nil {
			okResults = append(okResults, r)
		}
	}

	if len(okResults) == 0 {
		fmt.Printf("\n  %sNo STUN server reachable. UDP may be blocked.%s\n", red, reset)
		report.NATType = NATBlocked
		report.HolePunchProb = "None"
		report.Results = sameSocketResults
		return report
	}

	report.Results = sameSocketResults

	// Check IP consistency (same public IP from all servers?)
	ips := map[string]bool{}
	ports := map[int]bool{}
	for _, r := range okResults {
		ips[r.PublicIP] = true
		ports[r.PublicPort] = true
	}
	report.IPConsistent = len(ips) == 1
	report.PortConsistent = len(ports) == 1

	fmt.Printf("\n  Same-socket mapping: ")
	if report.PortConsistent && report.IPConsistent {
		fmt.Printf("%s✓ Consistent (same IP:port for all servers)%s\n", green, reset)
	} else if report.IPConsistent && !report.PortConsistent {
		fmt.Printf("%s✗ Port varies per destination (Symmetric NAT indicator)%s\n", red, reset)
	} else {
		fmt.Printf("%s✗ IP varies (multi-homed or carrier-grade NAT)%s\n", yellow, reset)
	}

	// Test 2: Different sockets → same server (port allocation pattern)
	fmt.Printf("\n%s%s[Test 2] Port Allocation Pattern%s\n", bold, cyan, reset)

	bestServer := okResults[0].Server
	var diffSocketPorts []int
	for i := 0; i < 3; i++ {
		r := querySTUNFresh(bestServer)
		if r.Error == nil {
			diffSocketPorts = append(diffSocketPorts, r.PublicPort)
			fmt.Printf("  Socket %d: local :%d → public :%d\n", i+1, r.LocalPort, r.PublicPort)
		}
	}

	portAlloc := "unknown"
	if len(diffSocketPorts) >= 3 {
		sort.Ints(diffSocketPorts)
		d1 := diffSocketPorts[1] - diffSocketPorts[0]
		d2 := diffSocketPorts[2] - diffSocketPorts[1]
		if d1 == d2 && d1 > 0 && d1 <= 4 {
			portAlloc = fmt.Sprintf("sequential (delta=%d)", d1)
			fmt.Printf("  Pattern: %s%s%s — predictable, good for hole punch\n", green, portAlloc, reset)
		} else if d1 == 0 && d2 == 0 {
			portAlloc = "port-preserving"
			fmt.Printf("  Pattern: %s%s%s — excellent for hole punch\n", green, portAlloc, reset)
		} else {
			portAlloc = "random"
			fmt.Printf("  Pattern: %s%s%s — harder to hole punch\n", yellow, portAlloc, reset)
		}
	}

	// Test 3: Hairpin test (can we send to our own public address?)
	fmt.Printf("\n%s%s[Test 3] Hairpin NAT (loopback through NAT)%s\n", bold, cyan, reset)
	report.HairpinOK = testHairpin(okResults[0].PublicAddr)
	if report.HairpinOK {
		fmt.Printf("  %s✓ Hairpin supported%s\n", green, reset)
	} else {
		fmt.Printf("  %s✗ Hairpin not supported (normal for most NATs)%s\n", yellow, reset)
	}

	// Test 4: UDP timeout / binding lifetime estimate
	fmt.Printf("\n%s%s[Test 4] NAT Binding Lifetime%s\n", bold, cyan, reset)
	lifetime := testBindingLifetime(bestServer)
	if lifetime > 0 {
		fmt.Printf("  Binding alive after %s: %s✓ OK%s\n", lifetime, green, reset)
	} else {
		fmt.Printf("  %sCould not determine binding lifetime%s\n", yellow, reset)
	}

	// Determine NAT type
	report.NATType = classifyNAT(report, portAlloc)

	// Determine hole punch probability
	report.HolePunchProb = assessHolePunch(report, portAlloc)

	return report
}

func testHairpin(publicAddr string) bool {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return false
	}
	defer conn.Close()

	addr, err := net.ResolveUDPAddr("udp4", publicAddr)
	if err != nil {
		return false
	}

	token := make([]byte, 8)
	rand.Read(token)
	msg := append([]byte("HAIRPIN:"), token...)

	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	conn.WriteToUDP(msg, addr)

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return false
	}
	return bytes.Contains(buf[:n], token)
}

func testBindingLifetime(server string) time.Duration {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return 0
	}
	defer conn.Close()

	// First query to establish binding
	r1 := querySTUN(conn, server)
	if r1.Error != nil {
		return 0
	}

	// Wait 10 seconds, then query again from same socket
	time.Sleep(10 * time.Second)

	r2 := querySTUN(conn, server)
	if r2.Error != nil {
		return 0
	}

	if r1.PublicAddr == r2.PublicAddr {
		return 10 * time.Second
	}
	return 0
}

func classifyNAT(report NATReport, portAlloc string) string {
	if report.LocalIP != "" {
		for _, r := range report.Results {
			if r.Error == nil && r.PublicIP == report.LocalIP {
				if report.PortConsistent {
					return NATOpen
				}
				return NATSymmetricFirewall
			}
		}
	}

	if !report.PortConsistent {
		return NATSymmetric
	}

	// Port consistent = cone NAT of some kind
	// Without a second STUN server IP we can't distinguish full/restricted/port-restricted
	// but port-consistent is the key signal
	if portAlloc == "port-preserving" {
		return NATFullCone
	}
	return NATRestrictedCone
}

func assessHolePunch(report NATReport, portAlloc string) string {
	switch report.NATType {
	case NATOpen:
		return "High"
	case NATFullCone:
		return "High"
	case NATRestrictedCone:
		return "High"
	case NATPortRestricted:
		return "Medium"
	case NATSymmetric:
		if portAlloc == "sequential" {
			return "Medium"
		}
		return "Low"
	case NATSymmetricFirewall:
		return "Low"
	case NATBlocked:
		return "None"
	}
	return "Unknown"
}

// --- Report Output ---

func printReport(report NATReport) {
	fmt.Printf("\n%s%s══════════════════════════════════════════%s\n", bold, cyan, reset)
	fmt.Printf("%s%s  NAT Diagnostic Report%s\n", bold, cyan, reset)
	fmt.Printf("%s%s══════════════════════════════════════════%s\n\n", bold, cyan, reset)

	fmt.Printf("  %-22s %s\n", "Local IP:", report.LocalIP)

	// Show all discovered public addresses
	seen := map[string]bool{}
	for _, r := range report.Results {
		if r.Error == nil && !seen[r.PublicAddr] {
			seen[r.PublicAddr] = true
			fmt.Printf("  %-22s %s\n", "Public Address:", r.PublicAddr)
		}
	}

	// NAT Type with color
	natColor := green
	switch report.NATType {
	case NATSymmetric, NATSymmetricFirewall:
		natColor = red
	case NATPortRestricted:
		natColor = yellow
	case NATBlocked:
		natColor = red
	}
	fmt.Printf("  %-22s %s%s%s\n", "NAT Type:", natColor, report.NATType, reset)

	// Port mapping
	if report.PortConsistent {
		fmt.Printf("  %-22s %s✓ Consistent%s\n", "Port Mapping:", green, reset)
	} else {
		fmt.Printf("  %-22s %s✗ Inconsistent%s\n", "Port Mapping:", red, reset)
	}

	// IP mapping
	if report.IPConsistent {
		fmt.Printf("  %-22s %s✓ Consistent%s\n", "IP Mapping:", green, reset)
	} else {
		fmt.Printf("  %-22s %s✗ Inconsistent%s\n", "IP Mapping:", red, reset)
	}

	// Hairpin
	if report.HairpinOK {
		fmt.Printf("  %-22s %s✓ Supported%s\n", "Hairpin NAT:", green, reset)
	} else {
		fmt.Printf("  %-22s %s✗ Not supported%s\n", "Hairpin NAT:", gray, reset)
	}

	// Hole punch probability
	probColor := green
	switch report.HolePunchProb {
	case "Medium":
		probColor = yellow
	case "Low":
		probColor = red
	case "None":
		probColor = red
	}
	fmt.Printf("\n  %-22s %s%s%s%s\n", "Hole Punch Success:", bold, probColor, report.HolePunchProb, reset)

	// Recommendation
	fmt.Printf("\n%s  Recommendation:%s\n", bold, reset)
	switch report.HolePunchProb {
	case "High":
		fmt.Printf("  %s✓ Your network is well-suited for P2P hole punching.%s\n", green, reset)
		fmt.Printf("  %s  Direct connections should work with most peers.%s\n", green, reset)
	case "Medium":
		fmt.Printf("  %s~ P2P may work with some peers.%s\n", yellow, reset)
		fmt.Printf("  %s  Peers behind Full Cone / Restricted Cone NAT should connect directly.%s\n", yellow, reset)
		fmt.Printf("  %s  Peers behind Symmetric NAT will need relay fallback.%s\n", yellow, reset)
	case "Low":
		fmt.Printf("  %s✗ P2P hole punching is unlikely to succeed.%s\n", red, reset)
		fmt.Printf("  %s  Most connections will use server relay.%s\n", red, reset)
		fmt.Printf("  %s  Consider using a TURN server or VPN for better connectivity.%s\n", red, reset)
	case "None":
		fmt.Printf("  %s✗ UDP is blocked. Only TCP relay will work.%s\n", red, reset)
		fmt.Printf("  %s  Check firewall settings or use a different network.%s\n", red, reset)
	}

	// Latency summary
	var latencies []time.Duration
	for _, r := range report.Results {
		if r.Error == nil {
			latencies = append(latencies, r.Latency)
		}
	}
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		min := latencies[0]
		max := latencies[len(latencies)-1]
		var sum time.Duration
		for _, l := range latencies {
			sum += l
		}
		avg := sum / time.Duration(len(latencies))
		fmt.Printf("\n  %-22s min=%s  avg=%s  max=%s\n",
			"STUN Latency:",
			min.Round(time.Millisecond),
			avg.Round(time.Millisecond),
			max.Round(time.Millisecond))
	}

	// Compatibility matrix
	fmt.Printf("\n%s  Compatibility with peer NAT types:%s\n", bold, reset)
	fmt.Printf("  %-28s %s\n", "Peer NAT Type", "P2P Possible?")
	fmt.Printf("  %s────────────────────────────────────────%s\n", gray, reset)

	type compat struct {
		peerNAT string
		result  string
		color   string
	}

	var matrix []compat
	switch report.NATType {
	case NATOpen, NATFullCone:
		matrix = []compat{
			{"Open / Full Cone", "✓ Yes", green},
			{"Restricted Cone", "✓ Yes", green},
			{"Port Restricted", "✓ Yes", green},
			{"Symmetric", "✓ Yes", green},
		}
	case NATRestrictedCone:
		matrix = []compat{
			{"Open / Full Cone", "✓ Yes", green},
			{"Restricted Cone", "✓ Yes", green},
			{"Port Restricted", "✓ Yes", green},
			{"Symmetric", "~ Maybe", yellow},
		}
	case NATPortRestricted:
		matrix = []compat{
			{"Open / Full Cone", "✓ Yes", green},
			{"Restricted Cone", "✓ Yes", green},
			{"Port Restricted", "✓ Yes", green},
			{"Symmetric", "✗ No", red},
		}
	case NATSymmetric:
		matrix = []compat{
			{"Open / Full Cone", "✓ Yes", green},
			{"Restricted Cone", "~ Maybe", yellow},
			{"Port Restricted", "✗ No", red},
			{"Symmetric", "✗ No", red},
		}
	default:
		matrix = []compat{
			{"Any", "✗ UDP Blocked", red},
		}
	}

	for _, m := range matrix {
		fmt.Printf("  %-28s %s%s%s\n", m.peerNAT, m.color, m.result, reset)
	}

	fmt.Println()
}

// --- Helpers ---

func printStep(label, value string) {
	fmt.Printf("  %-22s %s\n", label+":", value)
}

func getLocalIP() string {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return "unknown"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// --- Parallel STUN query helper (unused but available) ---

func querySTUNParallel(servers []string) []STUNResult {
	var wg sync.WaitGroup
	results := make([]STUNResult, len(servers))
	for i, server := range servers {
		wg.Add(1)
		go func(idx int, srv string) {
			defer wg.Done()
			results[idx] = querySTUNFresh(srv)
		}(i, server)
	}
	wg.Wait()
	return results
}
