// Package cmd — cross-platform reconcile lock with PID + TTL + hostname.
//
// Replaces the previous platform-split reconcile_lock_unix.go /
// reconcile_lock_windows.go pair (which had no stale-lock reclaim on Windows).
//
// Lock format: JSON file at <configDir>/reconcile.lock.
//
//	{
//	  "pid":        1234,
//	  "hostname":   "admin-pc",
//	  "started_at": "2026-04-19T01:00:00Z",
//	  "ttl_ns":     3600000000000
//	}
//
// Acquisition logic (in order):
//  1. O_CREATE|O_EXCL|O_WRONLY → success: write own metadata, done.
//  2. EEXIST: read + parse JSON.
//  3. started_at + ttl_ns < now → stale: delete and re-acquire.
//  4. hostname differs → cross-machine conflict: fail.
//  5. PID not alive (signal-0 probe) → stale process: delete and re-acquire.
//  6. Otherwise → live conflict: fail with informative message.
//
// Release: delete lock file (deferred in caller for crash safety).

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"
)

// reconcileLockTTL is the default maximum lifetime of a reconcile lock.
// If a lock is older than this it is assumed stale and reclaimed automatically.
const reconcileLockTTL = time.Hour

// reconcileLockRecord is the JSON body written into the lock file.
type reconcileLockRecord struct {
	PID       int       `json:"pid"`
	Hostname  string    `json:"hostname"`
	StartedAt time.Time `json:"started_at"`
	TTLNS     int64     `json:"ttl_ns"`
}

// acquireFileLock tries to acquire an exclusive reconcile lock at path.
// Returns a release function (removes the lock file) and any error.
//
// The release function is idempotent and safe to call from a defer.
func acquireFileLock(path string) (release func(), err error) {
	hostname, _ := os.Hostname()

	// --- Attempt 1: atomic create ---
	if rel, ok := tryCreate(path, hostname); ok {
		return rel, nil
	}

	// --- Attempt 2: read existing lock ---
	rec, readErr := readLockRecord(path)
	if readErr != nil {
		// File disappeared between our failed O_EXCL and now, or is corrupt.
		// Try once more; if it fails again, surface the error.
		if rel, ok := tryCreate(path, hostname); ok {
			return rel, nil
		}
		return nil, fmt.Errorf("reconcile lock: read existing lock at %q: %w", path, readErr)
	}

	// --- Check TTL expiry ---
	ttl := time.Duration(rec.TTLNS)
	if ttl <= 0 {
		ttl = reconcileLockTTL
	}
	if time.Since(rec.StartedAt) > ttl {
		// Stale by TTL — safe to reclaim.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("reconcile lock: remove stale lock: %w", err)
		}
		if rel, ok := tryCreate(path, hostname); ok {
			return rel, nil
		}
		return nil, fmt.Errorf("reconcile lock: could not acquire after removing stale TTL lock at %q", path)
	}

	// --- Check hostname ---
	if rec.Hostname != hostname {
		return nil, fmt.Errorf(
			"reconcile lock: cross-machine conflict — lock held by hostname %q (PID %d, started %s); "+
				"if that host crashed, delete %q manually or run: mesh-ctl reconcile --force-unlock",
			rec.Hostname, rec.PID, rec.StartedAt.Format(time.RFC3339), path,
		)
	}

	// --- Check PID alive ---
	if !isPIDAlive(rec.PID) {
		// Process is gone — safe to reclaim.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("reconcile lock: remove dead-PID lock: %w", err)
		}
		if rel, ok := tryCreate(path, hostname); ok {
			return rel, nil
		}
		return nil, fmt.Errorf("reconcile lock: could not acquire after removing dead-PID lock at %q", path)
	}

	// --- Live conflict ---
	return nil, fmt.Errorf(
		"reconcile lock: another reconcile is in progress (PID %d, hostname %q, started %s); "+
			"wait for it to finish or run: mesh-ctl reconcile --force-unlock",
		rec.PID, rec.Hostname, rec.StartedAt.Format(time.RFC3339),
	)
}

// tryCreate attempts an atomic O_CREATE|O_EXCL create of the lock file.
// Returns (release, true) on success, (nil, false) if the file already exists.
func tryCreate(path, hostname string) (release func(), ok bool) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, false
	}
	rec := reconcileLockRecord{
		PID:       os.Getpid(),
		Hostname:  hostname,
		StartedAt: time.Now().UTC(),
		TTLNS:     int64(reconcileLockTTL),
	}
	if encErr := json.NewEncoder(f).Encode(rec); encErr != nil {
		_ = f.Close()
		_ = os.Remove(path) // best-effort cleanup of partially-written lock
		return nil, false
	}
	_ = f.Close()

	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = os.Remove(path)
	}, true
}

// readLockRecord parses the JSON lock record from path.
func readLockRecord(path string) (reconcileLockRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return reconcileLockRecord{}, err
	}
	var rec reconcileLockRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return reconcileLockRecord{}, fmt.Errorf("parse lock JSON: %w", err)
	}
	return rec, nil
}

// isPIDAlive returns true if the process with the given PID is still running
// on the current host.
//
// On Unix: uses os.FindProcess (always succeeds) then sends signal 0, which
// checks existence without delivering a signal.
// On Windows: os.FindProcess opens a PROCESS_ALL_ACCESS handle; a non-nil
// process with no error indicates the process exists. We additionally call
// os.FindProcess and check the error because Windows FindProcess does NOT
// return an error for dead PIDs — so we probe with a zero-signal equivalent
// by attempting to open the process and checking whether the handle is valid.
//
// Note: on Windows, os.FindProcess always returns (non-nil, nil) regardless of
// whether the PID is alive (it just creates a handle struct). The reliable
// Windows approach is to call OpenProcess and check the error. However, since
// golang.org/x/sys/windows is already an indirect dep, we avoid adding a new
// direct import and instead use a runtime GOOS guard: on Windows we conservatively
// return true (treat as alive) so the lock is never silently stolen on Windows,
// and users can always --force-unlock instead. The TTL path handles eventual
// reclaim.
func isPIDAlive(pid int) bool {
	if runtime.GOOS == "windows" {
		// On Windows, we cannot safely probe PID liveness without CGO or
		// x/sys/windows.OpenProcess. Conservatively report the PID as alive;
		// TTL expiry (1 hour) handles eventual stale-lock reclaim.
		// Operators can always run --force-unlock for immediate recovery.
		return true
	}
	// Unix: signal(pid, 0) — no signal delivered, just existence check.
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return pidSignalZero(proc) == nil
}
