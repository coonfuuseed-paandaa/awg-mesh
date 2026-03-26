package node

import (
	"context"
	"fmt"
)

// ClientRunner runs node logic for client mode.
type ClientRunner struct {
	node *Node
}

// NewClientRunner creates a client mode runner.
func NewClientRunner(node *Node) *ClientRunner {
	return &ClientRunner{node: node}
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

	c.node.logger.Info().
		Str("public_key", publicKey.String()).
		Msg("client runner started")

	if c.node.topology != nil {
		for _, master := range c.node.topology.Masters {
			c.node.logger.Info().
				Str("master", master.Name).
				Str("host", master.Host).
				Str("overlay_ip", master.OverlayIP).
				Msg("would connect to master")
		}
	}

	<-ctx.Done()

	c.node.logger.Info().Msg("client runner stopping")
	return nil
}
