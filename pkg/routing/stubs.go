//go:build !linux

package routing

import (
	"errors"
	"net"
)

var errNotSupported = errors.New("routing: not supported on this platform")

// NetlinkRouter stub for non-Linux platforms.
type NetlinkRouter struct{}

// NewNetlinkRouter returns a stub router on non-Linux platforms.
func NewNetlinkRouter() *NetlinkRouter { return &NetlinkRouter{} }

func (r *NetlinkRouter) RouteAdd(_ *net.IPNet, _ net.IP, _ string) error     { return errNotSupported }
func (r *NetlinkRouter) RouteReplace(_ *net.IPNet, _ net.IP, _ string) error { return errNotSupported }
func (r *NetlinkRouter) RouteReplaceLink(_ *net.IPNet, _ string) error       { return errNotSupported }
func (r *NetlinkRouter) RouteReplaceLinkWithSrc(_ *net.IPNet, _ string, _ net.IP) error {
	return errNotSupported
}
func (r *NetlinkRouter) RouteDelete(_ *net.IPNet) error               { return errNotSupported }
func (r *NetlinkRouter) SetECMPRoute(_ *net.IPNet, _ []NextHop, _ ...net.IP) error {
	return errNotSupported
}
func (r *NetlinkRouter) RemoveECMPRoute(_ *net.IPNet) error           { return errNotSupported }
func (r *NetlinkRouter) ListRoutes() ([]RouteEntry, error)            { return nil, errNotSupported }
func (r *NetlinkRouter) AddrAdd(_ string, _ *net.IPNet) error         { return errNotSupported }
func (r *NetlinkRouter) AddrExists(_ string, _ *net.IPNet) (bool, error) {
	return false, errNotSupported
}
func (r *NetlinkRouter) LinkSetUp(_ string) error           { return errNotSupported }
func (r *NetlinkRouter) LinkGetIndex(_ string) (int, error) { return 0, errNotSupported }

// ProcSysctl stub for non-Linux platforms.
type ProcSysctl struct{}

// NewProcSysctl returns a stub sysctl on non-Linux platforms.
func NewProcSysctl() *ProcSysctl              { return &ProcSysctl{} }
func (s *ProcSysctl) EnableForwarding() error { return errNotSupported }
func (s *ProcSysctl) EnableL4Hash() error     { return errNotSupported }

// NftablesFirewall stub for non-Linux platforms.
type NftablesFirewall struct{}

// NewNftablesFirewall returns an error on non-Linux platforms.
func NewNftablesFirewall() (*NftablesFirewall, error)         { return nil, errNotSupported }
func (f *NftablesFirewall) SetupNAT(_ string) error           { return errNotSupported }
func (f *NftablesFirewall) TeardownNAT() error                { return errNotSupported }
func (f *NftablesFirewall) ClampMSSToPMTU() error             { return errNotSupported }
func (f *NftablesFirewall) EnableStickyECMP(_ string) error   { return errNotSupported }
func (f *NftablesFirewall) DisableStickyECMP(_ string) error  { return errNotSupported }
func (f *NftablesFirewall) EnableWGCrossTunnelForward() error { return errNotSupported }

// DSCPPolicy stub for non-Linux platforms.
type DSCPPolicy struct {
	DSCP    int
	Fwmark  int
	TableID int
	Gateway string
	Device  string
}

// SetupDSCPPolicyRouting is a no-op on non-Linux platforms.
func SetupDSCPPolicyRouting(_ []DSCPPolicy) error { return errNotSupported }

// TeardownDSCPPolicyRouting is a no-op on non-Linux platforms.
func TeardownDSCPPolicyRouting() error { return errNotSupported }
