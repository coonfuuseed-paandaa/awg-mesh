package node

import (
	"context"
	"fmt"
)

// EndpointRunner runs node logic for endpoint mode.
type EndpointRunner struct {
	node          *Node
	platformState endpointPlatformState
}

// NewEndpointRunner creates an endpoint mode runner.
func NewEndpointRunner(node *Node) *EndpointRunner {
	return &EndpointRunner{node: node}
}

// Run starts endpoint mode and blocks until context cancellation.
func (e *EndpointRunner) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}

	_, publicKey, err := EnsureKeypair(e.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}
	if err := startGRPCServer(ctx, e.node.config.ConfigDir, e.node.logger, nil, e); err != nil {
		return fmt.Errorf("start gRPC server: %w", err)
	}

	if err := e.createInterface(); err != nil {
		return fmt.Errorf("create endpoint interface: %w", err)
	}
	defer func() {
		if closeErr := e.closeInterface(); closeErr != nil {
			e.node.logger.Warn().Err(closeErr).Msg("failed to close endpoint interface")
		}
	}()

	e.node.logger.Info().
		Str("overlay_ip", e.node.config.OverlayIP).
		Str("public_key", publicKey.String()).
		Msg("endpoint runner started")

	<-ctx.Done()

	e.node.logger.Info().Msg("endpoint runner stopping")
	return nil
}
