//go:build linux

package routing

import (
	"fmt"
	"os/exec"
	"strings"
)

// EnableStickyECMP sets up connmark-based session stickiness for ECMP routes
// to balancerCIDR. New connections get their routing decision saved as a
// connmark; established connections restore the mark to reuse the same path.
func EnableStickyECMP(balancerCIDR string) error {
	if err := validateCIDR(balancerCIDR); err != nil {
		return err
	}

	// Restore connmark on packets belonging to existing connections.
	out, err := exec.Command(
		"iptables", "-t", "mangle", "-A", "PREROUTING",
		"-d", balancerCIDR,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
		"-j", "CONNMARK", "--restore-mark",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables connmark restore for %s: %w: %s", balancerCIDR, err, strings.TrimSpace(string(out)))
	}

	// Save connmark for new connections after routing decision.
	out, err = exec.Command(
		"iptables", "-t", "mangle", "-A", "POSTROUTING",
		"-d", balancerCIDR,
		"-m", "conntrack", "--ctstate", "NEW",
		"-j", "CONNMARK", "--save-mark",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables connmark save for %s: %w: %s", balancerCIDR, err, strings.TrimSpace(string(out)))
	}

	return nil
}

// DisableStickyECMP removes the connmark rules for balancerCIDR.
func DisableStickyECMP(balancerCIDR string) error {
	if err := validateCIDR(balancerCIDR); err != nil {
		return err
	}

	exec.Command(
		"iptables", "-t", "mangle", "-D", "PREROUTING",
		"-d", balancerCIDR,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
		"-j", "CONNMARK", "--restore-mark",
	).CombinedOutput()

	exec.Command(
		"iptables", "-t", "mangle", "-D", "POSTROUTING",
		"-d", balancerCIDR,
		"-m", "conntrack", "--ctstate", "NEW",
		"-j", "CONNMARK", "--save-mark",
	).CombinedOutput()

	return nil
}

// EnableL4Hash sets the kernel to use L4 (src/dst port) for ECMP hashing
// instead of just L3 (src/dst IP), providing better flow distribution.
func EnableL4Hash() error {
	out, err := exec.Command("sysctl", "-w", "net.ipv4.fib_multipath_hash_policy=1").CombinedOutput()
	if err != nil {
		return fmt.Errorf("sysctl fib_multipath_hash_policy: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
