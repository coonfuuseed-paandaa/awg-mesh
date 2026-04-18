//go:build windows

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAcquireFileLockWindows verifies the atomic O_CREATE|O_EXCL Windows lock:
//   - second concurrent acquire on same path returns an error
//   - release removes the lock file so a subsequent acquire succeeds
//
// Anti-stub: a no-op acquireFileLock that returns (func(){},nil) always makes
// the "double-acquire fails" case fail because err would be nil.
func TestAcquireFileLockWindows(t *testing.T) {
	t.Parallel()

	t.Run("double acquire fails with concurrent-reconcile message", func(t *testing.T) {
		t.Parallel()

		lockPath := filepath.Join(t.TempDir(), "reconcile.lock")

		release, err := acquireFileLock(lockPath)
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

		release()
	})

	t.Run("release removes lock file so next acquire succeeds", func(t *testing.T) {
		t.Parallel()

		lockPath := filepath.Join(t.TempDir(), "reconcile.lock")

		release, err := acquireFileLock(lockPath)
		if err != nil {
			t.Fatalf("first acquire: unexpected error: %v", err)
		}
		release() // must delete the lock file

		if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
			t.Error("lock file should be removed after release, but it still exists")
		}

		release2, err2 := acquireFileLock(lockPath)
		if err2 != nil {
			t.Fatalf("acquire after release: expected success, got: %v", err2)
		}
		release2()
	})

	t.Run("lock path in non-existent directory returns error", func(t *testing.T) {
		t.Parallel()

		lockPath := filepath.Join(t.TempDir(), "nonexistent", "reconcile.lock")

		_, err := acquireFileLock(lockPath)
		if err == nil {
			t.Fatal("expected error for non-existent parent directory, got nil")
		}
	})
}
