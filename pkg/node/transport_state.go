package node

import (
	"path/filepath"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/transport"
)

// NodeTransportState is an alias for the shared transport state type.
type NodeTransportState = transport.NodeTransportState

// TunnelTransport is an alias for the shared tunnel transport type.
type TunnelTransport = transport.TunnelTransport

func saveNodeTransportState(configDir string, state NodeTransportState) error {
	path := filepath.Join(configDir, "transport.yml")
	return transport.SaveNodeTransportState(path, state)
}

func loadNodeTransportState(configDir string) (NodeTransportState, error) {
	return transport.LoadNodeTransportState(configDir)
}
