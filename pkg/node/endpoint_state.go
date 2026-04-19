package node

import (
	"errors"
	"fmt"
	"os"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

// LoadKeypair returns the raw 32-byte private key from node.yml.
// tunnelName is accepted for interface compatibility; the state file path is
// fixed per node (one node.yml per container).
// Returns (nil, os.ErrNotExist) if node.yml does not yet exist.
// Returns a non-nil error wrapping os.ErrNotExist for missing file, or a
// distinct error for corrupt / permission failures (NFR-6 fail-closed).
func (e *EndpointRunner) LoadKeypair(tunnelName string) ([]byte, error) {
	state, err := LoadNodeState(e.node.config.ConfigDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("load node state for keypair (tunnel %s): %w", tunnelName, err)
	}

	privKey, parseErr := wg.ParseKey(state.PrivateKey)
	if parseErr != nil {
		return nil, fmt.Errorf("parse persisted private key (tunnel %s): %w", tunnelName, parseErr)
	}

	raw := privKey[:]
	result := make([]byte, len(raw))
	copy(result, raw)
	return result, nil
}

// PersistKeypair atomically writes privKey and its derived public key to
// node.yml via .tmp + rename (mode 0600, delegated to SaveNodeState).
// Fail-closed per NFR-6: only synthesizes fresh state when node.yml does not
// exist (os.ErrNotExist). Corrupt / permission errors propagate as-is.
func (e *EndpointRunner) PersistKeypair(tunnelName string, privKey []byte) error {
	if len(privKey) != 32 {
		return fmt.Errorf("private key must be 32 bytes, got %d (tunnel %s)", len(privKey), tunnelName)
	}

	var newPrivKey wg.Key
	copy(newPrivKey[:], privKey)
	newPubKey := newPrivKey.PublicKey()

	state, err := LoadNodeState(e.node.config.ConfigDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// NFR-6: propagate corrupt / I/O errors — do NOT overwrite.
			return fmt.Errorf("load node state before persist (tunnel %s): %w", tunnelName, err)
		}
		// os.ErrNotExist — synthesize fresh state.
		state = &NodeState{}
	}

	updated := NodeState{
		PrivateKey: newPrivKey.String(),
		PublicKey:  newPubKey.String(),
		Name:       state.Name,
		Mode:       state.Mode,
		OverlayIP:  state.OverlayIP,
	}
	// SaveNodeState uses atomic .tmp + rename at mode 0600 (see config.go).
	return SaveNodeState(e.node.config.ConfigDir, updated)
}

// LockRotation acquires the per-EndpointRunner rotation mutex and returns an
// unlock func. Callers MUST defer the returned func immediately (NFR-5).
// tunnelName is accepted for interface compatibility; a single mutex serializes
// all rotation RPCs for this endpoint (endpoints serve one tunnel in v1).
func (e *EndpointRunner) LockRotation(tunnelName string) (func(), error) {
	e.rotateMu.Lock()
	return e.rotateMu.Unlock, nil
}
