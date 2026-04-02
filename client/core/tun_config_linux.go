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

func addRoute(ifName, subnet, gateway string) error {
	return exec.Command("ip", "route", "add", subnet, "via", gateway, "dev", ifName).Run()
}

func removeRoute(ifName, subnet string) error {
	return exec.Command("ip", "route", "del", subnet, "dev", ifName).Run()
}

func enableIPForwarding() {
	exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").Run()
}

func enableNAT(ifName string) {
	// Masquerade traffic from TUN to physical interface
	exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING", "!", "-o", ifName, "-j", "MASQUERADE").Run()
	exec.Command("iptables", "-A", "FORWARD", "-i", ifName, "-j", "ACCEPT").Run()
	exec.Command("iptables", "-A", "FORWARD", "-o", ifName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run()
}

func disableNAT(ifName string) {
	exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING", "!", "-o", ifName, "-j", "MASQUERADE").Run()
	exec.Command("iptables", "-D", "FORWARD", "-i", ifName, "-j", "ACCEPT").Run()
	exec.Command("iptables", "-D", "FORWARD", "-o", ifName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run()
}
