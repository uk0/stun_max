package core

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// netstackProxy uses gVisor's userspace TCP/IP stack to proxy subnet traffic.
// Replaces the hand-rolled TCP state machine with production-grade TCP
// (congestion control, SACK, retransmission, window scaling — all handled by gVisor).
//
// Flow:
//   Peer packet → InjectInbound → gVisor TCP → handleTCPConn → io.Copy → real dest
//   Real dest responds → gVisor TCP → outboundLoop → P2P UDP → Peer
// PLACEHOLDER_REST_OF_FILE

const netstackNICID = 1

type netstackProxy struct {
	ep     *channel.Endpoint
	s      *stack.Stack
	dev    *TunDevice
	client *Client
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func newNetstackProxy(dev *TunDevice, client *Client) (*netstackProxy, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
		HandleLocal:        false,
	})

	ep := channel.New(1024, 1400, "")

	if tcpipErr := s.CreateNIC(netstackNICID, ep); tcpipErr != nil {
		return nil, fmt.Errorf("CreateNIC: %v", tcpipErr)
	}

	// Accept packets for ANY destination IP (we're a transparent proxy)
	s.SetPromiscuousMode(netstackNICID, true)
	s.SetSpoofing(netstackNICID, true)

	// Default route: all traffic goes through our NIC
	s.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: netstackNICID})

	// TCP tuning for streaming
	sackOpt := tcpip.TCPSACKEnabled(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sackOpt)
	rcvBuf := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4096, Default: 262144, Max: 4194304}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcvBuf)
	sndBuf := tcpip.TCPSendBufferSizeRangeOption{Min: 4096, Default: 262144, Max: 4194304}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sndBuf)

	np := &netstackProxy{
		ep:     ep,
		s:      s,
		dev:    dev,
		client: client,
		done:   make(chan struct{}),
	}
	np.ctx, np.cancel = context.WithCancel(context.Background())

	// TCP forwarder: intercepts all inbound TCP connections
	tcpFwd := tcp.NewForwarder(s, 0, 2048, np.handleTCPConn)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)

	// UDP forwarder: intercepts all inbound UDP
	udpFwd := udp.NewForwarder(s, np.handleUDPConn)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	// Outbound: read packets from gVisor and send to peer
	go np.outboundLoop()

	return np, nil
}

// InjectInbound feeds a raw IP packet from the peer into gVisor's TCP/IP stack.
func (np *netstackProxy) InjectInbound(pkt []byte) {
	if len(pkt) < 20 {
		return
	}
	pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(append([]byte(nil), pkt...)),
	})
	np.ep.InjectInbound(header.IPv4ProtocolNumber, pkb)
	pkb.DecRef()
}

// outboundLoop reads packets that gVisor generates (TCP ACKs, data segments, etc.)
// and sends them to the peer over P2P UDP or relay.
func (np *netstackProxy) outboundLoop() {
	for {
		select {
		case <-np.done:
			return
		default:
		}

		pkt := np.ep.ReadContext(np.ctx)
		if pkt == nil {
			return
		}

		view := pkt.ToView()
		buf := make([]byte, view.Size())
		view.Read(buf)
		view.Release()
		pkt.DecRef()

		atomic.AddInt64(&np.dev.bytesUp, int64(len(buf)))

		compressed := Compress(buf)
		if np.client.tunSendUDP(np.dev.peerID, compressed) {
			continue
		}
		encoded := base64.StdEncoding.EncodeToString(compressed)
		np.client.sendRelay(np.dev.peerID, "tun_data", TunData{Data: encoded})
	}
}

// handleTCPConn is called by gVisor for each new inbound TCP connection.
// gVisor has already completed the 3-way handshake. We get a net.Conn and bridge it.
func (np *netstackProxy) handleTCPConn(r *tcp.ForwarderRequest) {
	id := r.ID()
	var wq waiter.Queue

	ep, tcpipErr := r.CreateEndpoint(&wq)
	if tcpipErr != nil {
		r.Complete(true) // RST
		return
	}
	r.Complete(false)

	// Set keepalive
	ep.SocketOptions().SetKeepAlive(true)
	idle := tcpip.KeepaliveIdleOption(60 * time.Second)
	ep.SetSockOpt(&idle)
	interval := tcpip.KeepaliveIntervalOption(30 * time.Second)
	ep.SetSockOpt(&interval)

	conn := gonet.NewTCPConn(&wq, ep)

	dstIP := net.IP(id.LocalAddress.AsSlice())
	dstPort := id.LocalPort

	go np.bridgeTCP(conn, dstIP, dstPort)
}

// bridgeTCP connects to the real destination and bridges bidirectionally.
func (np *netstackProxy) bridgeTCP(src net.Conn, dstIP net.IP, dstPort uint16) {
	defer src.Close()

	dst, err := net.DialTimeout("tcp4",
		fmt.Sprintf("%s:%d", dstIP, dstPort), 10*time.Second)
	if err != nil {
		np.client.emit(EventLog, LogEvent{Level: "warn", Message: fmt.Sprintf(
			"netstack TCP: dial %s:%d failed: %v", dstIP, dstPort, err)})
		return
	}
	defer dst.Close()

	if cnt := atomic.AddInt32(&snatDebugCounter, 1); cnt <= 10 {
		np.client.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
			"netstack TCP: %s:%d connected", dstIP, dstPort)})
	}

	// Bidirectional copy — gVisor handles all TCP mechanics
	errc := make(chan error, 2)
	go func() { _, err := io.Copy(dst, src); errc <- err }()
	go func() { _, err := io.Copy(src, dst); errc <- err }()
	<-errc
}

// handleUDPConn is called by gVisor for each new inbound UDP flow.
func (np *netstackProxy) handleUDPConn(r *udp.ForwarderRequest) {
	id := r.ID()
	var wq waiter.Queue

	ep, tcpipErr := r.CreateEndpoint(&wq)
	if tcpipErr != nil {
		return
	}

	conn := gonet.NewUDPConn(&wq, ep)
	dstIP := net.IP(id.LocalAddress.AsSlice())
	dstPort := id.LocalPort

	go np.bridgeUDP(conn, dstIP, dstPort)
}

// bridgeUDP bridges a virtual UDP flow to a real UDP destination.
func (np *netstackProxy) bridgeUDP(src *gonet.UDPConn, dstIP net.IP, dstPort uint16) {
	defer src.Close()

	dst, err := net.Dial("udp4", fmt.Sprintf("%s:%d", dstIP, dstPort))
	if err != nil {
		return
	}
	defer dst.Close()

	errc := make(chan error, 2)
	go func() {
		buf := make([]byte, 65535)
		for {
			src.SetReadDeadline(time.Now().Add(120 * time.Second))
			n, err := src.Read(buf)
			if err != nil { errc <- err; return }
			dst.SetWriteDeadline(time.Now().Add(5 * time.Second))
			dst.Write(buf[:n])
		}
	}()
	go func() {
		buf := make([]byte, 65535)
		for {
			dst.SetReadDeadline(time.Now().Add(120 * time.Second))
			n, err := dst.Read(buf)
			if err != nil { errc <- err; return }
			src.SetWriteDeadline(time.Now().Add(5 * time.Second))
			src.Write(buf[:n])
		}
	}()
	<-errc
}

// handlePacket checks if a packet is for a routed subnet and injects it into netstack.
func (np *netstackProxy) handlePacket(pkt []byte) bool {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return false
	}
	dstIP := net.IP(pkt[16:20])
	for _, rn := range np.dev.routeNets {
		if rn.Contains(dstIP) {
			np.InjectInbound(pkt)
			return true
		}
	}
	return false
}

func (np *netstackProxy) Close() {
	np.once.Do(func() {
		close(np.done)
		np.cancel()
		np.s.Close()
		np.ep.Close()
	})
}
