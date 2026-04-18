//go:build windows

package cmd

import (
	"fmt"
	"os"
)

// acquireFileLock is a no-op advisory lock on Windows. Windows does not
// support fcntl-style file locking for exclusion between processes using the
// standard library without cgo; LockFileEx is available but requires unsafe
// code not warranted here. A warning is printed to stderr so the operator
// knows concurrent reconcile runs are not serialized.
func acquireFileLock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %q: %w", path, err)
	}
	fmt.Fprintln(os.Stderr, "warning: file locking is a no-op on Windows — concurrent 'reconcile' runs are not serialized")
	return func() { _ = f.Close() }, nil
}
