//go:build linux

package core

import (
	"fmt"
	"os/exec"
	"strings"

	"golang.zx2c4.com/wireguard/tun"
)

type wgTunIface struct {
	dev  tun.Device
	name string
}

func (w *wgTunIface) Read(b []byte) (int, error) {
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
	dev, err := tun.CreateTUN("stunmax", 1500)
	if err != nil {
		return nil, fmt.Errorf("TUN creation failed: %w", err)
	}
	name, err := dev.Name()
	if err != nil {
		dev.Close()
		return nil, err
	}
	return &wgTunIface{dev: dev, name: name}, nil
}

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
	exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING", "!", "-o", ifName, "-j", "MASQUERADE").Run()
	exec.Command("iptables", "-A", "FORWARD", "-i", ifName, "-j", "ACCEPT").Run()
	exec.Command("iptables", "-A", "FORWARD", "-o", ifName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run()
}

func disableNAT(ifName string) {
	exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING", "!", "-o", ifName, "-j", "MASQUERADE").Run()
	exec.Command("iptables", "-D", "FORWARD", "-i", ifName, "-j", "ACCEPT").Run()
	exec.Command("iptables", "-D", "FORWARD", "-o", ifName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run()
}

func checkForwardingStatus() string {
	out, _ := exec.Command("sysctl", "net.ipv4.ip_forward").CombinedOutput()
	return strings.TrimSpace(string(out))
}
