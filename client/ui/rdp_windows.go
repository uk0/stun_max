//go:build windows

package ui

import (
	"fmt"
	"os/exec"
	"os/user"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

// EnableRDP enables Remote Desktop, restricts to localhost only, starts service.
func EnableRDP() error {
	// Enable RDP via registry
	runCmd("reg", "add",
		`HKLM\SYSTEM\CurrentControlSet\Control\Terminal Server`,
		"/v", "fDenyTSConnections", "/t", "REG_DWORD", "/d", "0", "/f")

	// Disable NLA (allow non-domain RDP)
	runCmd("reg", "add",
		`HKLM\SYSTEM\CurrentControlSet\Control\Terminal Server\WinStations\RDP-Tcp`,
		"/v", "UserAuthentication", "/t", "REG_DWORD", "/d", "0", "/f")

	// Firewall: block RDP from all external, only allow 127.0.0.1
	// First disable all default RDP rules
	runCmd("netsh", "advfirewall", "firewall", "set", "rule",
		"group=remote desktop", "new", "enable=No")
	runCmd("netsh", "advfirewall", "firewall", "set", "rule",
		`group=远程桌面`, "new", "enable=No")
	// Remove any previous StunMax rule
	runCmd("netsh", "advfirewall", "firewall", "delete", "rule",
		"name=StunMax-RDP")
	// Add localhost-only rule
	runCmd("netsh", "advfirewall", "firewall", "add", "rule",
		"name=StunMax-RDP", "dir=in", "action=allow",
		"protocol=TCP", "localport=3389",
		"remoteip=127.0.0.1", "enable=yes")

	// Start service
	runCmd("sc", "config", "TermService", "start=auto")
	runCmd("net", "start", "TermService")

	return nil
}

// DisableRDP stops Remote Desktop service and disables it.
func DisableRDP() error {
	runCmd("net", "stop", "TermService", "/y")

	runCmd("reg", "add",
		`HKLM\SYSTEM\CurrentControlSet\Control\Terminal Server`,
		"/v", "fDenyTSConnections", "/t", "REG_DWORD", "/d", "1", "/f")

	// Remove our localhost-only rule
	runCmd("netsh", "advfirewall", "firewall", "delete", "rule",
		"name=StunMax-RDP")
	// Restore default RDP rules to disabled state
	runCmd("netsh", "advfirewall", "firewall", "set", "rule",
		"group=remote desktop", "new", "enable=No")
	runCmd("netsh", "advfirewall", "firewall", "set", "rule",
		`group=远程桌面`, "new", "enable=No")

	return nil
}

// SetUserPassword sets the password for an existing local user via net user.
func SetUserPassword(username, password string) error {
	out, err := runCmd("net", "user", username, password)
	if err != nil {
		// Fallback: PowerShell
		ps := fmt.Sprintf(
			`[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; `+
				`$pw = ConvertTo-SecureString '%s' -AsPlainText -Force; `+
				`Set-LocalUser -Name '%s' -Password $pw`,
			escapePSString(password), username)
		out2, err2 := runCmd("powershell", "-NoProfile", "-NonInteractive",
			"-ExecutionPolicy", "Bypass", "-Command", ps)
		if err2 != nil {
			return fmt.Errorf("%s | %s", strings.TrimSpace(out), strings.TrimSpace(out2))
		}
	}
	return nil
}

// GetCurrentUsername returns the current Windows username.
func GetCurrentUsername() string {
	u, err := user.Current()
	if err != nil {
		return "Administrator"
	}
	// user.Current() returns DOMAIN\user, extract just the username
	name := u.Username
	if idx := strings.LastIndex(name, `\`); idx >= 0 {
		name = name[idx+1:]
	}
	return name
}

// IsRDPEnabled checks if RDP is currently enabled.
func IsRDPEnabled() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Terminal Server`,
		registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	val, _, err := k.GetIntegerValue("fDenyTSConnections")
	if err != nil {
		return false
	}
	return val == 0
}

// UserExists checks if a local user exists.
func UserExists(username string) bool {
	_, err := runCmd("net", "user", username)
	return err == nil
}

// RDPSupported returns true on Windows.
func RDPSupported() bool { return true }

// HasPassword checks if the current user has a password set.
// Uses "net user <name>" output — if "Password required  No" and last set is "Never", no password.
func HasPassword(username string) bool {
	// Try PowerShell first (most reliable)
	out, err := runCmd("powershell", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-Command",
		`[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; `+
			`$u = Get-LocalUser -Name '`+escapePSString(username)+`' 2>$null; `+
			`if ($u -and $u.PasswordLastSet) { Write-Output 'HAS_PASSWORD' } else { Write-Output 'NO_PASSWORD' }`)
	if err == nil {
		if strings.Contains(out, "HAS_PASSWORD") {
			return true
		}
		if strings.Contains(out, "NO_PASSWORD") {
			return false
		}
	}
	// Fallback: assume has password (safer default)
	return true
}

func escapePSString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
