//go:build !windows

package cmd

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// acquireFileLock obtains an exclusive advisory flock on path.
// Returns a release function (unlock+close) and any error.
// On Unix systems this uses syscall.Flock; on Windows the build-tag
// routes to reconcile_lock_windows.go which provides a no-op fallback.
func acquireFileLock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %q: %w", path, err)
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire lock %q: another reconcile is running (LOCK_EX failed): %w", path, err)
	}

	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}
