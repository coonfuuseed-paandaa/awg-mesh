package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"gopkg.in/yaml.v3"
)

// NodeTransportState is persisted to /config/transport.yml on the node after AddTunnel/AddPeer.
type NodeTransportState struct {
	OverlayIP string           `yaml:"overlay_ip"`
	Tunnels   []TunnelTransport `yaml:"tunnels"`
}

// TunnelTransport records transport addressing for one tunnel.
type TunnelTransport struct {
	Name            string `yaml:"name"`
	TransportIP     string `yaml:"transport_ip"`
	PeerTransportIP string `yaml:"peer_transport_ip"`
	PeerPublicKey   string `yaml:"peer_public_key"`
	PeerEndpoint    string `yaml:"peer_endpoint"`
}

func saveNodeTransportState(configDir string, state NodeTransportState) error {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return fmt.Errorf("config directory is required")
	}

	data, err := yaml.Marshal(&state)
	if err != nil {
		return fmt.Errorf("marshal node transport state: %w", err)
	}
	path := filepath.Join(configDir, "transport.yml")
	tmpPath := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create transport state directory: %w", err)
	}
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write node transport state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace node transport state: %w", err)
	}
	return nil
}

func loadNodeTransportState(configDir string) (NodeTransportState, error) {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return NodeTransportState{}, fmt.Errorf("config directory is required")
	}

	path := filepath.Join(configDir, "transport.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NodeTransportState{}, nil
		}
		return NodeTransportState{}, err
	}

	var state NodeTransportState
	if err := yaml.Unmarshal(data, &state); err != nil {
		return NodeTransportState{}, err
	}
	return state, nil
}
