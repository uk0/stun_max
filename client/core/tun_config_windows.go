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
	return nil // Windows removes on close
}
