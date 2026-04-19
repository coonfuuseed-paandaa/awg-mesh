package node

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

// newTestEndpointRunner creates a minimal EndpointRunner backed by a temp dir.
func newTestEndpointRunner(t *testing.T) (*EndpointRunner, string) {
	t.Helper()
	dir := t.TempDir()
	node := &Node{config: NodeConfig{ConfigDir: dir}}
	return &EndpointRunner{node: node}, dir
}

// TestPersistKeypair_Happy saves a keypair and loads it back, verifying the
// public key is correctly derived from the private key.
func TestPersistKeypair_Happy(t *testing.T) {
	t.Parallel()

	runner, dir := newTestEndpointRunner(t)

	privKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}

	if err := runner.PersistKeypair("tunnel-a", privKey[:]); err != nil {
		t.Fatalf("PersistKeypair: %v", err)
	}

	loaded, err := runner.LoadKeypair("tunnel-a")
	if err != nil {
		t.Fatalf("LoadKeypair: %v", err)
	}

	if len(loaded) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(loaded))
	}

	var roundtrip wg.Key
	copy(roundtrip[:], loaded)
	if roundtrip != privKey {
		t.Fatalf("private key mismatch after roundtrip")
	}

	// Verify public key is correctly derived and persisted.
	state, err := LoadNodeState(dir)
	if err != nil {
		t.Fatalf("LoadNodeState: %v", err)
	}
	expectedPub := privKey.PublicKey().String()
	if state.PublicKey != expectedPub {
		t.Fatalf("public key mismatch: got %q want %q", state.PublicKey, expectedPub)
	}
}

// TestPersistKeypair_MissingFile verifies that PersistKeypair synthesizes fresh
// state when node.yml does not exist (os.ErrNotExist path).
func TestPersistKeypair_MissingFile(t *testing.T) {
	t.Parallel()

	runner, dir := newTestEndpointRunner(t)

	// Confirm node.yml does not exist.
	statePath := filepath.Join(dir, nodeStateFileName)
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("expected node.yml to be absent, got: %v", err)
	}

	privKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}

	// Should succeed — synthesizes fresh state on os.ErrNotExist.
	if err := runner.PersistKeypair("tunnel-a", privKey[:]); err != nil {
		t.Fatalf("PersistKeypair on missing file: %v", err)
	}

	// Verify file was created.
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("node.yml not created: %v", err)
	}

	loaded, err := runner.LoadKeypair("tunnel-a")
	if err != nil {
		t.Fatalf("LoadKeypair after persist: %v", err)
	}

	var roundtrip wg.Key
	copy(roundtrip[:], loaded)
	if roundtrip != privKey {
		t.Fatalf("private key mismatch after synthesized persist")
	}
}

// TestPersistKeypair_CorruptFile verifies that PersistKeypair propagates the
// error and does NOT write when node.yml contains corrupt YAML (NFR-6).
func TestPersistKeypair_CorruptFile(t *testing.T) {
	t.Parallel()

	runner, dir := newTestEndpointRunner(t)

	// Write corrupt content.
	statePath := filepath.Join(dir, nodeStateFileName)
	if err := os.WriteFile(statePath, []byte("private_key: [\x00CORRUPT"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	privKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}

	err = runner.PersistKeypair("tunnel-a", privKey[:])
	if err == nil {
		t.Fatal("expected error for corrupt node.yml, got nil")
	}

	// The error must NOT be os.ErrNotExist — it must be a real error.
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt-file error should not be os.ErrNotExist, got: %v", err)
	}

	// Verify file was not overwritten (still contains corrupt content).
	data, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("ReadFile after failed persist: %v", readErr)
	}
	if string(data) != "private_key: [\x00CORRUPT" {
		t.Fatalf("corrupt file was overwritten (should not be): %q", string(data))
	}
}

// TestLoadKeypair_NotExist verifies that LoadKeypair returns os.ErrNotExist
// when node.yml is absent.
func TestLoadKeypair_NotExist(t *testing.T) {
	t.Parallel()

	runner, _ := newTestEndpointRunner(t)

	_, err := runner.LoadKeypair("tunnel-a")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got: %v", err)
	}
}

// TestLockRotation_Serializes verifies that LockRotation serializes concurrent
// callers. Two goroutines compete; both must complete without data race.
func TestLockRotation_Serializes(t *testing.T) {
	t.Parallel()

	runner, _ := newTestEndpointRunner(t)

	results := make(chan int, 2)

	acquire := func(id int) {
		unlock, err := runner.LockRotation("tunnel-a")
		if err != nil {
			t.Errorf("LockRotation goroutine %d: %v", id, err)
			return
		}
		defer unlock()
		results <- id
	}

	go acquire(1)
	go acquire(2)

	// Both must complete successfully.
	r1 := <-results
	r2 := <-results
	if r1 == r2 {
		t.Fatalf("expected distinct goroutine IDs, got %d and %d", r1, r2)
	}
}

// TestPersistKeypair_PreservesExistingFields verifies that PersistKeypair only
// updates PrivateKey and PublicKey, preserving Name, Mode, and OverlayIP.
func TestPersistKeypair_PreservesExistingFields(t *testing.T) {
	t.Parallel()

	runner, _ := newTestEndpointRunner(t)

	// Write initial state with all fields populated.
	initialPriv, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}
	initial := NodeState{
		PrivateKey: initialPriv.String(),
		PublicKey:  initialPriv.PublicKey().String(),
		Name:       "my-endpoint",
		Mode:       "endpoint",
		OverlayIP:  "10.0.0.5",
	}
	if err := SaveNodeState(runner.node.config.ConfigDir, initial); err != nil {
		t.Fatalf("SaveNodeState: %v", err)
	}

	// Persist a new keypair.
	newPriv, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}
	if err := runner.PersistKeypair("tunnel-a", newPriv[:]); err != nil {
		t.Fatalf("PersistKeypair: %v", err)
	}

	// Verify keys updated, other fields preserved.
	state, err := LoadNodeState(runner.node.config.ConfigDir)
	if err != nil {
		t.Fatalf("LoadNodeState: %v", err)
	}
	if state.PrivateKey != newPriv.String() {
		t.Fatalf("PrivateKey not updated: got %q want %q", state.PrivateKey, newPriv.String())
	}
	if state.PublicKey != newPriv.PublicKey().String() {
		t.Fatalf("PublicKey not updated: got %q want %q", state.PublicKey, newPriv.PublicKey().String())
	}
	if state.Name != "my-endpoint" {
		t.Fatalf("Name was modified: got %q", state.Name)
	}
	if state.Mode != "endpoint" {
		t.Fatalf("Mode was modified: got %q", state.Mode)
	}
	if state.OverlayIP != "10.0.0.5" {
		t.Fatalf("OverlayIP was modified: got %q", state.OverlayIP)
	}
}
