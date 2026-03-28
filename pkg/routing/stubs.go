//go:build !linux

package routing

import "errors"

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
