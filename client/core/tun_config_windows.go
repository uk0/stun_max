//go:build windows

package core

import (
	"os/exec"
	"syscall"
)

func configureTunInterface(ifName, localIP, peerIP string) error {
	cmd := exec.Command("netsh", "interface", "ip", "set", "address", ifName, "static", localIP, "255.255.255.0")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		return err
	}
	cmd2 := exec.Command("netsh", "interface", "ip", "add", "route", peerIP+"/32", ifName)
	cmd2.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd2.Run()
	return nil
}

func removeTunInterface(ifName string) error {
	return nil
}

func addRoute(ifName, subnet, gateway string) error {
	cmd := exec.Command("netsh", "interface", "ip", "add", "route", subnet, ifName, gateway)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}

func removeRoute(ifName, subnet string) error {
	cmd := exec.Command("netsh", "interface", "ip", "delete", "route", subnet, ifName)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Run()
	return nil
}

func enableIPForwarding() {
	cmd := exec.Command("reg", "add", `HKLM\SYSTEM\CurrentControlSet\Services\Tcpip\Parameters`,
		"/v", "IPEnableRouter", "/t", "REG_DWORD", "/d", "1", "/f")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Run()
}

func enableNAT(ifName string) {
	// Windows ICS (Internet Connection Sharing) or netsh routing
	cmd := exec.Command("netsh", "routing", "ip", "nat", "install")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Run()
	cmd2 := exec.Command("netsh", "routing", "ip", "nat", "add", "interface", ifName, "full")
	cmd2.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd2.Run()
}

func disableNAT(ifName string) {
	cmd := exec.Command("netsh", "routing", "ip", "nat", "delete", "interface", ifName)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Run()
}
