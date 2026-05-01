//go:build !linux

package node

import "errors"

func installVRFOverlayPolicyRule(_ string, _ int) error {
	return errors.New("vrf policy rule: not supported on this platform")
}
