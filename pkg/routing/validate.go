package routing

import (
	"fmt"
	"net/netip"
	"regexp"
)

var ifaceNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9\-]{0,14}$`)

func validateIP(ip string) error {
	if _, err := netip.ParseAddr(ip); err != nil {
		return fmt.Errorf("invalid IP address %q: %w", ip, err)
	}
	return nil
}

func validateInterface(name string) error {
	if !ifaceNameRe.MatchString(name) {
		return fmt.Errorf("invalid interface name %q: must be alphanumeric with dashes, max 15 chars", name)
	}
	return nil
}

func validateCIDR(cidr string) error {
	if _, err := netip.ParsePrefix(cidr); err != nil {
		return fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	return nil
}
