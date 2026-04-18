package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeLockRecord writes a reconcileLockRecord JSON to path directly,
// bypassing the normal tryCreate path so tests can inject arbitrary records.
func writeLockRecord(t *testing.T, path string, rec reconcileLockRecord) {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal lock record: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write lock record to %s: %v", path, err)
	}
}

// TestAcquireFileLock_HappyPath: first acquire succeeds; second acquire on the
// same path fails with a "another reconcile is in progress" message; release
// removes the file so a third acquire succeeds.
//
// Anti-stub: a no-op acquireFileLock that returns (func(){},nil) always makes
// the "second acquire fails" assertion produce nil and the test fails.
func TestAcquireFileLock_HappyPath(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), "reconcile.lock")

	release1, err := acquireFileLock(lockPath)
	if err != nil {
		t.Fatalf("first acquire: unexpected error: %v", err)
	}

	_, err2 := acquireFileLock(lockPath)
	if err2 == nil {
		t.Fatal("second acquire: expected error (lock already held), got nil")
	}
	if !strings.Contains(err2.Error(), "another reconcile is in progress") {
		t.Errorf("second acquire error should mention 'another reconcile is in progress', got: %v", err2)
	}

	release1()

	// After release, the lock file must be gone.
	if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
		t.Error("lock file should be removed after release, but it still exists")
	}

	// Third acquire must now succeed.
	release3, err3 := acquireFileLock(lockPath)
	if err3 != nil {
		t.Fatalf("third acquire (after release): unexpected error: %v", err3)
	}
	release3()
}

// TestAcquireFileLock_StaleByTTL: a lock file with started_at older than TTL
// is automatically reclaimed without operator intervention.
//
// Anti-stub: if TTL check is missing, the reclaim never happens and the
// acquire returns an error instead of succeeding.
func TestAcquireFileLock_StaleByTTL(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), "reconcile.lock")

	hostname, _ := os.Hostname()
	stale := reconcileLockRecord{
		PID:       os.Getpid(), // same PID — liveness check would say "alive"
		Hostname:  hostname,
		StartedAt: time.Now().Add(-(reconcileLockTTL + time.Minute)), // expired
		TTLNS:     int64(reconcileLockTTL),
	}
	writeLockRecord(t, lockPath, stale)

	release, err := acquireFileLock(lockPath)
	if err != nil {
		t.Fatalf("acquire with stale-TTL lock: expected success, got: %v", err)
	}
	release()
}

// TestAcquireFileLock_DeadPID: a lock file referencing a PID that no longer
// exists is reclaimed automatically on Unix. On Windows the TTL path handles
// reclaim so we skip the PID-dead sub-case there.
//
// We use PID 0 as the sentinel for "dead process" on Unix (kill(0, 0) returns
// EPERM for process 0 on Linux, indicating we are not the sender — effectively
// confirming the PID is not a regular userspace process that we own). Simpler
// and portable: we use a PID that is guaranteed non-existent by writing
// math.MaxInt32 / 2 = 1073741823, which Linux limits at PID_MAX_DEFAULT=32768.
func TestAcquireFileLock_DeadPID(t *testing.T) {
	t.Parallel()

	if isWindows() {
		t.Skip("Windows uses TTL-based reclaim; PID probe not supported without CGO")
	}

	lockPath := filepath.Join(t.TempDir(), "reconcile.lock")
	hostname, _ := os.Hostname()

	// PID 99999 is almost certainly not running on any test host.
	deadPID := 99999
	rec := reconcileLockRecord{
		PID:       deadPID,
		Hostname:  hostname,
		StartedAt: time.Now(),
		TTLNS:     int64(reconcileLockTTL),
	}
	writeLockRecord(t, lockPath, rec)

	release, err := acquireFileLock(lockPath)
	if err != nil {
		t.Fatalf("acquire with dead-PID lock: expected success, got: %v", err)
	}
	release()
}

// TestAcquireFileLock_CrossMachine: a lock file from a different hostname
// reports a cross-machine conflict error and is NOT automatically removed.
//
// Anti-stub: if the hostname check is omitted, the function either reclaims
// the lock silently or returns a generic error without mentioning the remote host.
func TestAcquireFileLock_CrossMachine(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), "reconcile.lock")

	rec := reconcileLockRecord{
		PID:       12345,
		Hostname:  "some-other-machine.internal",
		StartedAt: time.Now(),
		TTLNS:     int64(reconcileLockTTL),
	}
	writeLockRecord(t, lockPath, rec)

	_, err := acquireFileLock(lockPath)
	if err == nil {
		t.Fatal("expected cross-machine conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "cross-machine conflict") {
		t.Errorf("error should mention 'cross-machine conflict', got: %v", err)
	}
	if !strings.Contains(err.Error(), "some-other-machine.internal") {
		t.Errorf("error should mention the remote hostname, got: %v", err)
	}
	if !strings.Contains(err.Error(), "force-unlock") {
		t.Errorf("error should suggest --force-unlock, got: %v", err)
	}
}

// TestAcquireFileLock_NonExistentDirectory: attempting to create a lock file
// in a non-existent directory returns an error immediately.
func TestAcquireFileLock_NonExistentDirectory(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), "nonexistent", "reconcile.lock")

	_, err := acquireFileLock(lockPath)
	if err == nil {
		t.Fatal("expected error for non-existent parent directory, got nil")
	}
}

// TestAcquireFileLock_ReleaseIsIdempotent: calling release twice must not
// panic or return an error (e.g. from a double os.Remove).
func TestAcquireFileLock_ReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), "reconcile.lock")

	release, err := acquireFileLock(lockPath)
	if err != nil {
		t.Fatalf("acquire: unexpected error: %v", err)
	}

	release()
	release() // second call must not panic
}

// TestLockRecord_WrittenWithCurrentPID verifies that a freshly acquired lock
// contains the calling process's PID, current hostname, and a non-zero started_at.
//
// Anti-stub: if tryCreate always writes PID=0 or hostname="", these assertions fail.
func TestLockRecord_WrittenWithCurrentPID(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), "reconcile.lock")

	release, err := acquireFileLock(lockPath)
	if err != nil {
		t.Fatalf("acquire: unexpected error: %v", err)
	}
	defer release()

	rec, readErr := readLockRecord(lockPath)
	if readErr != nil {
		t.Fatalf("readLockRecord: %v", readErr)
	}

	if rec.PID != os.Getpid() {
		t.Errorf("lock PID: got %d, want %d", rec.PID, os.Getpid())
	}

	hostname, _ := os.Hostname()
	if rec.Hostname != hostname {
		t.Errorf("lock hostname: got %q, want %q", rec.Hostname, hostname)
	}

	if rec.StartedAt.IsZero() {
		t.Error("lock started_at must not be zero")
	}

	if rec.TTLNS != int64(reconcileLockTTL) {
		t.Errorf("lock ttl_ns: got %d, want %d", rec.TTLNS, int64(reconcileLockTTL))
	}
}

// isWindows reports whether we are running on Windows. Used to skip
// PID-probe tests that require Unix signal semantics.
func isWindows() bool {
	return os.Getenv("GOOS") == "windows" || isWindowsRuntime()
}
