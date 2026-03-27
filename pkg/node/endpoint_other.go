//go:build !linux

package node

import "fmt"

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
