//go:build darwin

package core

import (
	"fmt"
	"net"
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

func protectServerRoute(serverHost string) {
	ips, err := net.LookupHost(serverHost)
	if err != nil || len(ips) == 0 {
		return
	}
	out, _ := exec.Command("route", "-n", "get", "default").CombinedOutput()
	gw := ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "gateway:") {
			gw = strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
			break
		}
	}
	if gw != "" {
		exec.Command("route", "add", "-host", ips[0], gw).Run()
	}
}

func removeServerRoute(serverHost string) {
	ips, err := net.LookupHost(serverHost)
	if err != nil || len(ips) == 0 {
		return
	}
	exec.Command("route", "delete", "-host", ips[0]).Run()
}

func removeRoute(ifName, subnet string) error {
	exec.Command("route", "delete", "-net", subnet).Run()
	return nil
}

func enableIPForwarding() {
	exec.Command("sysctl", "-w", "net.inet.ip.forwarding=1").Run()
}

func enableNAT(ifName string) {
	// macOS: pfctl NAT as backup alongside userspace SNAT
	out, err := exec.Command("route", "-n", "get", "default").CombinedOutput()
	if err != nil {
		return
	}
	phys := ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "interface:") {
			phys = strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
			break
		}
	}
	if phys == "" || phys == ifName {
		return
	}
	exec.Command("bash", "-c",
		fmt.Sprintf(`echo "nat on %s from 10.7.0.0/24 to any -> (%s)" | pfctl -ef - 2>/dev/null`, phys, phys)).Run()
}

func disableNAT(ifName string) {
	exec.Command("pfctl", "-d").Run()
}

// detectExitIP finds the local IP address that can reach the given subnet.
func detectExitIP(subnet string) net.IP {
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.To4() == nil {
				continue
			}
			if ipNet.Contains(ip) {
				return ip
			}
		}
	}
	return nil
}

func pickSNATIP(subnet string, exitIP net.IP) net.IP {
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil
	}
	base := ipNet.IP.To4()
	if base == nil {
		return nil
	}
	for i := 254; i >= 200; i-- {
		candidate := net.IPv4(base[0], base[1], base[2], byte(i))
		if exitIP != nil && candidate.Equal(exitIP) {
			continue
		}
		return candidate
	}
	return nil
}

func setupSNATRoute(ifName string, snatIP net.IP) {
	exec.Command("route", "add", "-host", snatIP.String(), "-interface", ifName).Run()
}

func cleanupSNATRoute(ifName string, snatIP net.IP) {
	if snatIP != nil {
		exec.Command("route", "delete", "-host", snatIP.String()).Run()
	}
}

func checkForwardingStatus() string {
	out, _ := exec.Command("sysctl", "net.inet.ip.forwarding").CombinedOutput()
	return strings.TrimSpace(string(out))
}
