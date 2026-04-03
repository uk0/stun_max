//go:build windows

package core

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"syscall"

	"golang.zx2c4.com/wireguard/tun"
)

// wgTunIface wraps wireguard tun.Device to implement tunIface.
type wgTunIface struct {
	dev  tun.Device
	name string
}

func (w *wgTunIface) Read(b []byte) (int, error) {
	// wireguard/tun uses batch reads; we read one packet at a time
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
	dev, err := tun.CreateTUN("StunMax", 1500)
	if err != nil {
		return nil, fmt.Errorf("Wintun device creation failed: %w", err)
	}
	name, err := dev.Name()
	if err != nil {
		dev.Close()
		return nil, err
	}
	return &wgTunIface{dev: dev, name: name}, nil
}

func configureTunInterface(ifName, localIP, peerIP string) error {
	if err := runSilentErr("netsh", "interface", "ip", "set", "address", ifName, "static", localIP, "255.255.255.0"); err != nil {
		return err
	}
	runSilent("netsh", "interface", "ip", "add", "route", peerIP+"/32", ifName)
	return nil
}

func removeTunInterface(ifName string) error {
	return nil
}

func addRoute(ifName, subnet, gateway string) error {
	return runSilentErr("netsh", "interface", "ip", "add", "route", subnet, ifName, gateway)
}

// protectServerRoute adds a specific host route for the server IP through the
// default gateway, ensuring WebSocket traffic is never routed through TUN.
func protectServerRoute(serverHost string) {
	// Resolve server hostname to IP
	ips, err := net.LookupHost(serverHost)
	if err != nil || len(ips) == 0 {
		return
	}
	serverIP := ips[0]

	// Find default gateway
	out, _ := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`(Get-NetRoute -DestinationPrefix '0.0.0.0/0' | Sort-Object RouteMetric | Select-Object -First 1).NextHop`).CombinedOutput()
	gw := strings.TrimSpace(string(out))
	if gw == "" || gw == "0.0.0.0" {
		return
	}

	// Add host route for server IP via default gateway
	runSilent("route", "add", serverIP, "mask", "255.255.255.255", gw, "metric", "1")
}

// removeServerRoute removes the protected server route.
func removeServerRoute(serverHost string) {
	ips, err := net.LookupHost(serverHost)
	if err != nil || len(ips) == 0 {
		return
	}
	runSilent("route", "delete", ips[0], "mask", "255.255.255.255")
}

func removeRoute(ifName, subnet string) error {
	runSilent("netsh", "interface", "ip", "delete", "route", subnet, ifName)
	return nil
}

func runSilentErr(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}

func checkForwardingStatus() string {
	// Check actual forwarding state + NAT state + firewall state
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; `+
			`$fwd = Get-NetIPInterface | Where-Object { $_.Forwarding -eq 'Enabled' } | Select-Object -ExpandProperty InterfaceAlias -ErrorAction SilentlyContinue; `+
			`$nat = Get-NetNat -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Name; `+
			`$fw = (Get-NetFirewallProfile | Where-Object {$_.Enabled -eq 'True'} | Measure-Object).Count; `+
			`$reg = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Services\Tcpip\Parameters' -Name IPEnableRouter -ErrorAction SilentlyContinue).IPEnableRouter; `+
			`"Fwd interfaces: [$($fwd -join ', ')]; NAT: [$($nat -join ', ')]; FW profiles active: $fw; Registry IPEnableRouter: $reg"`)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}

func enableIPForwarding() {
	// Only enable forwarding on the TUN interface and the physical interface
	// Do NOT blanket-enable on all interfaces — it disrupts WebSocket connections
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Get-NetAdapter | Where-Object { $_.Name -like 'StunMax*' -or $_.Name -like 'Ethernet*' -or $_.Name -like 'Wi-Fi*' } | ForEach-Object { Set-NetIPInterface -InterfaceIndex $_.ifIndex -Forwarding Enabled -ErrorAction SilentlyContinue }`)
	// Registry (persistent)
	runSilent("reg", "add", `HKLM\SYSTEM\CurrentControlSet\Services\Tcpip\Parameters`,
		"/v", "IPEnableRouter", "/t", "REG_DWORD", "/d", "1", "/f")
}

func enableNAT(ifName string) {
	// Userspace SNAT handles NAT now — no kernel NAT needed.
	// Just ensure forwarding and firewall allow forwarded packets.
	runSilent("netsh", "advfirewall", "firewall", "add", "rule",
		"name=StunMax-Forward", "dir=in", "action=allow", "enable=yes",
		"profile=any", "protocol=any")
	runSilent("netsh", "advfirewall", "firewall", "add", "rule",
		"name=StunMax-Forward-Out", "dir=out", "action=allow", "enable=yes",
		"profile=any", "protocol=any")
}

func disableNAT(ifName string) {
	runSilent("netsh", "advfirewall", "firewall", "delete", "rule", "name=StunMax-Forward")
	runSilent("netsh", "advfirewall", "firewall", "delete", "rule", "name=StunMax-Forward-Out")
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

// pickSNATIP picks a phantom IP in the target subnet for SNAT.
// Uses .254, .253, etc. avoiding the exitIP and common addresses.
func pickSNATIP(subnet string, exitIP net.IP) net.IP {
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil
	}
	base := ipNet.IP.To4()
	if base == nil {
		return nil
	}
	// Try .254, .253, .252 ...
	for i := 254; i >= 200; i-- {
		candidate := net.IPv4(base[0], base[1], base[2], byte(i))
		if exitIP != nil && candidate.Equal(exitIP) {
			continue
		}
		return candidate
	}
	return nil
}

// setupSNATRoute adds a host route for the SNAT IP pointing to the TUN interface.
// NOTE: We do NOT add the SNAT IP as a secondary address on TUN — that causes
// Windows to think TUN is on the physical subnet, disrupting WebSocket connections.
func setupSNATRoute(ifName string, snatIP net.IP) {
	s := snatIP.String()
	idx := getInterfaceIndex(ifName)

	// Add host route via TUN interface with low metric
	runSilent("route", "add", s, "mask", "255.255.255.255",
		"0.0.0.0", "metric", "1", "if", idx)
	runSilent("netsh", "interface", "ip", "add", "route",
		s+"/32", ifName, "0.0.0.0", "metric=1")
}

// getInterfaceIndex returns the Windows interface index for a given name.
func getInterfaceIndex(ifName string) string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "0"
	}
	for _, iface := range ifaces {
		if iface.Name == ifName {
			return fmt.Sprintf("%d", iface.Index)
		}
	}
	// Try partial match (Wintun names may differ)
	for _, iface := range ifaces {
		if strings.Contains(iface.Name, ifName) || strings.Contains(ifName, iface.Name) {
			return fmt.Sprintf("%d", iface.Index)
		}
	}
	return "0"
}

// cleanupSNATRoute removes the SNAT routes.
func cleanupSNATRoute(ifName string, snatIP net.IP) {
	if snatIP == nil {
		return
	}
	s := snatIP.String()
	runSilent("route", "delete", s)
	runSilent("netsh", "interface", "ip", "delete", "route", s+"/32", ifName)
}

func runSilent(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Run()
}
