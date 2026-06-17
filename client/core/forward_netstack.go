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
	portMap     sync.Map // uint16 → string ("host:port")
	filePortMap sync.Map // uint16 → *filePortEntry (file transfer handlers)
}

type filePortEntry struct {
	ft     *activeFileTransfer
	client *Client
}

func newForwardNetstack(client *Client, peerID string, isInitiator bool) (*forwardNetstack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
		HandleLocal:        false,
	})

	ep := channel.New(2048, 1472, "")

	if tcpipErr := s.CreateNIC(fwdNICID, ep); tcpipErr != nil {
		return nil, fmt.Errorf("CreateNIC: %v", tcpipErr)
	}

	s.SetPromiscuousMode(fwdNICID, true)
	s.SetSpoofing(fwdNICID, true)
	s.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: fwdNICID})

	// TCP tuning — large buffers for high throughput
	sackOpt := tcpip.TCPSACKEnabled(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sackOpt)
	rcvBuf := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4096, Default: 1048576, Max: 16777216}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcvBuf)
	sndBuf := tcpip.TCPSendBufferSizeRangeOption{Min: 4096, Default: 1048576, Max: 16777216}
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

		// Skip deflate — forwarded data is mostly TLS/encrypted (incompressible).
		// Use raw framing (0x00 header) to stay compatible with Decompress on receive side.
		raw := make([]byte, 1+len(buf))
		raw[0] = 0x00
		copy(raw[1:], buf)
		fn.sendPacket(raw)
	}
}

func (fn *forwardNetstack) sendPacket(data []byte) {
	// Check if any forward to this peer is forced to relay
	forceRelay := fn.client.isPeerForwardForceRelay(fn.peerID)

	if !forceRelay {
		if addr, ok := fn.client.directEndpoint(fn.peerID); ok {
			msg := make([]byte, 3+len(data))
			copy(msg[:3], []byte("FN:"))
			copy(msg[3:], data)
			if fn.client.udpSend(msg, addr) == nil {
				return
			}
		}
	}

	// Relay fallback
	encoded := base64.StdEncoding.EncodeToString(data)
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

// RegisterFileTransfer registers a virtual port for receiving a file stream.
func (fn *forwardNetstack) RegisterFileTransfer(port uint16, ft *activeFileTransfer, client *Client) {
	fn.filePortMap.Store(port, &filePortEntry{ft: ft, client: client})
}

// handleIncoming is called by gVisor for each new TCP connection from A (B side).
func (fn *forwardNetstack) handleIncoming(r *tcp.ForwarderRequest) {
	id := r.ID()
	virtualPort := id.LocalPort

	// Check if this is a file transfer port
	if fval, fok := fn.filePortMap.LoadAndDelete(virtualPort); fok {
		entry := fval.(*filePortEntry)
		var wq waiter.Queue
		ep, tcpipErr := r.CreateEndpoint(&wq)
		if tcpipErr != nil {
			r.Complete(true)
			return
		}
		r.Complete(false)
		conn := gonet.NewTCPConn(&wq, ep)
		go fn.receiveFileStream(conn, entry.ft, entry.client)
		return
	}

	// Regular forward port
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

// receiveFileStream reads file data from a gVisor TCP connection and writes to disk.
func (fn *forwardNetstack) receiveFileStream(conn net.Conn, ft *activeFileTransfer, client *Client) {
	defer conn.Close()

	ft.mu.Lock()
	f := ft.File
	fileSize := ft.FileSize
	startTime := ft.StartTime
	ft.mu.Unlock()

	if f == nil {
		return
	}

	client.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
		"File stream receiving: %s (%s)", ft.FileName, fmtFileSize(fileSize))})

	buf := make([]byte, 64*1024)
	for {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		n, err := conn.Read(buf)
		if n > 0 {
			ft.mu.Lock()
			if ft.File != nil {
				ft.File.Write(buf[:n])
			}
			ft.BytesDone += int64(n)
			bytesDone := ft.BytesDone
			ft.mu.Unlock()

			if bytesDone%(64*1024) < int64(n) {
				progress := float64(0)
				if fileSize > 0 {
					progress = float64(bytesDone) / float64(fileSize)
				}
				speed := float64(0)
				elapsed := time.Since(startTime).Seconds()
				if elapsed > 0 {
					speed = float64(bytesDone) / elapsed
				}
				client.emit(EventFileProgress, FileProgressEvent{
					TransferID: ft.TransferID,
					Progress:   progress,
					Speed:      speed,
					BytesDone:  bytesDone,
				})
			}
		}
		if err != nil {
			break
		}
	}

	// TCP stream ended — close file and verify hash
	ft.mu.Lock()
	if ft.File != nil {
		ft.File.Close()
		ft.File = nil
	}
	bytesDone := ft.BytesDone
	filePath := ft.FilePath
	fileName := ft.FileName
	expectedHash := ft.FileHash
	ft.mu.Unlock()

	// Verify SHA-256
	verified := false
	if expectedHash != "" && filePath != "" {
		if actualHash, err := hashFile(filePath); err == nil {
			if actualHash == expectedHash {
				verified = true
				client.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
					"File hash verified: %s (SHA-256: %s)", fileName, actualHash[:16])})
			} else {
				client.emit(EventLog, LogEvent{Level: "error", Message: fmt.Sprintf(
					"File hash MISMATCH: %s", fileName)})
				ft.mu.Lock()
				ft.Status = "error"
				ft.mu.Unlock()
				client.emit(EventFileError, FileErrorEvent{TransferID: ft.TransferID, Error: "hash mismatch"})
				return
			}
		}
	}

	ft.mu.Lock()
	ft.Status = "complete"
	ft.mu.Unlock()

	verifyStr := ""
	if verified {
		verifyStr = " (verified)"
	}
	client.emit(EventFileComplete, FileCompleteEvent{
		TransferID: ft.TransferID, FileName: fileName, Direction: "receive",
	})
	client.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf(
		"File received: %s (%s)%s", fileName, fmtFileSize(bytesDone), verifyStr)})
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
