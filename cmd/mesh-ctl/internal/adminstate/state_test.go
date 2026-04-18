package adminstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// newTestStore creates a Store backed by a temporary directory and returns it
// alongside the config-dir path for direct file inspection.
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	return NewStore(dir), dir
}

// nodePubkeyPath returns the path to the pubkey file for a node (for test assertions).
func nodePubkeyPath(cfgDir, node string) string {
	return filepath.Join(cfgDir, "nodes", node, pubkeyFilename)
}

// TestGetPubkey_NotExist verifies that GetPubkey returns ("", nil) for a fresh node.
func TestGetPubkey_NotExist(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	got, err := store.GetPubkey("ep-01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

// TestSetPubkey_Happy verifies the normal path: callback succeeds → file written.
func TestSetPubkey_Happy(t *testing.T) {
	t.Parallel()
	store, cfgDir := newTestStore(t)

	const wantKey = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	newKey, err := store.SetPubkey("ep-01", func(oldKey string) (string, error) {
		if oldKey != "" {
			t.Errorf("expected empty oldKey on fresh node, got %q", oldKey)
		}
		return wantKey, nil
	})
	if err != nil {
		t.Fatalf("SetPubkey error: %v", err)
	}
	if newKey != wantKey {
		t.Errorf("returned key mismatch: got %q, want %q", newKey, wantKey)
	}

	// Verify file on disk.
	data, readErr := os.ReadFile(nodePubkeyPath(cfgDir, "ep-01"))
	if readErr != nil {
		t.Fatalf("read pubkey file: %v", readErr)
	}
	if strings.TrimSpace(string(data)) != wantKey {
		t.Errorf("disk content mismatch: got %q, want %q", strings.TrimSpace(string(data)), wantKey)
	}

	// GetPubkey should return the new value.
	got, err := store.GetPubkey("ep-01")
	if err != nil {
		t.Fatalf("GetPubkey error: %v", err)
	}
	if got != wantKey {
		t.Errorf("GetPubkey mismatch: got %q, want %q", got, wantKey)
	}
}

// TestSetPubkey_RollbackOnError verifies that a callback error leaves the file unchanged.
func TestSetPubkey_RollbackOnError(t *testing.T) {
	t.Parallel()
	store, cfgDir := newTestStore(t)

	const initialKey = "1111111111111111111111111111111111111111111111111111111111111111"

	// Pre-populate the file with an initial key.
	nodeDir := filepath.Join(cfgDir, "nodes", "ep-01")
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, pubkeyFilename), []byte(initialKey+"\n"), 0o600); err != nil {
		t.Fatalf("write initial key: %v", err)
	}

	rpcErr := errors.New("RPC failed: master unreachable")
	_, err := store.SetPubkey("ep-01", func(oldKey string) (string, error) {
		if oldKey != initialKey {
			t.Errorf("expected oldKey=%q, got %q", initialKey, oldKey)
		}
		return "", rpcErr
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), rpcErr.Error()) {
		t.Errorf("error %q does not contain %q", err.Error(), rpcErr.Error())
	}

	// File must still contain the initial key.
	data, readErr := os.ReadFile(nodePubkeyPath(cfgDir, "ep-01"))
	if readErr != nil {
		t.Fatalf("read pubkey file: %v", readErr)
	}
	if strings.TrimSpace(string(data)) != initialKey {
		t.Errorf("file mutated on rollback: got %q, want %q", strings.TrimSpace(string(data)), initialKey)
	}
}

// TestSetPubkey_EmptyNewKey verifies that an empty newKey from the callback is rejected.
func TestSetPubkey_EmptyNewKey(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	_, err := store.SetPubkey("ep-01", func(_ string) (string, error) {
		return "", nil // empty newKey
	})
	if err == nil {
		t.Fatal("expected error for empty newKey, got nil")
	}
	if !strings.Contains(err.Error(), "empty pubkey") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestSetPubkey_CrashSimulation verifies crash-safety: if the tmp file exists from
// a previous crashed run, the next SetPubkey succeeds without error.
func TestSetPubkey_CrashSimulation(t *testing.T) {
	t.Parallel()
	store, cfgDir := newTestStore(t)

	const oldKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const newKey = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// Simulate pre-existing old key.
	nodeDir := filepath.Join(cfgDir, "nodes", "ep-01")
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, pubkeyFilename), []byte(oldKey+"\n"), 0o600); err != nil {
		t.Fatalf("write initial key: %v", err)
	}

	// Simulate a stale .tmp file left by a previous crash.
	tmpPath := filepath.Join(nodeDir, pubkeyFilename+".tmp")
	if err := os.WriteFile(tmpPath, []byte("stale content"), 0o600); err != nil {
		t.Fatalf("write stale tmp: %v", err)
	}

	// SetPubkey should overwrite the .tmp and rename successfully.
	got, err := store.SetPubkey("ep-01", func(curKey string) (string, error) {
		if curKey != oldKey {
			t.Errorf("expected oldKey=%q, got %q", oldKey, curKey)
		}
		return newKey, nil
	})
	if err != nil {
		t.Fatalf("SetPubkey after crash-sim: %v", err)
	}
	if got != newKey {
		t.Errorf("returned key: got %q, want %q", got, newKey)
	}

	// Verify the tmp file is gone.
	if _, statErr := os.Stat(tmpPath); !os.IsNotExist(statErr) {
		t.Error("stale .tmp file still exists after successful SetPubkey")
	}

	// Verify the final file has the new key.
	data, readErr := os.ReadFile(nodePubkeyPath(cfgDir, "ep-01"))
	if readErr != nil {
		t.Fatalf("read final pubkey: %v", readErr)
	}
	if strings.TrimSpace(string(data)) != newKey {
		t.Errorf("final key mismatch: got %q, want %q", strings.TrimSpace(string(data)), newKey)
	}
}

// TestSetPubkey_SecondWriteSeesNewKey verifies that a second SetPubkey sees the
// updated key from the first call.
func TestSetPubkey_SecondWriteSeesNewKey(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	const key1 = "1111111111111111111111111111111111111111111111111111111111111111"
	const key2 = "2222222222222222222222222222222222222222222222222222222222222222"

	if _, err := store.SetPubkey("ep-01", func(_ string) (string, error) {
		return key1, nil
	}); err != nil {
		t.Fatalf("first SetPubkey: %v", err)
	}

	var seenOldKey string
	if _, err := store.SetPubkey("ep-01", func(old string) (string, error) {
		seenOldKey = old
		return key2, nil
	}); err != nil {
		t.Fatalf("second SetPubkey: %v", err)
	}

	if seenOldKey != key1 {
		t.Errorf("second call saw oldKey=%q, want %q", seenOldKey, key1)
	}
}

// TestSetPubkey_Concurrent verifies that concurrent goroutines calling SetPubkey
// for the same node serialise correctly: each goroutine sees exactly one
// consistent old key and writes exactly one new key without corruption.
//
// Serialisation is actively verified via an atomic concurrency counter: the
// counter is incremented at the start of each callback and decremented at the
// end. If two callbacks ever execute simultaneously the peak count exceeds 1,
// which is a hard failure because it proves the critical section was entered
// concurrently — i.e. the mutex or the lock-file did not serialise correctly.
func TestSetPubkey_Concurrent(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	const goroutines = 8
	const baseKey = "00000000000000000000000000000000000000000000000000000000000000%02d"

	var (
		wg          sync.WaitGroup
		errs        = make([]error, goroutines)
		inCallback  atomic.Int32 // how many callbacks are executing right now
		maxConcurrent atomic.Int32 // peak observed concurrency
	)

	for i := 0; i < goroutines; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf(baseKey, i)
			_, errs[i] = store.SetPubkey("ep-concurrent", func(_ string) (string, error) {
				// Enter critical section: record peak concurrency.
				cur := inCallback.Add(1)
				for {
					old := maxConcurrent.Load()
					if cur <= old || maxConcurrent.CompareAndSwap(old, cur) {
						break
					}
				}
				defer inCallback.Add(-1)
				return key, nil
			})
		}()
	}
	wg.Wait()

	// All goroutines should have succeeded.
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d failed: %v", i, err)
		}
	}

	// Serialisation assertion: no two callbacks may have run at the same time.
	if peak := maxConcurrent.Load(); peak > 1 {
		t.Errorf("serialisation violated: %d callbacks executed concurrently (want max 1)", peak)
	}

	// The final key on disk should be one of the valid keys (not corrupted).
	got, err := store.GetPubkey("ep-concurrent")
	if err != nil {
		t.Fatalf("GetPubkey after concurrent: %v", err)
	}
	if !strings.HasPrefix(got, "00000000") {
		t.Errorf("final key looks corrupted: %q", got)
	}
}

// TestTruncKey verifies the truncKey helper produces expected output.
func TestTruncKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"abcd", "abcd"},
		{"abcdefgh", "abcdefgh"},
		{"abcdefghi", "abcdefgh…"},
		{"aabbccddeeff00112233445566778899", "aabbccdd…"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got := truncKey(tt.input)
			if got != tt.want {
				t.Errorf("truncKey(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
