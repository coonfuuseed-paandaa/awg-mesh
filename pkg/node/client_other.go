//go:build !linux

package node

import (
	"context"
	"fmt"
)

type clientPlatformState struct{}

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
