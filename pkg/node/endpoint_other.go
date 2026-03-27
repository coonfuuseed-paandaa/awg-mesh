//go:build !linux

package node

import (
	"fmt"

	grpcserver "github.com/thebtf/awg-mesh/pkg/grpc"
	"github.com/thebtf/awg-mesh/pkg/wg"
)

type endpointPlatformState struct{}

func (e *EndpointRunner) createInterface() error {
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}

	e.node.logger.Warn().Msg("AWG interface not available on this platform")
	return nil
}

func (e *EndpointRunner) closeInterface() error {
	return nil
}

func (e *EndpointRunner) ApplyParams(tunnelName string, cfg wg.Config) error {
	return fmt.Errorf("UAPI not supported on this platform")
}

func (e *EndpointRunner) ListPeers() []grpcserver.PeerInfo {
	return nil
}

func (e *EndpointRunner) AddPeer(publicKey []byte, presharedKey []byte, allowedIPs []string, endpointHost string, persistentKeepalive int32) error {
	return fmt.Errorf("peer management not supported on this platform")
}

func (e *EndpointRunner) RemovePeer(publicKey []byte) error {
	return fmt.Errorf("peer management not supported on this platform")
}
