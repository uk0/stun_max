package core

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/songgao/water"
)

// TunDevice holds the state of an active VPN tunnel.
type TunDevice struct {
	iface     *water.Interface
	ifName    string
	virtualIP net.IP
	peerIP    net.IP
	subnet    *net.IPNet
	peerID    string
	peerName  string
	bytesUp   int64
	bytesDown int64
	lastUp    int64
	lastDown  int64
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
}

// Virtual IP allocator: simple counter within 10.7.0.0/24
var tunIPCounter uint32 = 0

func nextTunIP() (string, string) {
	a := atomic.AddUint32(&tunIPCounter, 1)
	b := atomic.AddUint32(&tunIPCounter, 1)
	return fmt.Sprintf("10.7.0.%d", a), fmt.Sprintf("10.7.0.%d", b)
}

// StartTun initiates a TUN VPN with the given peer.
func (c *Client) StartTun(peerID string) error {
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

	myIP, peerIP := nextTunIP()
	subnet := "10.7.0.0/24"

	// Send setup to peer
	err = c.sendRelay(fullID, "tun_setup", TunSetup{
		VirtualIP: peerIP, // peer gets peerIP
		PeerIP:    myIP,   // peer's remote is our IP
		Subnet:    subnet,
	})
	if err != nil {
		return fmt.Errorf("send tun_setup: %w", err)
	}

	// Create local TUN device
	dev, err := c.createTunDevice(myIP, peerIP, subnet, fullID)
	if err != nil {
		return err
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

	// Notify peer
	c.sendRelay(dev.peerID, "tun_teardown", TunTeardown{})

	dev.closeOnce.Do(func() {
		close(dev.done)
	})

	if dev.iface != nil {
		dev.iface.Close()
	}
	removeTunInterface(dev.ifName)

	c.emit(EventTunStopped, LogEvent{Level: "info", Message: "VPN stopped"})
	return nil
}

func (c *Client) createTunDevice(localIP, peerIP, subnet, peerID string) (*TunDevice, error) {
	cfg := water.Config{
		DeviceType: water.TUN,
	}

	iface, err := water.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("TUN device creation failed (need root/admin): %w", err)
	}

	ifName := iface.Name()
	if err := configureTunInterface(ifName, localIP, peerIP); err != nil {
		iface.Close()
		return nil, fmt.Errorf("TUN interface config failed: %w", err)
	}

	_, ipNet, _ := net.ParseCIDR(subnet)

	dev := &TunDevice{
		iface:     iface,
		ifName:    ifName,
		virtualIP: net.ParseIP(localIP),
		peerIP:    net.ParseIP(peerIP),
		subnet:    ipNet,
		peerID:    peerID,
		done:      make(chan struct{}),
	}
	return dev, nil
}

// handleTunSetup processes an incoming tun_setup from a peer.
func (c *Client) handleTunSetup(msg Message) {
	var setup TunSetup
	if err := json.Unmarshal(msg.Payload, &setup); err != nil {
		c.emit(EventTunError, LogEvent{Level: "error", Message: "Invalid tun_setup: " + err.Error()})
		return
	}

	c.tunMu.Lock()
	if c.tunDevice != nil {
		c.tunMu.Unlock()
		c.emit(EventLog, LogEvent{Level: "warn", Message: "TUN setup received but VPN already active"})
		return
	}
	c.tunMu.Unlock()

	// setup.VirtualIP is our IP, setup.PeerIP is the remote peer's IP
	dev, err := c.createTunDevice(setup.VirtualIP, setup.PeerIP, setup.Subnet, msg.From)
	if err != nil {
		c.emit(EventTunError, LogEvent{Level: "error", Message: "TUN setup failed: " + err.Error()})
		return
	}

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

	c.emit(EventTunStarted, LogEvent{Level: "info", Message: fmt.Sprintf("VPN accepted: %s <-> %s (peer: %s)", setup.VirtualIP, setup.PeerIP, peerName)})
	c.emit(EventLog, LogEvent{Level: "info", Message: fmt.Sprintf("TUN VPN active (incoming): local=%s peer=%s", setup.VirtualIP, setup.PeerIP)})
}

// handleTunData processes incoming TUN data from a peer.
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

	atomic.AddInt64(&dev.bytesDown, int64(len(raw)))

	dev.mu.Lock()
	defer dev.mu.Unlock()
	if dev.iface != nil {
		dev.iface.Write(raw)
	}
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

	dev.closeOnce.Do(func() {
		close(dev.done)
	})
	if dev.iface != nil {
		dev.iface.Close()
	}
	removeTunInterface(dev.ifName)

	c.emit(EventTunStopped, LogEvent{Level: "info", Message: "VPN stopped by peer"})
}

// tunReadLoop reads IP packets from the TUN device and sends them to the peer.
func (c *Client) tunReadLoop(dev *TunDevice) {
	defer c.wg.Done()

	buf := make([]byte, 65536)
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
			case <-c.done:
			default:
				c.emit(EventTunError, LogEvent{Level: "error", Message: "TUN read: " + err.Error()})
			}
			return
		}

		if n == 0 {
			continue
		}

		packet := make([]byte, n)
		copy(packet, buf[:n])

		atomic.AddInt64(&dev.bytesUp, int64(n))

		compressed := Compress(packet)
		encoded := base64.StdEncoding.EncodeToString(compressed)

		c.sendRelay(dev.peerID, "tun_data", TunData{Data: encoded})
	}
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

	return TunInfo{
		Enabled:   true,
		VirtualIP: dev.virtualIP.String(),
		PeerIP:    dev.peerIP.String(),
		Subnet:    subnetStr,
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

	dev.closeOnce.Do(func() {
		close(dev.done)
	})
	if dev.iface != nil {
		dev.iface.Close()
	}
	removeTunInterface(dev.ifName)
}

// rateTicker should be called periodically (~1s) to compute TUN throughput.
// Already handled by TunStatus() snapshot approach (same as Forward).
