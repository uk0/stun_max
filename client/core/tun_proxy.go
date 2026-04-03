package core

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// subnetProxy handles proxying packets destined for routed subnets.
// Instead of writing to TUN (which requires kernel NAT), it uses
// Go's network stack to send packets directly — like EasyTier's approach.
type subnetProxy struct {
	dev       *TunDevice
	client    *Client
	icmpConn  net.PacketConn
	udpConns  sync.Map // "srcIP:srcPort:dstIP:dstPort" → *udpProxyConn
	tcpConns  sync.Map // "srcIP:srcPort:dstIP:dstPort" → *tcpProxyConn
	done      chan struct{}
	closeOnce sync.Once
}

type udpProxyConn struct {
	conn     net.PacketConn
	srcIP    net.IP
	srcPort  uint16
	dstIP    net.IP
	dstPort  uint16
	lastSeen time.Time
}

type tcpProxyConn struct {
	conn     net.Conn // real TCP connection to destination
	srcIP    net.IP
	srcPort  uint16
	dstIP    net.IP
	dstPort  uint16
	peerSeq  uint32 // next expected sequence number from peer
	ourSeq   uint32 // our sequence number for data sent to peer
	state    int    // 0=SYN_RECV, 1=ESTABLISHED, 2=CLOSING
	mu       sync.Mutex
	lastSeen time.Time
}

// sendTunPacket sends a raw IP packet to the peer via UDP P2P or relay.
func (sp *subnetProxy) sendTunPacket(pkt []byte) {
	compressed := Compress(pkt)

	// Try UDP P2P first
	if sp.client.tunSendUDP(sp.dev.peerID, compressed) {
		return
	}
	// Fall back to relay
	encoded := base64.StdEncoding.EncodeToString(compressed)
	sp.client.sendRelay(sp.dev.peerID, "tun_data", TunData{Data: encoded})
}

func newSubnetProxy(dev *TunDevice, client *Client) *subnetProxy {
	sp := &subnetProxy{
		dev:    dev,
		client: client,
		done:   make(chan struct{}),
	}
	// Open raw ICMP socket for ping proxy
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err == nil {
		sp.icmpConn = conn
		go sp.icmpReadLoop()
	}
	// Cleanup stale UDP connections periodically
	go sp.cleanupLoop()
	return sp
}

func (sp *subnetProxy) Close() {
	sp.closeOnce.Do(func() {
		close(sp.done)
		if sp.icmpConn != nil {
			sp.icmpConn.Close()
		}
		sp.udpConns.Range(func(key, val any) bool {
			if uc, ok := val.(*udpProxyConn); ok {
				uc.conn.Close()
			}
			sp.udpConns.Delete(key)
			return true
		})
		sp.tcpConns.Range(func(key, val any) bool {
			if tc, ok := val.(*tcpProxyConn); ok {
				tc.conn.Close()
			}
			sp.tcpConns.Delete(key)
			return true
		})
	})
}

// handlePacket processes an IP packet destined for a routed subnet.
// Returns true if the packet was handled (proxied), false if it should be written to TUN.
func (sp *subnetProxy) handlePacket(pkt []byte) bool {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl {
		return false
	}

	dstIP := net.IP(pkt[16:20])
	srcIP := net.IP(pkt[12:16])
	proto := pkt[9]

	// Check if destination is in a routed subnet
	inRoute := false
	for _, rn := range sp.dev.routeNets {
		if rn.Contains(dstIP) {
			inRoute = true
			break
		}
	}
	if !inRoute {
		return false
	}

	switch proto {
	case 1: // ICMP — still handled by legacy proxy (raw socket)
		return sp.handleICMP(pkt, ihl, srcIP, dstIP)
	}
	// TCP and UDP now handled by netstackProxy (gVisor)
	return false
}

// handleICMP proxies ICMP echo requests through Go's network stack.
func (sp *subnetProxy) handleICMP(pkt []byte, ihl int, srcIP, dstIP net.IP) bool {
	if sp.icmpConn == nil || len(pkt) < ihl+8 {
		return false
	}
	icmpType := pkt[ihl]
	if icmpType != 8 { // Only handle Echo Request
		return false
	}

	// Extract ICMP ID and sequence for matching reply
	icmpID := binary.BigEndian.Uint16(pkt[ihl+4 : ihl+6])
	icmpSeq := binary.BigEndian.Uint16(pkt[ihl+6 : ihl+8])
	icmpPayload := pkt[ihl+8:]

	// Build and send ICMP echo request via Go's network stack
	msg := &icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   int(icmpID),
			Seq:  int(icmpSeq),
			Data: icmpPayload,
		},
	}
	wb, err := msg.Marshal(nil)
	if err != nil {
		return false
	}

	dst := &net.IPAddr{IP: dstIP}
	sp.icmpConn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err = sp.icmpConn.WriteTo(wb, dst)
	if err != nil {
		return false
	}

	// Store mapping for reply: dstIP+icmpID → srcIP (peer's virtual IP)
	key := fmt.Sprintf("icmp:%s:%d", dstIP, icmpID)
	sp.dev.natTable.Store(key, &natEntry{
		originalSrcIP: append(net.IP{}, srcIP...),
		lastSeen:      time.Now(),
	})

	if cnt := atomic.AddInt32(&snatDebugCounter, 1); cnt <= 5 {
		sp.client.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
			"ICMP proxy #%d: %s→%s id=%d seq=%d", cnt, srcIP, dstIP, icmpID, icmpSeq)})
	}
	return true
}

// icmpReadLoop reads ICMP replies and sends them back to the peer.
func (sp *subnetProxy) icmpReadLoop() {
	buf := make([]byte, 65536)
	for {
		select {
		case <-sp.done:
			return
		default:
		}

		sp.icmpConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, peer, err := sp.icmpConn.ReadFrom(buf)
		if err != nil {
			continue
		}

		// Parse ICMP reply
		msg, err := icmp.ParseMessage(1, buf[:n]) // 1 = ICMP for IPv4
		if err != nil {
			continue
		}
		if msg.Type != ipv4.ICMPTypeEchoReply {
			continue
		}

		echo, ok := msg.Body.(*icmp.Echo)
		if !ok {
			continue
		}

		// Lookup NAT table: key is target IP + ICMP ID
		srcAddr := net.ParseIP(peer.String())
		if srcAddr == nil {
			if ipAddr, ok := peer.(*net.IPAddr); ok {
				srcAddr = ipAddr.IP
			} else {
				continue
			}
		}
		key := fmt.Sprintf("icmp:%s:%d", srcAddr, echo.ID)
		val, ok := sp.dev.natTable.Load(key)
		if !ok {
			continue
		}
		entry := val.(*natEntry)

		// Build IP packet: src=target, dst=peer's virtual IP

		// Construct full IP + ICMP reply packet
		replyPkt := buildICMPReplyPacket(srcAddr.To4(), entry.originalSrcIP.To4(),
			uint16(echo.ID), uint16(echo.Seq), echo.Data)
		if replyPkt == nil {
			continue
		}

		atomic.AddInt64(&sp.dev.bytesUp, int64(len(replyPkt)))

		// Send back to peer via direct TCP or relay
		sp.sendTunPacket(replyPkt)

		if cnt := atomic.AddInt32(&snatDebugCounter, 1); cnt <= 10 {
			sp.client.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
				"ICMP reply proxied: %s→%s id=%d seq=%d",
				srcAddr, entry.originalSrcIP, echo.ID, echo.Seq)})
		}
	}
}

// handleUDP proxies UDP packets through Go's network stack.
func (sp *subnetProxy) handleUDP(pkt []byte, ihl int, srcIP, dstIP net.IP) bool {
	if len(pkt) < ihl+8 {
		return false
	}
	srcPort := binary.BigEndian.Uint16(pkt[ihl : ihl+2])
	dstPort := binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
	udpLen := binary.BigEndian.Uint16(pkt[ihl+4 : ihl+6])
	if int(udpLen) < 8 || len(pkt) < ihl+int(udpLen) {
		return false
	}
	payload := pkt[ihl+8 : ihl+int(udpLen)]

	key := fmt.Sprintf("udp:%s:%d:%s:%d", srcIP, srcPort, dstIP, dstPort)

	// Get or create UDP proxy connection
	val, loaded := sp.udpConns.LoadOrStore(key, (*udpProxyConn)(nil))
	var uc *udpProxyConn
	if loaded && val != nil {
		uc = val.(*udpProxyConn)
	}
	if uc == nil {
		// Create new UDP connection
		conn, err := net.ListenPacket("udp4", ":0")
		if err != nil {
			return false
		}
		uc = &udpProxyConn{
			conn:    conn,
			srcIP:   append(net.IP{}, srcIP...),
			srcPort: srcPort,
			dstIP:   append(net.IP{}, dstIP...),
			dstPort: dstPort,
		}
		sp.udpConns.Store(key, uc)
		go sp.udpReadLoop(uc, key)
	}
	uc.lastSeen = time.Now()

	dst := &net.UDPAddr{IP: dstIP, Port: int(dstPort)}
	uc.conn.WriteTo(payload, dst)
	return true
}

// udpReadLoop reads UDP replies and sends them back to the peer.
func (sp *subnetProxy) udpReadLoop(uc *udpProxyConn, key string) {
	defer func() {
		uc.conn.Close()
		sp.udpConns.Delete(key)
	}()

	buf := make([]byte, 65536)
	for {
		select {
		case <-sp.done:
			return
		default:
		}

		uc.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		n, from, err := uc.conn.ReadFrom(buf)
		if err != nil {
			if time.Since(uc.lastSeen) > 120*time.Second {
				return // idle timeout
			}
			continue
		}

		fromAddr, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}

		// Build IP+UDP reply packet: src=from, dst=peer's virtual IP
		replyPkt := buildUDPReplyPacket(
			fromAddr.IP.To4(), uc.srcIP.To4(),
			uint16(fromAddr.Port), uc.srcPort,
			buf[:n])
		if replyPkt == nil {
			continue
		}

		atomic.AddInt64(&sp.dev.bytesUp, int64(len(replyPkt)))
		sp.sendTunPacket(replyPkt)
	}
}

// handleTCP proxies TCP connections through Go's network stack.
func (sp *subnetProxy) handleTCP(pkt []byte, ihl int, srcIP, dstIP net.IP) bool {
	if len(pkt) < ihl+20 {
		return false
	}
	srcPort := binary.BigEndian.Uint16(pkt[ihl : ihl+2])
	dstPort := binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
	seq := binary.BigEndian.Uint32(pkt[ihl+4 : ihl+8])
	dataOff := int(pkt[ihl+12]>>4) * 4
	flags := pkt[ihl+13]

	if dataOff < 20 || len(pkt) < ihl+dataOff {
		return false
	}

	isSYN := flags&0x02 != 0
	isACK := flags&0x10 != 0
	isFIN := flags&0x01 != 0
	isRST := flags&0x04 != 0

	key := fmt.Sprintf("tcp:%s:%d:%s:%d", srcIP, srcPort, dstIP, dstPort)

	// Handle SYN — new connection
	if isSYN && !isACK {
		// Clean up stale connection
		if old, ok := sp.tcpConns.LoadAndDelete(key); ok {
			if otc, _ := old.(*tcpProxyConn); otc != nil {
				otc.mu.Lock()
				otc.state = 2
				otc.conn.Close()
				otc.mu.Unlock()
			}
		}

		dst := net.JoinHostPort(dstIP.String(), fmt.Sprintf("%d", dstPort))
		conn, err := net.DialTimeout("tcp4", dst, 5*time.Second)
		if err != nil {
			sp.sendTCPPacket(dstIP, srcIP, dstPort, srcPort, 0, seq+1, 0x14, nil)
			return true
		}

		initSeq := randomSeq()
		tc := &tcpProxyConn{
			conn:    conn,
			srcIP:   append(net.IP{}, srcIP...),
			srcPort: srcPort,
			dstIP:   append(net.IP{}, dstIP...),
			dstPort: dstPort,
			peerSeq: seq + 1,
			ourSeq:  initSeq,
			state:   0,
		}
		sp.tcpConns.Store(key, tc)

		// SYN-ACK with MSS
		mssOpt := make([]byte, 4)
		mssOpt[0] = 2
		mssOpt[1] = 4
		binary.BigEndian.PutUint16(mssOpt[2:4], tunMSS)
		sp.sendTCPPacketOpts(dstIP, srcIP, dstPort, srcPort, tc.ourSeq, tc.peerSeq, 0x12, mssOpt, nil)
		tc.ourSeq++

		go sp.tcpReadLoop(tc, key)

		if cnt := atomic.AddInt32(&snatDebugCounter, 1); cnt <= 10 {
			sp.client.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
				"TCP proxy: %s:%d → %s:%d", srcIP, srcPort, dstIP, dstPort)})
		}
		return true
	}

	// Lookup existing connection
	val, ok := sp.tcpConns.Load(key)
	if !ok {
		return false
	}
	tc := val.(*tcpProxyConn)
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.lastSeen = time.Now()

	if isRST {
		tc.state = 2
		tc.conn.Close()
		sp.tcpConns.Delete(key)
		return true
	}

	if isACK && tc.state == 0 {
		tc.state = 1
	}

	if isFIN {
		tc.peerSeq++
		sp.sendTCPPacket(tc.dstIP, tc.srcIP, tc.dstPort, tc.srcPort, tc.ourSeq, tc.peerSeq, 0x10, nil)
		tc.conn.Close()
		tc.state = 2
		return true
	}

	// Handle data from peer → NAS
	payload := pkt[ihl+dataOff:]
	if len(payload) > 0 && tc.state == 1 {
		if seq == tc.peerSeq {
			tc.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, werr := tc.conn.Write(payload)
			if werr != nil {
				sp.sendTCPPacket(tc.dstIP, tc.srcIP, tc.dstPort, tc.srcPort, tc.ourSeq, tc.peerSeq, 0x14, nil)
				tc.conn.Close()
				tc.state = 2
				sp.tcpConns.Delete(key)
				return true
			}
			tc.peerSeq += uint32(len(payload))
		}
		// Always ACK with current peerSeq (handles both in-order and retransmit)
		sp.sendTCPPacket(tc.dstIP, tc.srcIP, tc.dstPort, tc.srcPort, tc.ourSeq, tc.peerSeq, 0x10, nil)
	}

	return true
}

// seqDiff returns the positive difference between two TCP sequence numbers,
// handling 32-bit wraparound. Returns 0 if b is not ahead of a.
func seqDiff(b, a uint32) uint32 {
	diff := b - a
	if diff > 0 && diff < 1<<31 {
		return diff
	}
	return 0
}

// tcpReadLoop reads from the real TCP connection and sends data to peer as TCP segments.
func (sp *subnetProxy) tcpReadLoop(tc *tcpProxyConn, key string) {
	defer func() {
		tc.mu.Lock()
		if tc.state != 2 {
			sp.sendTCPPacket(tc.dstIP, tc.srcIP, tc.dstPort, tc.srcPort, tc.ourSeq, tc.peerSeq, 0x11, nil)
			tc.state = 2
		}
		tc.mu.Unlock()
		tc.conn.Close()
		time.AfterFunc(30*time.Second, func() { sp.tcpConns.Delete(key) })
	}()

	// Small read buffer: 4KB = ~3 segments per read.
	// Prevents flooding the UDP tunnel with burst of 47 packets (64KB/1360).
	buf := make([]byte, 4096)
	for {
		select {
		case <-sp.done:
			return
		default:
		}

		tc.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		n, err := tc.conn.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])

			for off := 0; off < n; off += 1360 {
				end := off + 1360
				if end > n {
					end = n
				}

				tc.mu.Lock()
				if tc.state != 1 {
					tc.mu.Unlock()
					return
				}
				seq := tc.ourSeq
				ackSeq := tc.peerSeq
				tc.ourSeq += uint32(end - off)
				tc.mu.Unlock()

				flags := byte(0x10)
				if end == n {
					flags = 0x18
				}
				sp.sendTCPPacket(tc.dstIP, tc.srcIP, tc.dstPort, tc.srcPort,
					seq, ackSeq, flags, data[off:end])

				// Pace: small delay between segments to avoid flooding the UDP tunnel.
				// 200μs per segment ≈ ~50 Mbps max throughput, enough for video streaming.
				if off+1360 < n {
					time.Sleep(200 * time.Microsecond)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// randomSeq generates a cryptographically random TCP initial sequence number.
func randomSeq() uint32 {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<32-1))
	if err != nil {
		return 100000 // fallback
	}
	return uint32(n.Uint64())
}

// sendTCPPacket constructs and sends a full IPv4+TCP packet back to the peer.
func (sp *subnetProxy) sendTCPPacket(srcIP, dstIP net.IP, srcPort, dstPort uint16, seq, ack uint32, flags byte, payload []byte) {
	sp.sendTCPPacketOpts(srcIP, dstIP, srcPort, dstPort, seq, ack, flags, nil, payload)
}

// tcpIPID is an incrementing IP identification counter for TCP proxy packets.
var tcpIPID uint32

// sendTCPPacketOpts constructs and sends a full IPv4+TCP packet with optional TCP options.
func (sp *subnetProxy) sendTCPPacketOpts(srcIP, dstIP net.IP, srcPort, dstPort uint16, seq, ack uint32, flags byte, tcpOpts []byte, payload []byte) {
	// Pad TCP options to 4-byte boundary
	optsLen := len(tcpOpts)
	optsPadded := optsLen
	if optsPadded%4 != 0 {
		optsPadded += 4 - (optsPadded % 4)
	}
	tcpHdrLen := 20 + optsPadded
	totalLen := 20 + tcpHdrLen + len(payload)
	pkt := make([]byte, totalLen)

	// IPv4 header
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(totalLen))
	ipID := atomic.AddUint32(&tcpIPID, 1)
	binary.BigEndian.PutUint16(pkt[4:6], uint16(ipID)) // IP identification
	pkt[6] = 0x40 // DF (Don't Fragment)
	pkt[8] = 64   // TTL
	pkt[9] = 6    // TCP
	copy(pkt[12:16], srcIP.To4())
	copy(pkt[16:20], dstIP.To4())

	// TCP header
	binary.BigEndian.PutUint16(pkt[20:22], srcPort)
	binary.BigEndian.PutUint16(pkt[22:24], dstPort)
	binary.BigEndian.PutUint32(pkt[24:28], seq)
	binary.BigEndian.PutUint32(pkt[28:32], ack)
	pkt[32] = byte(tcpHdrLen/4) << 4 // data offset
	pkt[33] = flags
	binary.BigEndian.PutUint16(pkt[34:36], 65535) // window size

	// TCP options
	if optsLen > 0 {
		copy(pkt[40:40+optsLen], tcpOpts)
	}

	// Payload
	if len(payload) > 0 {
		copy(pkt[20+tcpHdrLen:], payload)
	}

	// TCP checksum (with pseudo-header)
	tcpCksum := tcpChecksumFull(pkt[20:], srcIP.To4(), dstIP.To4())
	binary.BigEndian.PutUint16(pkt[36:38], tcpCksum)

	// IP checksum
	cksum := ipHeaderChecksum(pkt[:20])
	binary.BigEndian.PutUint16(pkt[10:12], cksum)

	atomic.AddInt64(&sp.dev.bytesUp, int64(len(pkt)))
	sp.sendTunPacket(pkt)
}

// tcpChecksumFull computes TCP checksum including pseudo-header.
func tcpChecksumFull(tcpSegment []byte, srcIP, dstIP []byte) uint16 {
	// Pseudo-header: srcIP(4) + dstIP(4) + zero(1) + proto(1) + tcpLen(2)
	pLen := len(tcpSegment)
	pseudo := make([]byte, 12+pLen)
	copy(pseudo[0:4], srcIP)
	copy(pseudo[4:8], dstIP)
	pseudo[9] = 6 // TCP
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(pLen))
	copy(pseudo[12:], tcpSegment)
	// Zero out checksum field in the copy
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

func (sp *subnetProxy) cleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-sp.done:
			return
		case <-ticker.C:
			sp.dev.cleanupNATTable()
		}
	}
}

// buildICMPReplyPacket constructs a full IPv4+ICMP echo reply packet.
func buildICMPReplyPacket(srcIP, dstIP net.IP, id, seq uint16, data []byte) []byte {
	if len(srcIP) < 4 || len(dstIP) < 4 {
		return nil
	}
	icmpLen := 8 + len(data)
	totalLen := 20 + icmpLen
	pkt := make([]byte, totalLen)

	// IPv4 header
	pkt[0] = 0x45 // version=4, IHL=5
	binary.BigEndian.PutUint16(pkt[2:4], uint16(totalLen))
	pkt[8] = 64   // TTL
	pkt[9] = 1    // ICMP
	copy(pkt[12:16], srcIP)
	copy(pkt[16:20], dstIP)

	// IP checksum
	cksum := ipHeaderChecksum(pkt[:20])
	binary.BigEndian.PutUint16(pkt[10:12], cksum)

	// ICMP echo reply
	pkt[20] = 0 // type=0 (echo reply)
	pkt[21] = 0 // code=0
	binary.BigEndian.PutUint16(pkt[24:26], id)
	binary.BigEndian.PutUint16(pkt[26:28], seq)
	copy(pkt[28:], data)

	// ICMP checksum
	icmpCksum := icmpChecksum(pkt[20:])
	binary.BigEndian.PutUint16(pkt[22:24], icmpCksum)

	return pkt
}

// buildUDPReplyPacket constructs a full IPv4+UDP packet.
func buildUDPReplyPacket(srcIP, dstIP net.IP, srcPort, dstPort uint16, payload []byte) []byte {
	if len(srcIP) < 4 || len(dstIP) < 4 {
		return nil
	}
	udpLen := 8 + len(payload)
	totalLen := 20 + udpLen
	pkt := make([]byte, totalLen)

	// IPv4 header
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(totalLen))
	pkt[8] = 64  // TTL
	pkt[9] = 17  // UDP
	copy(pkt[12:16], srcIP)
	copy(pkt[16:20], dstIP)
	cksum := ipHeaderChecksum(pkt[:20])
	binary.BigEndian.PutUint16(pkt[10:12], cksum)

	// UDP header
	binary.BigEndian.PutUint16(pkt[20:22], srcPort)
	binary.BigEndian.PutUint16(pkt[22:24], dstPort)
	binary.BigEndian.PutUint16(pkt[24:26], uint16(udpLen))
	copy(pkt[28:], payload)

	// UDP checksum (optional for IPv4, set to 0)
	binary.BigEndian.PutUint16(pkt[26:28], 0)

	return pkt
}

func icmpChecksum(data []byte) uint16 {
	var sum uint32
	for i := 0; i < len(data)-1; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(data[i : i+2]))
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}
