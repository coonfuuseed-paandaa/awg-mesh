package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	"gopkg.in/yaml.v3"
)

const nodeStateFileName = "node.yml"

// NodeState stores persisted node identity and metadata.
type NodeState struct {
	PrivateKey string `yaml:"private_key"`
	PublicKey  string `yaml:"public_key"`
	Name       string `yaml:"name"`
	Mode       string `yaml:"mode"`
	OverlayIP  string `yaml:"overlay_ip"`
}

// SaveNodeState writes node state to node.yml in dir.
func SaveNodeState(dir string, state NodeState) error {
	cleanDir := strings.TrimSpace(dir)
	if cleanDir == "" {
		return fmt.Errorf("config directory is required")
	}

	if err := os.MkdirAll(cleanDir, 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	data, err := yaml.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal node state: %w", err)
	}

	statePath := filepath.Join(cleanDir, nodeStateFileName)
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		return fmt.Errorf("write node state file: %w", err)
	}

	return nil
}

// LoadNodeState reads node state from node.yml in dir.
func LoadNodeState(dir string) (*NodeState, error) {
	cleanDir := strings.TrimSpace(dir)
	if cleanDir == "" {
		return nil, fmt.Errorf("config directory is required")
	}

	statePath := filepath.Join(cleanDir, nodeStateFileName)
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("read node state file: %w", err)
	}

	var state NodeState
	if err := yaml.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal node state yaml: %w", err)
	}

	return &state, nil
}

// EnsureKeypair loads persisted keys or creates and saves a new keypair.
func EnsureKeypair(dir string) (privateKey wg.Key, publicKey wg.Key, err error) {
	state, loadErr := LoadNodeState(dir)
	if loadErr == nil {
		privateKey, err = parseNodeKey(state.PrivateKey, "private")
		if err != nil {
			return wg.Key{}, wg.Key{}, err
		}

		publicKey, err = parseNodeKey(state.PublicKey, "public")
		if err != nil {
			return wg.Key{}, wg.Key{}, err
		}

		return privateKey, publicKey, nil
	}

	if !errors.Is(loadErr, os.ErrNotExist) {
		return wg.Key{}, wg.Key{}, fmt.Errorf("load node state: %w", loadErr)
	}

	privateKey, err = wg.GeneratePrivateKey()
	if err != nil {
		return wg.Key{}, wg.Key{}, fmt.Errorf("generate private key: %w", err)
	}

	publicKey = privateKey.PublicKey()
	stateToSave := NodeState{
		PrivateKey: privateKey.String(),
		PublicKey:  publicKey.String(),
		Name:       "",
		Mode:       "",
		OverlayIP:  "",
	}

	if err := SaveNodeState(dir, stateToSave); err != nil {
		return wg.Key{}, wg.Key{}, fmt.Errorf("save node state: %w", err)
	}

	return privateKey, publicKey, nil
}

func parseNodeKey(value string, keyName string) (wg.Key, error) {
	if strings.TrimSpace(value) == "" {
		return wg.Key{}, fmt.Errorf("%s key is empty in node state", keyName)
	}

	parsedKey, err := wg.ParseKey(value)
	if err != nil {
		return wg.Key{}, fmt.Errorf("parse %s key from node state: %w", keyName, err)
	}

	return parsedKey, nil
}
