package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClientState persists client configuration for restart recovery.
// Saved after mesh-ctl client init, loaded on container restart.
type ClientState struct {
	OverlayIP       string               `yaml:"overlay_ip"`
	RoutingPolicies []RoutingPolicyState `yaml:"routing_policies,omitempty"`
	DNS             *DNSState            `yaml:"dns,omitempty"`
	Masters         []NodeRef            `yaml:"masters,omitempty"`
	Endpoints       []NodeRef            `yaml:"endpoints,omitempty"`
}

// RoutingPolicyState mirrors topology.RoutingPolicy for persistence.
type RoutingPolicyState struct {
	Name    string   `yaml:"name"`
	DSCP    int      `yaml:"dscp"`
	Targets []string `yaml:"targets"`
}

// DNSState mirrors topology.DNSConfig for persistence.
type DNSState struct {
	Zone     string `yaml:"zone"`
	Listen   string `yaml:"listen,omitempty"`
	Upstream string `yaml:"upstream,omitempty"`
}

// NodeRef stores minimal node info for state recovery.
type NodeRef struct {
	Name      string `yaml:"name"`
	OverlayIP string `yaml:"overlay_ip"`
}

const clientStateFile = "client-state.yml"

// saveClientState persists client configuration to /config/client-state.yml.
func saveClientState(configDir string, state ClientState) error {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return fmt.Errorf("config directory is required")
	}

	data, err := yaml.Marshal(&state)
	if err != nil {
		return fmt.Errorf("marshal client state: %w", err)
	}

	path := filepath.Join(configDir, clientStateFile)
	tmpPath := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create client state directory: %w", err)
	}
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write client state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace client state: %w", err)
	}
	return nil
}

// loadClientState reads client configuration from /config/client-state.yml.
func loadClientState(configDir string) (ClientState, error) {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return ClientState{}, fmt.Errorf("config directory is required")
	}

	path := filepath.Join(configDir, clientStateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ClientState{}, nil
		}
		return ClientState{}, err
	}

	var state ClientState
	if err := yaml.Unmarshal(data, &state); err != nil {
		return ClientState{}, err
	}
	return state, nil
}
