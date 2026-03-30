//go:build linux

package node

import (
	"fmt"
	"net"
	"strings"

	"github.com/thebtf/awg-mesh/pkg/routing"
	"github.com/vishvananda/netlink"
)

// AssignOverlayIP assigns the overlay IP to loopback with a /32 mask.
func AssignOverlayIP(ip string) error {
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil {
		return fmt.Errorf("overlay IP must be a valid IP address")
	}

	addr := &net.IPNet{IP: parsedIP, Mask: net.CIDRMask(32, 32)}
	router := routing.NewNetlinkRouter()

	exists, err := router.AddrExists("lo", addr)
	if err == nil && exists {
		return nil
	}

	return router.AddrAdd("lo", addr)
}

// RemoveOverlayIP removes the overlay IP from loopback.
func RemoveOverlayIP(ip string) error {
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil {
		return fmt.Errorf("overlay IP must be a valid IP address")
	}

	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("get loopback: %w", err)
	}

	addr := &netlink.Addr{
		IPNet: &net.IPNet{IP: parsedIP, Mask: net.CIDRMask(32, 32)},
	}
	if err := netlink.AddrDel(lo, addr); err != nil {
		return fmt.Errorf("addr del %s/32 dev lo: %w", parsedIP, err)
	}
	return nil
}
