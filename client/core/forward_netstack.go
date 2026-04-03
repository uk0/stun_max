package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// forwardNetstack is a per-peer gVisor TCP/IP stack for port forwarding.
// A side: DialTCP to create virtual connections that get sent to B via P2P UDP.
// B side: TCP forwarder accepts virtual connections and bridges to real targets.
//
// Virtual IP scheme: A=10.99.0.1, B=10.99.0.2 (per-peer, not conflicting with TUN VPN 10.7.0.x)
// PLACEHOLDER_FORWARD_NETSTACK

const (
	fwdNICID    = 2 // different from netstackProxy's NIC ID
	fwdLocalIP  = "10.99.0.1"
	fwdRemoteIP = "10.99.0.2"
)

type forwardNetstack struct {
	ep       *channel.Endpoint
	s        *stack.Stack
	tnet     *gonet.TCPConn // unused, just for type reference
	client   *Client
	peerID   string
	localIP  tcpip.Address
	remoteIP tcpip.Address
	done     chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
	// B side: maps virtual port → real target address
	portMap  sync.Map // uint16 → string ("host:port")
}

func newForwardNetstack(client *Client, peerID string, isInitiator bool) (*forwardNetstack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
		HandleLocal:        false,
	})

	ep := channel.New(1024, 1400, "")

	if tcpipErr := s.CreateNIC(fwdNICID, ep); tcpipErr != nil {
		return nil, fmt.Errorf("CreateNIC: %v", tcpipErr)
	}

	s.SetPromiscuousMode(fwdNICID, true)
	s.SetSpoofing(fwdNICID, true)
	s.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: fwdNICID})

	// TCP tuning
	sackOpt := tcpip.TCPSACKEnabled(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sackOpt)
	rcvBuf := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4096, Default: 262144, Max: 4194304}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcvBuf)
	sndBuf := tcpip.TCPSendBufferSizeRangeOption{Min: 4096, Default: 262144, Max: 4194304}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sndBuf)

	// Assign local IP
	var myIP, peerIP string
	if isInitiator {
		myIP, peerIP = fwdLocalIP, fwdRemoteIP
	} else {
		myIP, peerIP = fwdRemoteIP, fwdLocalIP
	}

	myAddr := tcpip.AddrFromSlice(net.ParseIP(myIP).To4())
	protoAddr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: myAddr.WithPrefix(),
	}
	if tcpipErr := s.AddProtocolAddress(fwdNICID, protoAddr, stack.AddressProperties{}); tcpipErr != nil {
		return nil, fmt.Errorf("AddProtocolAddress: %v", tcpipErr)
	}

	fn := &forwardNetstack{
		ep:       ep,
		s:        s,
		client:   client,
		peerID:   peerID,
		localIP:  myAddr,
		remoteIP: tcpip.AddrFromSlice(net.ParseIP(peerIP).To4()),
		done:     make(chan struct{}),
	}
	fn.ctx, fn.cancel = context.WithCancel(context.Background())

	// B side: set up TCP forwarder to accept connections from A
	if !isInitiator {
		tcpFwd := tcp.NewForwarder(s, 0, 2048, fn.handleIncoming)
		s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)
	}

	go fn.outboundLoop()

	fn.client.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
		"fwd-ns: created stack myIP=%s peerIP=%s initiator=%v", myIP, peerIP, isInitiator)})

	return fn, nil
}

// InjectInbound feeds a raw IP packet from the peer into this forward netstack.
func (fn *forwardNetstack) InjectInbound(pkt []byte) {
	if len(pkt) < 20 {
		return
	}
	proto := pkt[9]
	fn.client.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
		"fwd-ns IN: %d bytes proto=%d src=%d.%d.%d.%d dst=%d.%d.%d.%d",
		len(pkt), proto, pkt[12], pkt[13], pkt[14], pkt[15], pkt[16], pkt[17], pkt[18], pkt[19])})

	pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(append([]byte(nil), pkt...)),
	})
	fn.ep.InjectInbound(header.IPv4ProtocolNumber, pkb)
	pkb.DecRef()
}

// outboundLoop reads packets from gVisor and sends to peer via P2P UDP or relay.
func (fn *forwardNetstack) outboundLoop() {
	for {
		pkt := fn.ep.ReadContext(fn.ctx)
		if pkt == nil {
			return
		}

		view := pkt.ToView()
		buf := make([]byte, view.Size())
		view.Read(buf)
		view.Release()
		pkt.DecRef()

		if len(buf) >= 20 {
			proto := buf[9]
			fn.client.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
				"fwd-ns OUT: %d bytes proto=%d src=%d.%d.%d.%d dst=%d.%d.%d.%d",
				len(buf), proto, buf[12], buf[13], buf[14], buf[15], buf[16], buf[17], buf[18], buf[19])})
		}

		// Send via P2P UDP with "FN:" prefix, or relay
		compressed := Compress(buf)
		fn.sendPacket(compressed)
	}
}

func (fn *forwardNetstack) sendPacket(compressed []byte) {
	// Try P2P UDP with "FN:" prefix
	fn.client.peerConnsMu.RLock()
	pc := fn.client.peerConns[fn.peerID]
	fn.client.peerConnsMu.RUnlock()

	if pc != nil && pc.Mode == "direct" && pc.UDPAddr != nil {
		fn.client.connMu.Lock()
		udpConn := fn.client.udpConn
		fn.client.connMu.Unlock()
		if udpConn != nil {
			msg := make([]byte, 3+len(compressed))
			copy(msg[:3], []byte("FN:"))
			copy(msg[3:], compressed)
			if _, err := udpConn.WriteToUDP(msg, pc.UDPAddr); err == nil {
				return
			}
		}
	}

	// Relay fallback
	encoded := base64.StdEncoding.EncodeToString(compressed)
	fn.client.sendRelay(fn.peerID, "fwd_data", TunData{Data: encoded})
}

// DialTCP creates a virtual TCP connection through the gVisor stack to the peer.
// Used by A side to bridge local TCP → virtual TCP → peer.
func (fn *forwardNetstack) DialTCP(virtualPort uint16) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(fn.ctx, 10*time.Second)
	defer cancel()

	remoteAddr := tcpip.FullAddress{Addr: fn.remoteIP, Port: virtualPort}
	conn, err := gonet.DialContextTCP(ctx, fn.s, remoteAddr, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("DialTCP port %d: %v", virtualPort, err)
	}
	return conn, nil
}

// RegisterTarget registers a virtual port → real target mapping (B side).
func (fn *forwardNetstack) RegisterTarget(virtualPort uint16, target string) {
	fn.portMap.Store(virtualPort, target)
	fn.client.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
		"fwd-ns: registered port %d → %s", virtualPort, target)})
}

// handleIncoming is called by gVisor for each new TCP connection from A (B side).
func (fn *forwardNetstack) handleIncoming(r *tcp.ForwarderRequest) {
	id := r.ID()
	virtualPort := id.LocalPort

	fn.client.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
		"fwd-ns: incoming TCP from %s:%d to port %d", id.RemoteAddress, id.RemotePort, virtualPort)})

	val, ok := fn.portMap.Load(virtualPort)
	if !ok {
		fn.client.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf(
			"fwd-ns: no target for port %d, sending RST", virtualPort)})
		r.Complete(true)
		return
	}
	target := val.(string)

	var wq waiter.Queue
	ep, tcpipErr := r.CreateEndpoint(&wq)
	if tcpipErr != nil {
		r.Complete(true)
		return
	}
	r.Complete(false)

	ep.SocketOptions().SetKeepAlive(true)
	conn := gonet.NewTCPConn(&wq, ep)

	go fn.bridgeForward(conn, target, virtualPort)
}

func (fn *forwardNetstack) bridgeForward(src net.Conn, target string, port uint16) {
	defer src.Close()

	dst, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		fn.client.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf(
			"forward netstack: dial %s failed: %v", target, err)})
		return
	}
	defer dst.Close()

	optimizeTCP(dst)

	errc := make(chan error, 2)
	go func() { _, err := io.Copy(dst, src); errc <- err }()
	go func() { _, err := io.Copy(src, dst); errc <- err }()
	<-errc
}

func (fn *forwardNetstack) Close() {
	fn.once.Do(func() {
		close(fn.done)
		fn.cancel()
		fn.s.Close()
		fn.ep.Close()
	})
}

// getOrCreateFwdNetstack returns the forward netstack for a peer, creating one if needed.
func (c *Client) getOrCreateFwdNetstack(peerID string, isInitiator bool) (*forwardNetstack, error) {
	c.fwdNetstacksMu.RLock()
	fn, ok := c.fwdNetstacks[peerID]
	c.fwdNetstacksMu.RUnlock()
	if ok {
		return fn, nil
	}

	c.fwdNetstacksMu.Lock()
	defer c.fwdNetstacksMu.Unlock()

	// Double-check after acquiring write lock
	if fn, ok := c.fwdNetstacks[peerID]; ok {
		return fn, nil
	}

	fn, err := newForwardNetstack(c, peerID, isInitiator)
	if err != nil {
		return nil, err
	}
	c.fwdNetstacks[peerID] = fn
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
		"Forward netstack created for peer %s (initiator=%v)", shortID(peerID), isInitiator)})
	return fn, nil
}

// handleFwdNetstackPacket routes an incoming FN: UDP packet to the right forward netstack.
func (c *Client) handleFwdNetstackPacket(raw []byte, fromAddr *net.UDPAddr) {
	c.peerConnsMu.RLock()
	var peerID string
	for id, pc := range c.peerConns {
		if pc.UDPAddr != nil && pc.UDPAddr.String() == fromAddr.String() {
			peerID = id
			break
		}
	}
	c.peerConnsMu.RUnlock()

	if peerID == "" {
		c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf(
			"fwd-ns: FN packet from unknown addr %s", fromAddr)})
		return
	}

	c.fwdNetstacksMu.RLock()
	fn, ok := c.fwdNetstacks[peerID]
	c.fwdNetstacksMu.RUnlock()
	if !ok {
		c.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf(
			"fwd-ns: no netstack for peer %s", shortID(peerID))})
		return
	}
	fn.InjectInbound(raw)
}

// handleFwdData handles fwd_data relay messages (fallback when P2P UDP unavailable).
func (c *Client) handleFwdData(msg Message) {
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

	c.fwdNetstacksMu.RLock()
	fn, ok := c.fwdNetstacks[msg.From]
	c.fwdNetstacksMu.RUnlock()
	if ok {
		fn.InjectInbound(raw)
	}
}
