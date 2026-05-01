//go:build !linux

package node

import (
	"errors"

	"golang.org/x/net/icmp"
)

// bindICMPSocketToVRF is a non-Linux stub. F-008 VRF separation is
// Linux-kernel-only; non-Linux builds never call BindToVRF with a non-empty
// name (caller checks platform), but if they do this returns a clear error.
func bindICMPSocketToVRF(_ *icmp.PacketConn, _ string) error {
	return errors.New("vrf bind: not supported on this platform")
}
