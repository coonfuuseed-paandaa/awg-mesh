//go:build windows

package adminstate

import (
	"fmt"
	"os"
	"path/filepath"
)

const lockFilename = ".lock"

// acquireLock implements an exclusive per-node lock on Windows using atomic
// file creation (O_CREATE|O_EXCL).  Only one process can create the file at a
// time; if the file already exists another operation is in progress.
//
// The release function closes and removes the lock file so subsequent calls
// can acquire it.  Lock files are never orphaned on process exit because
// Windows automatically removes files opened with O_EXCL when the handle is
// closed (unless the process is hard-killed, in which case the next run
// re-tries and the file is stale-checked).
//
// Note: We intentionally do not use a PID+TTL JSON approach here because the
// admin CLI is not a long-running daemon: individual commands complete in
// seconds and the lock lifespan is bounded by the RPC timeout (30 s).
func acquireLock(nodeDir string) (release func(), err error) {
	lockPath := filepath.Join(nodeDir, lockFilename)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("another admin-state operation is in progress (lock held at %q)", lockPath)
		}
		return nil, fmt.Errorf("acquire admin-state lock %q: %w", lockPath, err)
	}

	return func() {
		_ = f.Close()
		_ = os.Remove(lockPath)
	}, nil
}
