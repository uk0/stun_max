//go:build darwin

package core

import "os/exec"

func configureTunInterface(ifName, localIP, peerIP string) error {
	if err := exec.Command("ifconfig", ifName, localIP, peerIP, "up").Run(); err != nil {
		return err
	}
	exec.Command("route", "add", "-host", peerIP, "-interface", ifName).Run()
	return nil
}

func removeTunInterface(ifName string) error {
	// macOS auto-removes utun on close
	return nil
}
