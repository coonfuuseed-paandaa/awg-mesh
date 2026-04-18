//go:build !linux

package node

import (
	"fmt"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

// applyPeerKeyUpdate is not supported on non-Linux platforms.
func (m *MasterRunner) applyPeerKeyUpdate(tunnel *MasterTunnel, newPubkey wg.Key, allowedIPs []string) error {
	return fmt.Errorf("wgctrl peer update not supported on this platform")
}
