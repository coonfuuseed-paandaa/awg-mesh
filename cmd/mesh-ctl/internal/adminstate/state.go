// Package adminstate provides transactional admin-state pubkey management.
//
// All writes to ~/.mesh-ctl/nodes/<name>/pubkey MUST go through SetPubkey.
// Direct os.WriteFile calls to that path are prohibited in production code.
//
// Design (Option D — "do it right the first time"):
//
//	Admin pubkey file is the single source of truth.
//	Every write is transactional with the gRPC RPC: the callback issues all
//	RPCs first; only on full success is the file updated atomically.
//	On any failure the file is left unchanged so operators can retry safely.
package adminstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	pubkeyFilename = "pubkey"
)

// Store holds paths and synchronization primitives for admin-state access.
//
// mu serialises in-process concurrent calls.  Cross-process mutual exclusion
// is provided by platform-specific lock files (see lock_unix.go / lock_windows.go).
type Store struct {
	configDir string // e.g. ~/.mesh-ctl
	mu        sync.Mutex
}

// NewStore constructs a Store rooted at configDir (typically ~/.mesh-ctl).
func NewStore(configDir string) *Store {
	return &Store{configDir: configDir}
}

// nodePath returns the directory that holds admin state for <node>.
func (s *Store) nodePath(node string) string {
	return filepath.Join(s.configDir, "nodes", node)
}

// GetPubkey reads the current admin pubkey for <node>.
// Returns ("", nil) if the file does not yet exist (fresh/uninitialized node).
// The returned value is exactly as stored — callers must not modify it.
func (s *Store) GetPubkey(node string) (string, error) {
	path := filepath.Join(s.nodePath(node), pubkeyFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read admin pubkey for %q: %w", node, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// TxnCallback is invoked inside the admin-state transaction.
//
// oldKey is the current value from the admin pubkey file (empty string if not
// yet written). The callback MUST issue all required gRPC calls (UpdateTunnelPeer
// on every master, etc.) and return (newKey, nil) only when ALL calls succeed.
//
// Returning an error causes SetPubkey to propagate it without touching the file.
// Returning an empty newKey causes SetPubkey to return an error without writing.
type TxnCallback func(oldKey string) (newKey string, err error)

// SetPubkey transactionally updates the admin pubkey for <node>.
//
// Workflow:
//  1. Acquire per-node in-process mutex and cross-process advisory lock (.lock file).
//  2. Read current admin pubkey (snapshot).
//  3. Invoke cb(snapshot); cb must issue all RPCs that depend on the old key.
//  4. If cb returns error  → unlock, return error, file unchanged.
//  5. If cb returns newKey → write newKey atomically (write to pubkey.tmp,
//     fsync, rename to pubkey).
//  6. Unlock.
//
// The atomic write guarantees that any reader sees either the old or the new
// key — never a partial write.  Cross-process locking ensures two concurrent
// "endpoint init" invocations on the same node serialise correctly.
func (s *Store) SetPubkey(node string, cb TxnCallback) (newKey string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Ensure nodes/<node> directory exists (user-only permissions).
	if mkErr := os.MkdirAll(s.nodePath(node), 0o700); mkErr != nil {
		return "", fmt.Errorf("ensure node dir for %q: %w", node, mkErr)
	}

	// Cross-process advisory lock.
	release, lockErr := acquireLock(s.nodePath(node))
	if lockErr != nil {
		return "", fmt.Errorf("acquire admin-state lock for %q: %w", node, lockErr)
	}
	defer release()

	// Snapshot current admin state.
	oldKey, readErr := s.GetPubkey(node)
	if readErr != nil {
		return "", readErr
	}

	// Invoke callback — RPCs happen here.
	newKey, err = cb(oldKey)
	if err != nil {
		return "", fmt.Errorf(
			"admin-state transaction failed for %q (file unchanged, still %s): %w",
			node, truncKey(oldKey), err,
		)
	}
	if newKey == "" {
		return "", fmt.Errorf(
			"admin-state transaction for %q returned empty pubkey; refusing to write",
			node,
		)
	}

	// Atomic write: tmp → fsync → rename.
	path := filepath.Join(s.nodePath(node), pubkeyFilename)
	tmpPath := path + ".tmp"

	f, openErr := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if openErr != nil {
		return "", fmt.Errorf("open tmp pubkey for %q: %w", node, openErr)
	}

	if _, writeErr := f.WriteString(newKey + "\n"); writeErr != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write tmp pubkey for %q: %w", node, writeErr)
	}
	if syncErr := f.Sync(); syncErr != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("fsync tmp pubkey for %q: %w", node, syncErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close tmp pubkey for %q: %w", node, closeErr)
	}
	if renameErr := os.Rename(tmpPath, path); renameErr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("rename tmp pubkey for %q: %w", node, renameErr)
	}

	return newKey, nil
}

// truncKey returns the first 8 characters of key for use in error messages.
// Avoids leaking the full pubkey into error logs while still being identifiable.
func truncKey(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[:8] + "…"
}
