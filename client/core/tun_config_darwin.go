//go:build darwin

package core

import (
	"fmt"
	"os/exec"
	"strings"

	"golang.zx2c4.com/wireguard/tun"
)

type wgTunIface struct {
	dev  tun.Device
	name string
}

func (w *wgTunIface) Read(b []byte) (int, error) {
	// macOS utun needs 4 bytes offset for AF header
	bufs := [][]byte{make([]byte, len(b)+4)}
	sizes := make([]int, 1)
	n, err := w.dev.Read(bufs, sizes, 4)
	if err != nil {
		return 0, err
	}
	if n == 0 || sizes[0] == 0 {
		return 0, fmt.Errorf("no packets read")
	}
	copy(b, bufs[0][4:4+sizes[0]])
	return sizes[0], nil
}

func (w *wgTunIface) Write(b []byte) (int, error) {
	// macOS utun needs 4 bytes AF header prepended
	buf := make([]byte, 4+len(b))
	if len(b) > 0 && (b[0]>>4) == 6 {
		buf[3] = 30 // AF_INET6
	} else {
		buf[3] = 2 // AF_INET
	}
	copy(buf[4:], b)
	bufs := [][]byte{buf}
	n, err := w.dev.Write(bufs, 4)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("no packets written")
	}
	return len(b), nil
}

func (w *wgTunIface) Close() error {
	return w.dev.Close()
}

func (w *wgTunIface) Name() string {
	return w.name
}

func createPlatformTun() (tunIface, error) {
	dev, err := tun.CreateTUN("utun", 1500)
	if err != nil {
		return nil, fmt.Errorf("TUN creation failed: %w", err)
	}
	name, err := dev.Name()
	if err != nil {
		dev.Close()
		return nil, err
	}
	return &wgTunIface{dev: dev, name: name}, nil
}

func configureTunInterface(ifName, localIP, peerIP string) error {
	if err := exec.Command("ifconfig", ifName, localIP, peerIP, "up").Run(); err != nil {
		return err
	}
	exec.Command("route", "add", "-host", peerIP, "-interface", ifName).Run()
	return nil
}

func removeTunInterface(ifName string) error {
	return nil
}

func addRoute(ifName, subnet, gateway string) error {
	return exec.Command("route", "add", "-net", subnet, gateway).Run()
}

func removeRoute(ifName, subnet string) error {
	exec.Command("route", "delete", "-net", subnet).Run()
	return nil
}

func enableIPForwarding() {
	exec.Command("sysctl", "-w", "net.inet.ip.forwarding=1").Run()
}

func enableNAT(ifName string) {
	// Masquerade: packets from VPN subnet going out any physical interface
	// Try common interface names
	for _, phys := range []string{"en0", "en1", "en2", "en3"} {
		exec.Command("bash", "-c",
			fmt.Sprintf(`echo "nat on %s from 10.7.0.0/24 to any -> (%s)" | pfctl -ef - 2>/dev/null`, phys, phys)).Run()
	}
}

func disableNAT(ifName string) {
	exec.Command("pfctl", "-d").Run()
}

func checkForwardingStatus() string {
	out, _ := exec.Command("sysctl", "net.inet.ip.forwarding").CombinedOutput()
	return strings.TrimSpace(string(out))
}
