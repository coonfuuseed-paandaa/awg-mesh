//go:build linux

package node

import (
	"os"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// discoverWANInterface returns the network interface name associated with the default IPv4 route.
// If MESH_INTERFACE env var is set and non-empty, it is used directly.
// Falls back to "eth0" if no default route is found.
func discoverWANInterface() string {
	if envIface := strings.TrimSpace(os.Getenv("MESH_INTERFACE")); envIface != "" {
		return envIface
	}

	routes, err := netlink.RouteList(nil, unix.AF_INET)
	if err != nil {
		return "eth0"
	}

	for _, r := range routes {
		if r.Dst == nil { // default route
			link, err := netlink.LinkByIndex(r.LinkIndex)
			if err == nil {
				return link.Attrs().Name
			}
		}
	}

	return "eth0"
}
