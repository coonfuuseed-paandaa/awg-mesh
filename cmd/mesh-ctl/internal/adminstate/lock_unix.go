//go:build !windows

package adminstate

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const lockFilename = ".lock"

// acquireLock obtains an exclusive POSIX advisory flock on the per-node .lock
// file inside nodeDir.  Returns a release function (unlock+close) and any
// error.  Uses LOCK_EX|LOCK_NB so it fails immediately rather than blocking
// if another process holds the lock.
func acquireLock(nodeDir string) (release func(), err error) {
	lockPath := filepath.Join(nodeDir, lockFilename)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open admin-state lock file %q: %w", lockPath, err)
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %q: another operation is in progress (LOCK_EX failed): %w", lockPath, err)
	}

	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}
