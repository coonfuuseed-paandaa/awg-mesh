//go:build !linux

package node

import (
	"fmt"

	grpcserver "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	"github.com/rs/zerolog"
)

// detectLegacyWg0 is a no-op stub on non-Linux platforms. Legacy wg0 migration
// only applies to Linux kernel network interfaces.
func detectLegacyWg0() bool { return false }

// migrateLegacyWg0 is a no-op stub on non-Linux platforms.
func migrateLegacyWg0(_ zerolog.Logger) error { return nil }

// endpointPlatformState is an empty stub on non-Linux platforms.
// On Linux, this struct holds the ifaces map and its protecting mutex.
// The wg.Interface type is Linux-only, so this struct cannot reference it here.
type endpointPlatformState struct{}

func (e *EndpointRunner) createInterface() error {
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}

	e.node.logger.Warn().Msg("AWG interface not available on this platform")
	return nil
}

// closeAllIfaces is a no-op on non-Linux platforms (no interfaces are created).
func (e *EndpointRunner) closeAllIfaces() error {
	return nil
}

// cleanupStaleIfaces is a no-op on non-Linux platforms.
func (e *EndpointRunner) cleanupStaleIfaces(_ map[string]bool) {}

func (e *EndpointRunner) ApplyParams(tunnelName string, cfg wg.Config) error {
	return fmt.Errorf("UAPI not supported on this platform")
}

func (e *EndpointRunner) ListPeers() []grpcserver.PeerInfo {
	return nil
}

func (e *EndpointRunner) AddPeer(publicKey []byte, presharedKey []byte, allowedIPs []string, endpointHost string, persistentKeepalive int32, peerName string) error {
	return fmt.Errorf("peer management not supported on this platform")
}

func (e *EndpointRunner) RemovePeer(publicKey []byte) error {
	return fmt.Errorf("peer management not supported on this platform")
}

func (e *EndpointRunner) ConfigureTransport(pubkeyHex, localIP, peerIP string, allowedIPs []string, peerName string, extraRoutes []string) error {
	return fmt.Errorf("transport configuration not supported on this platform")
}

// GetListenPort is a no-op stub on non-Linux platforms.
func (e *EndpointRunner) GetListenPort(_ string) (int, error) {
	return 0, nil
}
