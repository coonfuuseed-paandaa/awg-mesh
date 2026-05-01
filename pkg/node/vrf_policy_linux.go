//go:build linux

package node

import (
	"errors"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// installVRFOverlayPolicyRule adds an `ip rule` entry routing destinations in
// `overlaySpaceCIDR` through `vrfTable`. Idempotent — if a rule with same
// (Dst, Table) is present, it is left alone. Required for F-008 Variant D so
// apps in the main netns (not SO_BINDTODEVICE-bound to VRF) consult the
// VRF table for overlay traffic.
//
// Equivalent shell: ip rule add to <overlaySpaceCIDR> lookup <vrfTable>
func installVRFOverlayPolicyRule(overlaySpaceCIDR string, vrfTable int) error {
	_, dst, err := net.ParseCIDR(overlaySpaceCIDR)
	if err != nil {
		return fmt.Errorf("parse overlay CIDR %q: %w", overlaySpaceCIDR, err)
	}

	rules, err := netlink.RuleList(unix.AF_INET)
	if err != nil {
		return fmt.Errorf("list ip rules: %w", err)
	}
	for _, r := range rules {
		if r.Table == vrfTable && r.Dst != nil &&
			r.Dst.IP.Equal(dst.IP) && r.Dst.Mask.String() == dst.Mask.String() {
			// Already present — idempotent skip.
			return nil
		}
	}

	rule := netlink.NewRule()
	rule.Dst = dst
	rule.Table = vrfTable
	rule.Family = unix.AF_INET
	rule.Priority = 100
	if addErr := netlink.RuleAdd(rule); addErr != nil && !errors.Is(addErr, unix.EEXIST) {
		return fmt.Errorf("rule add to %s table %d: %w", overlaySpaceCIDR, vrfTable, addErr)
	}
	return nil
}
