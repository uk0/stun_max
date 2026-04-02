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
	return nil
}

func addRoute(ifName, subnet, gateway string) error {
	return exec.Command("route", "add", "-net", subnet, gateway).Run()
}

func removeRoute(ifName, subnet string) error {
	exec.Command("route", "delete", "-net", subnet).Run()
	return nil
}

func enableIPForwarding() {
	exec.Command("sysctl", "-w", "net.inet.ip.forwarding=1").Run()
}

func enableNAT(ifName string) {
	exec.Command("bash", "-c",
		`echo "nat on en0 from `+ifName+`:network to any -> (en0)" | pfctl -ef -`).Run()
}

func disableNAT(ifName string) {
	exec.Command("pfctl", "-d").Run()
}
