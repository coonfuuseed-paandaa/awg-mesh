//go:build !linux

package node

import (
	"errors"
	"net"
)

// VRFAnchorIfaceNamePrefix is the prefix for anchor dummy interface names.
// Defined on all platforms so callers compile cross-platform.
const VRFAnchorIfaceNamePrefix = "wg-vrf-"

// errNotSupported is returned by all VRFManager methods on non-Linux platforms.
var errNotSupported = errors.New("vrf: not supported on this platform")

// VRFManager is an opaque struct on non-Linux platforms. All methods return
// errNotSupported so callers compile and fail predictably at runtime.
type VRFManager struct{}

// NewVRFManager constructs a no-op VRFManager on non-Linux platforms.
func NewVRFManager(_ string, _ uint32, _ net.IP) *VRFManager {
	return &VRFManager{}
}

func (m *VRFManager) Setup() error                    { return errNotSupported }
func (m *VRFManager) EnslaveInterface(_ string) error { return errNotSupported }
func (m *VRFManager) UnslaveInterface(_ string) error { return errNotSupported }
func (m *VRFManager) Teardown() error                 { return errNotSupported }
func (m *VRFManager) IsCreated() bool                 { return false }
func (m *VRFManager) Name() string                    { return "" }
func (m *VRFManager) Table() uint32                   { return 0 }

// IsVRFSupported always returns errNotSupported on non-Linux platforms.
func IsVRFSupported() error { return errNotSupported }
