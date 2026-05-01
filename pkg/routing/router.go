//go:build linux

package routing

import "net"

// Router abstracts IP route, address, and link operations.
// Production implementation uses netlink; tests can mock.
type Router interface {
	RouteAdd(dest *net.IPNet, via net.IP, dev string) error
	RouteReplace(dest *net.IPNet, via net.IP, dev string) error
	RouteDelete(dest *net.IPNet) error
	SetECMPRoute(dest *net.IPNet, nexthops []NextHop, src ...net.IP) error
	SetECMPRouteInTable(dest *net.IPNet, nexthops []NextHop, table int, src ...net.IP) error
	RemoveECMPRoute(dest *net.IPNet) error
	ListRoutes() ([]RouteEntry, error)

	AddrAdd(ifaceName string, addr *net.IPNet) error
	AddrExists(ifaceName string, addr *net.IPNet) (bool, error)

	LinkSetUp(ifaceName string) error
	LinkGetIndex(ifaceName string) (int, error)
}

// Firewall abstracts nftables/iptables operations.
type Firewall interface {
	SetupNAT(iface string) error
	TeardownNAT() error
	ClampMSSToPMTU() error
	EnableStickyECMP(balancerCIDR string) error
	DisableStickyECMP(balancerCIDR string) error
	EnableWGCrossTunnelForward() error
}

// Sysctl abstracts kernel parameter management.
type Sysctl interface {
	EnableForwarding() error
	EnableL4Hash() error
}
