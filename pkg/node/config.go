package node

import (
	"bytes"
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

// String returns a safe representation with the private key redacted.
func (s NodeState) String() string {
	return fmt.Sprintf("NodeState{Name:%s Mode:%s OverlayIP:%s PublicKey:%s PrivateKey:[REDACTED]}",
		s.Name, s.Mode, s.OverlayIP, s.PublicKey)
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
	tmpPath := statePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write node state temp file: %w", err)
	}
	if err := os.Rename(tmpPath, statePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename node state file: %w", err)
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
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&state); err != nil {
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
		// Only auto-recover from YAML parse errors (corrupt/truncated file).
		// Filesystem errors (permission denied, I/O error) should propagate.
		var yamlErr *yaml.TypeError
		isYAMLErr := errors.As(loadErr, &yamlErr) || strings.Contains(loadErr.Error(), "unmarshal node state yaml")
		if !isYAMLErr {
			return wg.Key{}, wg.Key{}, fmt.Errorf("load node state: %w", loadErr)
		}

		statePath := filepath.Join(strings.TrimSpace(dir), nodeStateFileName)
		if removeErr := os.Remove(statePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return wg.Key{}, wg.Key{}, fmt.Errorf("remove corrupt node state: %w (original: %w)", removeErr, loadErr)
		}
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
