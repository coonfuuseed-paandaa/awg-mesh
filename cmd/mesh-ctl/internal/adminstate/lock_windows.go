//go:build windows

package adminstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const lockFilename = ".lock"

// adminLockTTL is the conservative upper bound for how long a single
// admin-state operation (endpoint init, key rotation) should take.
// Individual commands complete within the gRPC timeout (~30 s); 5 minutes
// provides a generous safety margin before treating a lock as stale.
const adminLockTTL = 5 * time.Minute

// adminLockRecord is the JSON payload written into the per-node lock file.
type adminLockRecord struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	TTLNS     int64     `json:"ttl_ns"`
}

// acquireLock implements an exclusive per-node lock on Windows.
//
// Strategy:
//  1. Attempt atomic O_CREATE|O_EXCL create → write PID + TTL metadata → done.
//  2. On EEXIST: read + parse JSON metadata from the existing lock file.
//  3. If started_at + ttl_ns < now → stale by TTL → delete and re-acquire.
//  4. Otherwise → report conflict with PID and start time for diagnosis.
//
// On hard kill the lock file is left on disk. The next run reclaims it via
// TTL expiry (adminLockTTL = 5 min), so operations are never permanently
// stuck. Operators can also delete the lock file manually if needed.
//
// Note: reliable PID-liveness probing on Windows requires CGO or
// golang.org/x/sys/windows.OpenProcess, neither of which we want as a
// dependency here.  TTL expiry is the sole reclaim mechanism.
func acquireLock(nodeDir string) (release func(), err error) {
	lockPath := filepath.Join(nodeDir, lockFilename)

	// --- Attempt 1: atomic create ---
	if rel, ok := tryAdminLockCreate(lockPath); ok {
		return rel, nil
	}

	// --- Attempt 2: parse existing lock ---
	rec, readErr := readAdminLockRecord(lockPath)
	if readErr != nil {
		// File may have vanished between our failed O_EXCL and the read.
		// Retry once; surface the error if it fails again.
		if rel, ok := tryAdminLockCreate(lockPath); ok {
			return rel, nil
		}
		return nil, fmt.Errorf("acquire admin-state lock %q: read existing lock: %w", lockPath, readErr)
	}

	// --- Check TTL expiry ---
	ttl := time.Duration(rec.TTLNS)
	if ttl <= 0 {
		ttl = adminLockTTL
	}
	if time.Since(rec.StartedAt) > ttl {
		// Stale by TTL — safe to reclaim.
		if removeErr := os.Remove(lockPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return nil, fmt.Errorf("acquire admin-state lock %q: remove stale lock: %w", lockPath, removeErr)
		}
		if rel, ok := tryAdminLockCreate(lockPath); ok {
			return rel, nil
		}
		return nil, fmt.Errorf("acquire admin-state lock %q: could not acquire after removing stale TTL lock", lockPath)
	}

	// --- Live conflict ---
	return nil, fmt.Errorf(
		"another admin-state operation is in progress (lock held at %q, PID %d, started %s); "+
			"if that process crashed, delete the lock file manually or wait %s for TTL expiry",
		lockPath, rec.PID, rec.StartedAt.Format(time.RFC3339), ttl,
	)
}

// tryAdminLockCreate attempts atomic O_CREATE|O_EXCL creation of the lock file,
// writes PID + TTL metadata as JSON, and returns (release, true) on success.
// Returns (nil, false) if the file already exists.
func tryAdminLockCreate(lockPath string) (release func(), ok bool) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, false
	}
	rec := adminLockRecord{
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC(),
		TTLNS:     int64(adminLockTTL),
	}
	if encErr := json.NewEncoder(f).Encode(rec); encErr != nil {
		_ = f.Close()
		_ = os.Remove(lockPath) // best-effort cleanup of partial write
		return nil, false
	}
	_ = f.Close()

	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = os.Remove(lockPath)
	}, true
}

// readAdminLockRecord parses the JSON admin lock record from path.
func readAdminLockRecord(path string) (adminLockRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return adminLockRecord{}, err
	}
	var rec adminLockRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return adminLockRecord{}, fmt.Errorf("parse lock JSON: %w", err)
	}
	return rec, nil
}
