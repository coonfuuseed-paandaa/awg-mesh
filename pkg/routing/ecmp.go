//go:build linux

package routing

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// NextHop describes a single nexthop in an ECMP route.
type NextHop struct {
	Via    string
	Dev    string
	Weight int
}

// SetECMPRoute installs a multipath route to dest with the given nexthops.
// At least one nexthop must be provided.
// Builds: ip route replace dest nexthop via X dev Y weight W [nexthop ...]
func SetECMPRoute(dest string, nexthops []NextHop) error {
	if len(nexthops) == 0 {
		return fmt.Errorf("at least one nexthop is required for ECMP route to %s", dest)
	}
	if dest != "default" {
		if err := validateCIDR(dest); err != nil {
			return err
		}
	}
	for _, nh := range nexthops {
		if err := validateIP(nh.Via); err != nil {
			return fmt.Errorf("nexthop via: %w", err)
		}
		if err := validateInterface(nh.Dev); err != nil {
			return fmt.Errorf("nexthop dev: %w", err)
		}
	}

	args := []string{"route", "replace", dest}
	for _, nh := range nexthops {
		weight := nh.Weight
		if weight < 1 {
			weight = 1
		}
		args = append(args, "nexthop", "via", nh.Via, "dev", nh.Dev, "weight", strconv.Itoa(weight))
	}

	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip route replace %s (ecmp, %d nexthops): %w: %s",
			dest, len(nexthops), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveECMPRoute removes the ECMP route to dest.
func RemoveECMPRoute(dest string) error {
	return DeleteRoute(dest)
}
