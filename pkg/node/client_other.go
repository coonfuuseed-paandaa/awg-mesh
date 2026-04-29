//go:build !linux

package node

import (
	"context"
	"fmt"

	grpcserver "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
)

type clientPlatformState struct{}

func initClientPlatformState() clientPlatformState {
	return clientPlatformState{}
}

func (c *ClientRunner) createInterfaces(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if c == nil || c.node == nil {
		return fmt.Errorf("client runner node is required")
	}

	c.node.logger.Warn().Msg("AWG interfaces are not available on this platform")
	return nil
}

func (c *ClientRunner) closeInterfaces() error {
	return nil
}

func (c *ClientRunner) AddPeer(publicKey []byte, _ []byte, _ []string, _ string, _ int32, _ string) error {
	if len(publicKey) == 0 {
		return fmt.Errorf("public key is required")
	}
	return fmt.Errorf("client peer management is not available on this platform")
}

func (c *ClientRunner) ListPeers() []grpcserver.PeerInfo {
	return nil
}

func (c *ClientRunner) RemovePeer(publicKey []byte) error {
	if len(publicKey) == 0 {
		return fmt.Errorf("public key is required")
	}
	return fmt.Errorf("client peer management is not available on this platform")
}

func (c *ClientRunner) ConfigureTransport(_ string, _ string, _ string, _ []string, _ string, _ []string) error {
	return fmt.Errorf("client transport configuration is not available on this platform")
}

func (c *ClientRunner) reconcileFromTransportState() error {
	return nil
}

func (c *ClientRunner) startHealthCheck(_ context.Context) {}

func (c *ClientRunner) rebuildClientECMP(_ string) error { return nil }

func (c *ClientRunner) setupDSCPRouting() error { return nil }

func (c *ClientRunner) teardownDSCPRouting() {}

func (c *ClientRunner) SetBalancerIP(_, _ string) {}

func (c *ClientRunner) startDNSServer(_ context.Context) {}

func (c *ClientRunner) SaveClientState() error { return nil }

func (c *ClientRunner) setupClientFirewallRules() {}
