//go:build windows

package core

import (
	"fmt"
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
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; `+
			`$fwd = Get-NetIPInterface | Where-Object { $_.Forwarding -eq 'Enabled' } | Select-Object -ExpandProperty InterfaceAlias; `+
			`$nat = Get-NetNat -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Name; `+
			`"Forwarding on: $($fwd -join ', '); NAT: $($nat -join ', ')"`)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}

func enableIPForwarding() {
	// Method 1: Set-NetIPInterface (immediate, per-interface)
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Get-NetAdapter | ForEach-Object { Set-NetIPInterface -InterfaceIndex $_.ifIndex -Forwarding Enabled -ErrorAction SilentlyContinue }`)
	// Method 2: Registry (persistent, needs reboot for first time)
	runSilent("reg", "add", `HKLM\SYSTEM\CurrentControlSet\Services\Tcpip\Parameters`,
		"/v", "IPEnableRouter", "/t", "REG_DWORD", "/d", "1", "/f")
	// Method 3: Enable RRAS service (Routing and Remote Access)
	runSilent("sc", "config", "RemoteAccess", "start=", "auto")
	runSilent("net", "start", "RemoteAccess")
	// Method 4: Disable firewall on TUN interface to allow forwarding
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Set-NetFirewallProfile -All -Enabled False -ErrorAction SilentlyContinue`)
}

func enableNAT(ifName string) {
	// Method 1: New-NetNat (Win10/11)
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Remove-NetNat -Name StunMaxNAT -Confirm:$false -ErrorAction SilentlyContinue`)
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`New-NetNat -Name StunMaxNAT -InternalIPInterfaceAddressPrefix "10.7.0.0/24" -ErrorAction SilentlyContinue`)
	// Method 2: ICS (Internet Connection Sharing) via COM
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`$m = New-Object -ComObject HNetCfg.HNetShare; `+
			`$conns = $m.EnumEveryConnection; `+
			`foreach($c in $conns) { `+
			`  $props = $m.NetConnectionProps($c); `+
			`  $cfg = $m.INetSharingConfigurationForINetConnection($c); `+
			`  if($props.Name -eq '`+ifName+`') { `+
			`    $cfg.EnableSharing(1) `+  // 1 = private
			`  } elseif($props.Status -eq 2) { `+ // 2 = connected
			`    $cfg.EnableSharing(0) `+  // 0 = public (share internet)
			`  } `+
			`}`)
	// Method 3: netsh routing (Windows Server fallback)
	runSilent("netsh", "routing", "ip", "nat", "install")
	runSilent("netsh", "routing", "ip", "nat", "add", "interface", ifName, "full")
}

func disableNAT(ifName string) {
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Remove-NetNat -Name StunMaxNAT -Confirm:$false -ErrorAction SilentlyContinue`)
	runSilent("netsh", "routing", "ip", "nat", "delete", "interface", ifName)
	// Disable ICS
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`$m = New-Object -ComObject HNetCfg.HNetShare; `+
			`$conns = $m.EnumEveryConnection; `+
			`foreach($c in $conns) { `+
			`  $cfg = $m.INetSharingConfigurationForINetConnection($c); `+
			`  $cfg.DisableSharing() `+
			`}`)
	// Re-enable firewall
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Set-NetFirewallProfile -All -Enabled True -ErrorAction SilentlyContinue`)
	// Disable forwarding
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Get-NetAdapter | ForEach-Object { Set-NetIPInterface -InterfaceIndex $_.ifIndex -Forwarding Disabled -ErrorAction SilentlyContinue }`)
}

func runSilent(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Run()
}
