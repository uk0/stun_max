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
	// Method 1: Per-interface forwarding (immediate)
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Get-NetAdapter | ForEach-Object { Set-NetIPInterface -InterfaceIndex $_.ifIndex -Forwarding Enabled -ErrorAction SilentlyContinue }`)
	// Method 2: Registry (persistent)
	runSilent("reg", "add", `HKLM\SYSTEM\CurrentControlSet\Services\Tcpip\Parameters`,
		"/v", "IPEnableRouter", "/t", "REG_DWORD", "/d", "1", "/f")
	// Method 3: RRAS service
	runSilent("sc", "config", "RemoteAccess", "start=", "auto")
	runSilent("net", "start", "RemoteAccess")
	// Method 4: Allow forwarding through Windows Firewall
	runSilent("netsh", "advfirewall", "firewall", "add", "rule",
		"name=StunMax-Forward", "dir=in", "action=allow", "enable=yes",
		"profile=any", "protocol=any")
	runSilent("netsh", "advfirewall", "firewall", "add", "rule",
		"name=StunMax-Forward-Out", "dir=out", "action=allow", "enable=yes",
		"profile=any", "protocol=any")
	// Disable firewall on TUN profile to allow forwarded packets
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Set-NetFirewallProfile -All -Enabled False -ErrorAction SilentlyContinue`)
}

func enableNAT(ifName string) {
	// Method 1: New-NetNat (requires Hyper-V on some Win10 versions)
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Remove-NetNat -Name StunMaxNAT -Confirm:$false -ErrorAction SilentlyContinue`)
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`New-NetNat -Name StunMaxNAT -InternalIPInterfaceAddressPrefix "10.7.0.0/24" -ErrorAction SilentlyContinue`)

	// Method 2: ICS (Internet Connection Sharing) — most reliable on Win10
	// Share the physical adapter's internet with the TUN interface
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
		`$m = New-Object -ComObject HNetCfg.HNetShare; `+
			`$conns = $m.EnumEveryConnection; `+
			`foreach($c in $conns) { `+
			`  try { `+
			`    $props = $m.NetConnectionProps($c); `+
			`    $cfg = $m.INetSharingConfigurationForINetConnection($c); `+
			`    if($props.Name -eq '`+ifName+`') { `+
			`      $cfg.EnableSharing(1) `+ // 1 = private (receives shared internet)
			`    } elseif($props.Status -eq 2 -and $props.Name -ne '`+ifName+`') { `+
			`      $cfg.EnableSharing(0) `+ // 0 = public (shares its internet)
			`    } `+
			`  } catch {} `+
			`}`)

	// Method 3: Add explicit route for return traffic
	// Ensure 10.7.0.0/24 replies go back through TUN
	runSilent("netsh", "interface", "ip", "add", "route", "10.7.0.0/24", ifName)
}

func disableNAT(ifName string) {
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Remove-NetNat -Name StunMaxNAT -Confirm:$false -ErrorAction SilentlyContinue`)
	// Disable ICS
	runSilent("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
		`$m = New-Object -ComObject HNetCfg.HNetShare; `+
			`$conns = $m.EnumEveryConnection; `+
			`foreach($c in $conns) { `+
			`  try { `+
			`    $cfg = $m.INetSharingConfigurationForINetConnection($c); `+
			`    $cfg.DisableSharing() `+
			`  } catch {} `+
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
