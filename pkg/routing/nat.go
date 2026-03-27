//go:build linux

package routing

import (
	"fmt"
	"os/exec"
	"strings"
)

// EnableMasquerade adds an iptables MASQUERADE rule for outbound traffic on iface.
func EnableMasquerade(iface string) error {
	if err := validateInterface(iface); err != nil {
		return err
	}
	out, err := exec.Command(
		"iptables", "-t", "nat", "-A", "POSTROUTING", "-o", iface, "-j", "MASQUERADE",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables enable masquerade on %s: %w: %s", iface, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DisableMasquerade removes the iptables MASQUERADE rule for outbound traffic on iface.
func DisableMasquerade(iface string) error {
	if err := validateInterface(iface); err != nil {
		return err
	}
	out, err := exec.Command(
		"iptables", "-t", "nat", "-D", "POSTROUTING", "-o", iface, "-j", "MASQUERADE",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables disable masquerade on %s: %w: %s", iface, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// EnableForwarding enables IPv4 packet forwarding via sysctl.
func EnableForwarding() error {
	out, err := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").CombinedOutput()
	if err != nil {
		return fmt.Errorf("enable ip_forward: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
