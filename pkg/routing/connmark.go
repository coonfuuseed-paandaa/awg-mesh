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
//
// The function is idempotent: each rule is only added if it does not already
// exist (checked via iptables -C), so it is safe to call on every rebuildECMP.
func EnableStickyECMP(balancerCIDR string) error {
	if err := validateCIDR(balancerCIDR); err != nil {
		return err
	}

	// Restore connmark on packets belonging to existing connections.
	restoreArgs := []string{
		"-t", "mangle", "-C", "PREROUTING",
		"-d", balancerCIDR,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
		"-j", "CONNMARK", "--restore-mark",
	}
	if exec.Command("iptables", restoreArgs...).Run() != nil {
		restoreArgs[2] = "-A"
		out, err := exec.Command("iptables", restoreArgs...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("iptables connmark restore for %s: %w: %s", balancerCIDR, err, strings.TrimSpace(string(out)))
		}
	}

	// Save connmark for new connections after routing decision.
	saveArgs := []string{
		"-t", "mangle", "-C", "POSTROUTING",
		"-d", balancerCIDR,
		"-m", "conntrack", "--ctstate", "NEW",
		"-j", "CONNMARK", "--save-mark",
	}
	if exec.Command("iptables", saveArgs...).Run() != nil {
		saveArgs[2] = "-A"
		out, err := exec.Command("iptables", saveArgs...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("iptables connmark save for %s: %w: %s", balancerCIDR, err, strings.TrimSpace(string(out)))
		}
	}

	return nil
}

// DisableStickyECMP removes the connmark rules for balancerCIDR.
// Errors from both deletions are collected and returned to the caller.
func DisableStickyECMP(balancerCIDR string) error {
	if err := validateCIDR(balancerCIDR); err != nil {
		return err
	}

	var errs []string

	out, err := exec.Command(
		"iptables", "-t", "mangle", "-D", "PREROUTING",
		"-d", balancerCIDR,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
		"-j", "CONNMARK", "--restore-mark",
	).CombinedOutput()
	if err != nil {
		errs = append(errs, fmt.Sprintf("iptables connmark restore delete for %s: %s: %s", balancerCIDR, err, strings.TrimSpace(string(out))))
	}

	out, err = exec.Command(
		"iptables", "-t", "mangle", "-D", "POSTROUTING",
		"-d", balancerCIDR,
		"-m", "conntrack", "--ctstate", "NEW",
		"-j", "CONNMARK", "--save-mark",
	).CombinedOutput()
	if err != nil {
		errs = append(errs, fmt.Sprintf("iptables connmark save delete for %s: %s: %s", balancerCIDR, err, strings.TrimSpace(string(out))))
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
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
