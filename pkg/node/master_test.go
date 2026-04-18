package node

import (
	"errors"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	"github.com/rs/zerolog"
)

// newTestMasterRunner constructs a MasterRunner suitable for unit tests.
// It uses a temp-dir-backed Node so saveTransportState has a real filesystem.
// The node has no topology and no running goroutines — safe to use directly.
func newTestMasterRunner(t *testing.T) *MasterRunner {
	t.Helper()
	configDir := t.TempDir()
	logger := zerolog.Nop()
	return &MasterRunner{
		node: &Node{
			config: NodeConfig{
				Name:      "test-master",
				Mode:      "master",
				ConfigDir: configDir,
			},
			logger: logger,
		},
		tunnels: make(map[string]*MasterTunnel),
	}
}

// injectTunnel inserts a pre-built MasterTunnel into the runner's tunnels map.
// Bypasses AddTunnel so no wgctrl or interface creation is attempted.
func injectTunnel(m *MasterRunner, t *MasterTunnel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tunnels[t.Name] = t
}

// makeKey returns a deterministic 32-byte WireGuard key from a seed byte.
func makeKey(seed byte) wg.Key {
	var k wg.Key
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k
}

// TestUpdateTunnelPeer_TunnelNotFound verifies that UpdateTunnelPeer returns
// ErrTunnelNotFound (via errors.Is) when the named tunnel does not exist, and
// that the tunnels map is unchanged.
//
// Anti-stub guarantee: replacing the not-found branch with `return false, nil`
// causes this test to fail because err == nil.
func TestUpdateTunnelPeer_TunnelNotFound(t *testing.T) {
	t.Parallel()

	m := newTestMasterRunner(t)
	// No tunnels injected — map is empty.

	var key [32]byte
	unchanged, err := m.UpdateTunnelPeer("nonexistent", key, "", nil)

	if err == nil {
		t.Fatal("expected error for missing tunnel, got nil")
	}
	if !errors.Is(err, ErrTunnelNotFound) {
		t.Fatalf("expected ErrTunnelNotFound, got: %v", err)
	}
	if unchanged {
		t.Error("unchanged must be false on error")
	}

	// Assert no state mutation: map is still empty.
	m.mu.RLock()
	count := len(m.tunnels)
	m.mu.RUnlock()
	if count != 0 {
		t.Errorf("tunnels map length = %d after not-found, want 0", count)
	}
}

// TestUpdateTunnelPeer_SameKey verifies that UpdateTunnelPeer returns
// (unchanged=true, nil) when the new key matches the current peer key,
// without touching the data plane (applyPeerKeyUpdateFn must NOT be called).
//
// Anti-stub guarantee: replacing the same-key check with a direct UAPI call
// causes the applyFnCalled counter to be non-zero, failing the assertion.
func TestUpdateTunnelPeer_SameKey(t *testing.T) {
	t.Parallel()

	existingKey := makeKey(0x11)

	m := newTestMasterRunner(t)
	injectTunnel(m, &MasterTunnel{
		Name:          "ep-01",
		PeerPublicKey: existingKey,
	})

	applyFnCalled := 0
	m.applyPeerKeyUpdateFn = func(_ *MasterTunnel, _ wg.Key, _ []string) error {
		applyFnCalled++
		return nil
	}

	var keyBytes [32]byte
	copy(keyBytes[:], existingKey[:])

	unchanged, err := m.UpdateTunnelPeer("ep-01", keyBytes, "", nil)

	if err != nil {
		t.Fatalf("expected nil error for same-key, got: %v", err)
	}
	if !unchanged {
		t.Error("expected unchanged=true for same-key call")
	}
	if applyFnCalled != 0 {
		t.Errorf("applyPeerKeyUpdateFn called %d times; want 0 (no UAPI work on same key)", applyFnCalled)
	}

	// Assert in-memory key is still the original.
	m.mu.RLock()
	gotKey := m.tunnels["ep-01"].PeerPublicKey
	m.mu.RUnlock()
	if gotKey != existingKey {
		t.Errorf("tunnel key changed after same-key call: got %v, want %v", gotKey, existingKey)
	}
}

// TestUpdateTunnelPeer_DifferentKey verifies the happy path: when a new key B
// differs from the existing key A, UpdateTunnelPeer calls applyPeerKeyUpdateFn
// (simulating wgctrl success), updates the in-memory state, and returns
// (unchanged=false, nil).
//
// Anti-stub guarantee: replacing the UAPI call with a no-op leaves the tunnel
// key unchanged, causing the final key assertion to fail.
func TestUpdateTunnelPeer_DifferentKey(t *testing.T) {
	t.Parallel()

	oldKey := makeKey(0xAA)
	newKey := makeKey(0xBB)

	m := newTestMasterRunner(t)
	injectTunnel(m, &MasterTunnel{
		Name:          "ep-02",
		PeerPublicKey: oldKey,
	})

	applyFnCalled := 0
	m.applyPeerKeyUpdateFn = func(_ *MasterTunnel, _ wg.Key, _ []string) error {
		applyFnCalled++
		return nil // simulate successful UAPI apply
	}

	var newKeyBytes [32]byte
	copy(newKeyBytes[:], newKey[:])

	unchanged, err := m.UpdateTunnelPeer("ep-02", newKeyBytes, "", nil)

	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if unchanged {
		t.Error("expected unchanged=false for different-key call")
	}
	if applyFnCalled != 1 {
		t.Errorf("applyPeerKeyUpdateFn called %d times, want 1", applyFnCalled)
	}

	// Assert in-memory key was updated to the new key.
	m.mu.RLock()
	gotKey := m.tunnels["ep-02"].PeerPublicKey
	m.mu.RUnlock()
	if gotKey != newKey {
		t.Errorf("tunnel key after update: got %v, want %v", gotKey, newKey)
	}
}

// TestUpdateTunnelPeer_ApplyFails verifies C3 rollback: when applyPeerKeyUpdateFn
// returns an error, UpdateTunnelPeer restores the old key in memory, does NOT
// call saveTransportState (persist), and returns a non-nil error.
//
// Anti-stub guarantee: removing the rollback assignment (`tunnel.PeerPublicKey = oldPubkey`)
// causes the final key assertion to fail — the tunnel would hold the new key.
func TestUpdateTunnelPeer_ApplyFails(t *testing.T) {
	t.Parallel()

	oldKey := makeKey(0xCC)
	newKey := makeKey(0xDD)

	m := newTestMasterRunner(t)
	injectTunnel(m, &MasterTunnel{
		Name:          "ep-03",
		PeerPublicKey: oldKey,
	})

	applyErr := errors.New("wgctrl: device not found")
	m.applyPeerKeyUpdateFn = func(_ *MasterTunnel, _ wg.Key, _ []string) error {
		return applyErr // simulate UAPI failure
	}

	var newKeyBytes [32]byte
	copy(newKeyBytes[:], newKey[:])

	unchanged, err := m.UpdateTunnelPeer("ep-03", newKeyBytes, "", nil)

	if err == nil {
		t.Fatal("expected error from failed apply, got nil")
	}
	if unchanged {
		t.Error("unchanged must be false on UAPI error")
	}

	// Assert in-memory key was rolled back to the old key.
	m.mu.RLock()
	gotKey := m.tunnels["ep-03"].PeerPublicKey
	m.mu.RUnlock()
	if gotKey != oldKey {
		t.Errorf("key not rolled back: got %v, want original %v", gotKey, oldKey)
	}

	// Assert the new key is NOT stored (confirming rollback, not a stale write).
	if gotKey == newKey {
		t.Error("key was NOT rolled back: tunnel holds the new key after UAPI failure")
	}
}
