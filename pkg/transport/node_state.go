package transport

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

// CurrentSchemaVersion is the transport.yml schema version written by v1.6.0+.
// Files written by older releases will have SchemaVersion == 0 (legacy).
const CurrentSchemaVersion = 1

// NodeTransportState is persisted to /config/transport.yml on the node after
// AddTunnel/AddPeer. Shared between pkg/node and pkg/grpc to ensure a single
// source of truth for the transport.yml schema.
type NodeTransportState struct {
	// SchemaVersion identifies the transport.yml schema generation.
	// 0 means a file written by a pre-v1.6.0 release (legacy defaults apply).
	// 1 means a file written by v1.6.0+ (AllowedIPs and PersistentKeepalive are authoritative).
	SchemaVersion int              `yaml:"schema_version,omitempty"`
	OverlayIP     string           `yaml:"overlay_ip"`
	Tunnels       []TunnelTransport `yaml:"tunnels"`
}

// TunnelTransport persists per-tunnel transport-layer state on the node side.
//
// Schema version 1 (v1.7.0+): adds AllowedIPs and PersistentKeepalive to preserve
// topology-driven values across restarts. See .agent/specs/client-ecmp/spec.md
// FR-4, FR-5 for the design rationale.
type TunnelTransport struct {
	Name            string `yaml:"name"`
	OverlayIP       string `yaml:"overlay_ip,omitempty"`
	TransportIP     string `yaml:"transport_ip"`
	PeerTransportIP string `yaml:"peer_transport_ip"`
	PeerPublicKey   string `yaml:"peer_public_key"`
	PeerEndpoint    string `yaml:"peer_endpoint"`
	BalancerIP      string `yaml:"balancer_ip,omitempty"`
	// AllowedIPs is the list of CIDR prefixes routed through this tunnel peer.
	// Written by v1.6.0+; empty in legacy state files (see IsLegacySchema).
	AllowedIPs []string `yaml:"allowed_ips,omitempty"`
	// PersistentKeepalive is the WireGuard keepalive interval in seconds for this peer.
	// Written by v1.6.0+; zero in legacy state files (see IsLegacySchema).
	PersistentKeepalive int32 `yaml:"persistent_keepalive,omitempty"`
}

// IsLegacySchema reports whether state was written by a pre-v1.6.0 release.
// Legacy files have SchemaVersion == 0 and omit AllowedIPs / PersistentKeepalive.
func IsLegacySchema(state NodeTransportState) bool {
	return state.SchemaVersion == 0
}

// ApplyLegacyDefaults fills in AllowedIPs and PersistentKeepalive for tunnels
// that lack them (i.e. state loaded from a pre-v1.6.0 transport.yml).
// It logs a single WARN and stamps SchemaVersion = CurrentSchemaVersion so the
// function is idempotent if called twice.
func ApplyLegacyDefaults(state *NodeTransportState, logger zerolog.Logger) {
	logger.Warn().
		Int("tunnel_count", len(state.Tunnels)).
		Str("event", "transport_state_legacy_schema").
		Msg("transport.yml pre-v1.6.0 schema detected; applying fallback defaults. Run 'mesh-ctl client init' to migrate.")

	for i := range state.Tunnels {
		if len(state.Tunnels[i].AllowedIPs) == 0 {
			state.Tunnels[i].AllowedIPs = []string{"0.0.0.0/0"}
		}
		if state.Tunnels[i].PersistentKeepalive == 0 {
			state.Tunnels[i].PersistentKeepalive = 25
		}
	}

	state.SchemaVersion = CurrentSchemaVersion
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
