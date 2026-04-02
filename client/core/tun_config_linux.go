//go:build linux

package core

import "os/exec"

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
