package transport

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// NodeTransportState is persisted to /config/transport.yml on the node after
// AddTunnel/AddPeer. Shared between pkg/node and pkg/grpc to ensure a single
// source of truth for the transport.yml schema.
type NodeTransportState struct {
	OverlayIP string           `yaml:"overlay_ip"`
	Tunnels   []TunnelTransport `yaml:"tunnels"`
}

// TunnelTransport records transport addressing for one tunnel.
type TunnelTransport struct {
	Name            string `yaml:"name"`
	OverlayIP       string `yaml:"overlay_ip,omitempty"`
	TransportIP     string `yaml:"transport_ip"`
	PeerTransportIP string `yaml:"peer_transport_ip"`
	PeerPublicKey   string `yaml:"peer_public_key"`
	PeerEndpoint    string `yaml:"peer_endpoint"`
	BalancerIP      string `yaml:"balancer_ip,omitempty"`
}

// SaveNodeTransportState writes transport.yml atomically.
func SaveNodeTransportState(path string, state NodeTransportState) error {
	data, err := yaml.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal node transport state %q: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create transport state directory for %q: %w", path, err)
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write temporary node transport state %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace node transport state %q: %w", path, err)
	}
	return nil
}

// LoadNodeTransportState reads transport.yml. Returns zero value if not found.
func LoadNodeTransportState(configDir string) (NodeTransportState, error) {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return NodeTransportState{}, fmt.Errorf("config directory is required")
	}

	path := filepath.Join(configDir, "transport.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NodeTransportState{}, nil
		}
		return NodeTransportState{}, fmt.Errorf("read node transport state %q: %w", path, err)
	}

	var state NodeTransportState
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&state); err != nil {
		return NodeTransportState{}, fmt.Errorf("unmarshal node transport state %q: %w", path, err)
	}
	return state, nil
}
