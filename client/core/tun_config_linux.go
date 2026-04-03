//go:build linux

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
	bufs := [][]byte{b}
	sizes := make([]int, 1)
	n, err := w.dev.Read(bufs, sizes, 0)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("no packets read")
	}
	return sizes[0], nil
}

func (w *wgTunIface) Write(b []byte) (int, error) {
	bufs := [][]byte{b}
	n, err := w.dev.Write(bufs, 0)
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
	dev, err := tun.CreateTUN("stunmax", 1500)
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
	if err := exec.Command("ip", "addr", "add", localIP+"/24", "dev", ifName).Run(); err != nil {
		return err
	}
	if err := exec.Command("ip", "link", "set", "dev", ifName, "up").Run(); err != nil {
		return err
	}
	exec.Command("ip", "route", "add", peerIP+"/32", "dev", ifName).Run()
	return nil
}

func removeTunInterface(ifName string) error {
	exec.Command("ip", "link", "set", "dev", ifName, "down").Run()
	return nil
}

func addRoute(ifName, subnet, gateway string) error {
	return exec.Command("ip", "route", "add", subnet, "via", gateway, "dev", ifName).Run()
}

func protectServerRoute(serverHost string) {
	// Linux: add host route via default gateway
	ips, err := net.LookupHost(serverHost)
	if err != nil || len(ips) == 0 {
		return
	}
	out, _ := exec.Command("ip", "route", "show", "default").CombinedOutput()
	// Parse "default via X.X.X.X dev ethX"
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "default via ") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				exec.Command("ip", "route", "add", ips[0]+"/32", "via", parts[2]).Run()
			}
			break
		}
	}
}

func removeServerRoute(serverHost string) {
	ips, err := net.LookupHost(serverHost)
	if err != nil || len(ips) == 0 {
		return
	}
	exec.Command("ip", "route", "del", ips[0]+"/32").Run()
}

func removeRoute(ifName, subnet string) error {
	return exec.Command("ip", "route", "del", subnet, "dev", ifName).Run()
}

func enableIPForwarding() {
	exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").Run()
}

func enableNAT(ifName string) {
	// Userspace SNAT handles NAT now — iptables MASQUERADE as backup
	exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", "10.7.0.0/24", "!", "-o", ifName, "-j", "MASQUERADE").Run()
	exec.Command("iptables", "-A", "FORWARD", "-i", ifName, "-j", "ACCEPT").Run()
	exec.Command("iptables", "-A", "FORWARD", "-o", ifName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run()
}

func disableNAT(ifName string) {
	exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", "10.7.0.0/24", "!", "-o", ifName, "-j", "MASQUERADE").Run()
	exec.Command("iptables", "-D", "FORWARD", "-i", ifName, "-j", "ACCEPT").Run()
	exec.Command("iptables", "-D", "FORWARD", "-o", ifName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run()
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
	exec.Command("ip", "route", "add", snatIP.String()+"/32", "dev", ifName).Run()
}

func cleanupSNATRoute(ifName string, snatIP net.IP) {
	if snatIP != nil {
		exec.Command("ip", "route", "del", snatIP.String()+"/32", "dev", ifName).Run()
	}
}

func checkForwardingStatus() string {
	out, _ := exec.Command("sysctl", "net.ipv4.ip_forward").CombinedOutput()
	return strings.TrimSpace(string(out))
}
