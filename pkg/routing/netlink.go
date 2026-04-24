//go:build linux

package routing

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"
)

// NetlinkRouter implements Router using Linux netlink sockets.
// Zero fork overhead — communicates directly with the kernel.
type NetlinkRouter struct{}

// NewNetlinkRouter creates a new netlink-based router.
func NewNetlinkRouter() *NetlinkRouter {
	return &NetlinkRouter{}
}

// RouteAdd adds a route: dest via gateway through device.
func (r *NetlinkRouter) RouteAdd(dest *net.IPNet, via net.IP, dev string) error {
	link, err := netlink.LinkByName(dev)
	if err != nil {
		return fmt.Errorf("link %q: %w", dev, err)
	}

	route := &netlink.Route{
		Dst:       dest,
		Gw:        via,
		LinkIndex: link.Attrs().Index,
	}
	if err := netlink.RouteAdd(route); err != nil {
		return fmt.Errorf("route add %s via %s dev %s: %w", dest, via, dev, err)
	}
	return nil
}

// RouteReplace adds or replaces a route.
func (r *NetlinkRouter) RouteReplace(dest *net.IPNet, via net.IP, dev string) error {
	link, err := netlink.LinkByName(dev)
	if err != nil {
		return fmt.Errorf("link %q: %w", dev, err)
	}

	route := &netlink.Route{
		Dst:       dest,
		Gw:        via,
		LinkIndex: link.Attrs().Index,
	}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("route replace %s via %s dev %s: %w", dest, via, dev, err)
	}
	return nil
}

// RouteReplaceLink adds or replaces a scope=link route (no gateway) to dest via dev.
// Use when destination selection should be delegated to the interface driver.
func (r *NetlinkRouter) RouteReplaceLink(dest *net.IPNet, dev string) error {
	return r.RouteReplaceLinkWithSrc(dest, dev, nil)
}

// RouteReplaceLinkWithSrc is like RouteReplaceLink but also hints the preferred
// source IP for the kernel route. Pass nil src to fall back to default behaviour.
// Required when the preferred src is an overlay IP bound to lo (not the dev's
// primary IP), so kernel picks the overlay IP for outgoing packets instead of
// the transport IP on dev.
func (r *NetlinkRouter) RouteReplaceLinkWithSrc(dest *net.IPNet, dev string, src net.IP) error {
	link, err := netlink.LinkByName(dev)
	if err != nil {
		return fmt.Errorf("link %q: %w", dev, err)
	}

	route := &netlink.Route{
		Dst:       dest,
		LinkIndex: link.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
	}
	if src != nil {
		route.Src = src
	}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("route replace %s dev %s scope link: %w", dest, dev, err)
	}
	return nil
}

func isMissingRouteDeleteError(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}

// RouteDelete removes a route to dest.
func (r *NetlinkRouter) RouteDelete(dest *net.IPNet) error {
	route := &netlink.Route{Dst: dest}
	if err := netlink.RouteDel(route); err != nil {
		if isMissingRouteDeleteError(err) {
			return nil // already absent, idempotent delete
		}
		return fmt.Errorf("route del %s: %w", dest, err)
	}
	return nil
}

// SetECMPRoute installs a multipath ECMP route with weighted nexthops.
func (r *NetlinkRouter) SetECMPRoute(dest *net.IPNet, nexthops []NextHop, src ...net.IP) error {
	if len(nexthops) == 0 {
		return fmt.Errorf("at least one nexthop required for ECMP route to %s", dest)
	}

	multipath := make([]*netlink.NexthopInfo, 0, len(nexthops))
	for _, nh := range nexthops {
		link, err := netlink.LinkByName(nh.Dev)
		if err != nil {
			return fmt.Errorf("nexthop dev %q: %w", nh.Dev, err)
		}
		gw := net.ParseIP(nh.Via)
		if gw == nil {
			return fmt.Errorf("nexthop via %q: invalid IP", nh.Via)
		}
		gw = gw.To4()
		if gw == nil {
			return fmt.Errorf("nexthop via %q: IPv6 not supported for IPv4 ECMP route", nh.Via)
		}
		weight := nh.Weight
		if weight < 1 {
			weight = 1
		}
		multipath = append(multipath, &netlink.NexthopInfo{
			LinkIndex: link.Attrs().Index,
			Hops:      weight - 1, // netlink uses hops = weight - 1
			Gw:        gw,
		})
	}

	route := &netlink.Route{
		Dst:       dest,
		MultiPath: multipath,
	}
	if len(src) > 0 && src[0] != nil {
		route.Src = src[0].To4()
	}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("ecmp route replace %s (%d nexthops): %w", dest, len(nexthops), err)
	}
	return nil
}

// RemoveECMPRoute removes the route to dest.
func (r *NetlinkRouter) RemoveECMPRoute(dest *net.IPNet) error {
	return r.RouteDelete(dest)
}

// ListRoutes returns all IPv4 routes.
func (r *NetlinkRouter) ListRoutes() ([]RouteEntry, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("route list: %w", err)
	}

	entries := make([]RouteEntry, 0, len(routes))
	for _, route := range routes {
		dest := "default"
		if route.Dst != nil {
			dest = route.Dst.String()
		}
		via := ""
		if route.Gw != nil {
			via = route.Gw.String()
		}
		dev := ""
		if route.LinkIndex > 0 {
			if link, linkErr := netlink.LinkByIndex(route.LinkIndex); linkErr == nil {
				dev = link.Attrs().Name
			}
		}
		entries = append(entries, RouteEntry{
			Destination: dest,
			Via:         via,
			Device:      dev,
		})
	}
	return entries, nil
}

// AddrAdd assigns an IP address to an interface.
func (r *NetlinkRouter) AddrAdd(ifaceName string, addr *net.IPNet) error {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("link %q: %w", ifaceName, err)
	}

	netlinkAddr := &netlink.Addr{IPNet: addr}
	if err := netlink.AddrAdd(link, netlinkAddr); err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return nil // already assigned, idempotent
		}
		return fmt.Errorf("addr add %s dev %s: %w", addr, ifaceName, err)
	}
	return nil
}

// AddrExists checks if an address is already assigned to an interface.
func (r *NetlinkRouter) AddrExists(ifaceName string, addr *net.IPNet) (bool, error) {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return false, fmt.Errorf("link %q: %w", ifaceName, err)
	}

	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return false, fmt.Errorf("addr list %s: %w", ifaceName, err)
	}

	for _, existing := range addrs {
		if existing.IP.Equal(addr.IP) {
			return true, nil
		}
	}
	return false, nil
}

// LinkSetUp brings a network interface up.
func (r *NetlinkRouter) LinkSetUp(ifaceName string) error {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("link %q: %w", ifaceName, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("link set up %s: %w", ifaceName, err)
	}
	return nil
}

// LinkGetIndex returns the kernel interface index for a named interface.
func (r *NetlinkRouter) LinkGetIndex(ifaceName string) (int, error) {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return 0, fmt.Errorf("link %q: %w", ifaceName, err)
	}
	return link.Attrs().Index, nil
}
