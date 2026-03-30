//go:build !linux

package routing

import (
	"errors"
	"net"
)

// RouteEntry represents a single routing table entry.
type RouteEntry struct {
	Destination string
	Via         string
	Device      string
}

// NextHop describes a single nexthop in an ECMP route.
type NextHop struct {
	Via    string
	Dev    string
	Weight int
}

var errNotSupported = errors.New("routing: not supported on this platform")

// AddRoute is not supported on non-Linux platforms.
func AddRoute(dest string, via string, dev string) error { return errNotSupported }

// DeleteRoute is not supported on non-Linux platforms.
func DeleteRoute(dest string) error { return errNotSupported }

// ReplaceRoute is not supported on non-Linux platforms.
func ReplaceRoute(dest string, via string, dev string) error { return errNotSupported }

// ListRoutes is not supported on non-Linux platforms.
func ListRoutes() ([]RouteEntry, error) { return nil, errNotSupported }

// SetECMPRoute is not supported on non-Linux platforms.
func SetECMPRoute(dest string, nexthops []NextHop) error { return errNotSupported }

// RemoveECMPRoute is not supported on non-Linux platforms.
func RemoveECMPRoute(dest string) error { return errNotSupported }

// EnableMasquerade is not supported on non-Linux platforms.
func EnableMasquerade(iface string) error { return errNotSupported }

// DisableMasquerade is not supported on non-Linux platforms.
func DisableMasquerade(iface string) error { return errNotSupported }

// EnableForwarding is not supported on non-Linux platforms.
func EnableForwarding() error { return errNotSupported }

// ClampMSS is not supported on non-Linux platforms.
func ClampMSS(iface string, mss int) error { return errNotSupported }

// RemoveMSSClamp is not supported on non-Linux platforms.
func RemoveMSSClamp(iface string, mss int) error { return errNotSupported }

// ClampMSSToPMTU is not supported on non-Linux platforms.
func ClampMSSToPMTU() error { return errNotSupported }

// EnableStickyECMP is not supported on non-Linux platforms.
func EnableStickyECMP(balancerCIDR string) error { return errNotSupported }

// DisableStickyECMP is not supported on non-Linux platforms.
func DisableStickyECMP(balancerCIDR string) error { return errNotSupported }

// EnableL4Hash is not supported on non-Linux platforms.
func EnableL4Hash() error { return errNotSupported }

// NetlinkRouter stub for non-Linux platforms.
type NetlinkRouter struct{}

// NewNetlinkRouter returns a stub router on non-Linux platforms.
func NewNetlinkRouter() *NetlinkRouter { return &NetlinkRouter{} }

func (r *NetlinkRouter) RouteAdd(_ *net.IPNet, _ net.IP, _ string) error        { return errNotSupported }
func (r *NetlinkRouter) RouteReplace(_ *net.IPNet, _ net.IP, _ string) error   { return errNotSupported }
func (r *NetlinkRouter) RouteDelete(_ *net.IPNet) error                         { return errNotSupported }
func (r *NetlinkRouter) SetECMPRoute(_ *net.IPNet, _ []NextHop) error          { return errNotSupported }
func (r *NetlinkRouter) RemoveECMPRoute(_ *net.IPNet) error                     { return errNotSupported }
func (r *NetlinkRouter) ListRoutes() ([]RouteEntry, error)                      { return nil, errNotSupported }
func (r *NetlinkRouter) AddrAdd(_ string, _ *net.IPNet) error                   { return errNotSupported }
func (r *NetlinkRouter) AddrExists(_ string, _ *net.IPNet) (bool, error)       { return false, errNotSupported }
func (r *NetlinkRouter) LinkSetUp(_ string) error                               { return errNotSupported }
func (r *NetlinkRouter) LinkGetIndex(_ string) (int, error)                     { return 0, errNotSupported }

// ProcSysctl stub for non-Linux platforms.
type ProcSysctl struct{}

// NewProcSysctl returns a stub sysctl on non-Linux platforms.
func NewProcSysctl() *ProcSysctl                 { return &ProcSysctl{} }
func (s *ProcSysctl) EnableForwarding() error     { return errNotSupported }
func (s *ProcSysctl) EnableL4Hash() error         { return errNotSupported }

// NftablesFirewall stub for non-Linux platforms.
type NftablesFirewall struct{}

// NewNftablesFirewall returns an error on non-Linux platforms.
func NewNftablesFirewall() (*NftablesFirewall, error) { return nil, errNotSupported }
func (f *NftablesFirewall) SetupNAT(_ string) error             { return errNotSupported }
func (f *NftablesFirewall) TeardownNAT() error                  { return errNotSupported }
func (f *NftablesFirewall) ClampMSSToPMTU() error               { return errNotSupported }
func (f *NftablesFirewall) EnableStickyECMP(_ string) error     { return errNotSupported }
func (f *NftablesFirewall) DisableStickyECMP(_ string) error    { return errNotSupported }
