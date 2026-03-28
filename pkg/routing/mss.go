//go:build linux

package routing

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ClampMSS adds an iptables rule to clamp TCP MSS on SYN packets forwarded out iface.
func ClampMSS(iface string, mss int) error {
	if err := validateInterface(iface); err != nil {
		return err
	}
	out, err := exec.Command(
		"iptables", "-A", "FORWARD",
		"-o", iface,
		"-p", "tcp",
		"--tcp-flags", "SYN,RST", "SYN",
		"-j", "TCPMSS",
		"--set-mss", strconv.Itoa(mss),
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables clamp mss %d on %s: %w: %s", mss, iface, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ClampMSSToPMTU adds a global iptables rule to clamp TCP MSS to path MTU on
// forwarded SYN packets. This prevents fragmentation issues when traffic
// traverses WireGuard tunnels with reduced MTU.
func ClampMSSToPMTU() error {
	out, err := exec.Command(
		"iptables", "-A", "FORWARD",
		"-p", "tcp",
		"--tcp-flags", "SYN,RST", "SYN",
		"-j", "TCPMSS",
		"--clamp-mss-to-pmtu",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables clamp mss to pmtu: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveMSSClamp removes the iptables MSS clamping rule for iface.
func RemoveMSSClamp(iface string, mss int) error {
	if err := validateInterface(iface); err != nil {
		return err
	}
	out, err := exec.Command(
		"iptables", "-D", "FORWARD",
		"-o", iface,
		"-p", "tcp",
		"--tcp-flags", "SYN,RST", "SYN",
		"-j", "TCPMSS",
		"--set-mss", strconv.Itoa(mss),
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables remove mss clamp %d on %s: %w: %s", mss, iface, err, strings.TrimSpace(string(out)))
	}
	return nil
}
