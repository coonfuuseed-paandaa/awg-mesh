package node

import (
	"context"
	"fmt"
	"time"

	grpcserver "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

// ClientRunner runs node logic for client mode.
type ClientRunner struct {
	node          *Node
	startTime     time.Time
	platformState clientPlatformState
}

// NewClientRunner creates a client mode runner.
func NewClientRunner(node *Node) *ClientRunner {
	return &ClientRunner{
		node:          node,
		platformState: initClientPlatformState(),
	}
}

// GetPublicKey returns the client's WireGuard public key.
func (c *ClientRunner) GetPublicKey() (wg.Key, error) {
	_, pubKey, err := EnsureKeypair(c.node.config.ConfigDir)
	return pubKey, err
}

// Run starts client mode and blocks until context cancellation.
func (c *ClientRunner) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if c == nil || c.node == nil {
		return fmt.Errorf("client runner node is required")
	}

	_, publicKey, err := EnsureKeypair(c.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}
	if c.node.config.OverlayIP != "" {
		if err := AssignOverlayIP(c.node.config.OverlayIP); err != nil {
			return fmt.Errorf("assign overlay IP: %w", err)
		}
	}
	if err := startGRPCServer(ctx, c.node.config.ConfigDir, c.node.logger, nil, nil, c, c, nil, c); err != nil {
		return fmt.Errorf("start gRPC server: %w", err)
	}
	c.startTime = time.Now()

	if err := c.createInterfaces(ctx); err != nil {
		return fmt.Errorf("create client interfaces: %w", err)
	}
	defer func() {
		c.teardownDSCPRouting()
		if closeErr := c.closeInterfaces(); closeErr != nil {
			c.node.logger.Warn().Err(closeErr).Msg("failed to close client interfaces")
		}
	}()

	if err := c.reconcileFromTransportState(); err != nil {
		return fmt.Errorf("reconcile client transport state: %w", err)
	}

	if err := c.setupDSCPRouting(); err != nil {
		c.node.logger.Warn().Err(err).Msg("setup DSCP policy routing failed (non-fatal)")
	}

	c.node.logger.Info().
		Str("public_key", publicKey.String()).
		Msg("client runner started")
	c.node.logger.Info().Str("wan_interface", discoverWANInterface()).Msg("WAN interface discovered")

	c.startHealthCheck(ctx)

	<-ctx.Done()

	c.node.logger.Info().Msg("client runner stopping")
	return nil
}

func (c *ClientRunner) GetNodeState() grpcserver.NodeState {
	return grpcserver.NodeState{
		Name:      c.node.config.Name,
		Mode:      "client",
		OverlayIP: c.node.config.OverlayIP,
		StartTime: c.startTime,
	}
}
