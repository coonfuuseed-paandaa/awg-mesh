//go:build !linux

package node

import (
	"fmt"

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
