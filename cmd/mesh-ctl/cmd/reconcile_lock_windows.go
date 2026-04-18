//go:build windows

package cmd

import (
	"fmt"
	"os"
)

// acquireFileLock implements an exclusive process-level lock on Windows using
// atomic file creation (O_CREATE|O_EXCL). If the lock file already exists,
// another reconcile is in progress and the call returns an error immediately
// without blocking. The release function closes and removes the lock file.
//
// This avoids a dependency on golang.org/x/sys/windows LockFileEx while still
// providing real mutual exclusion: O_EXCL is honoured by the Windows kernel and
// guarantees that only one process succeeds in creating the file.
func acquireFileLock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("another reconcile is in progress (lock held at %q)", path)
		}
		return nil, fmt.Errorf("acquire lock file %q: %w", path, err)
	}
	return func() {
		_ = f.Close()
		_ = os.Remove(path)
	}, nil
}
