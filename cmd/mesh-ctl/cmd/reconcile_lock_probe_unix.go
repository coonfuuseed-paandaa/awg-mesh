//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

// pidSignalZero sends signal 0 to the process, which checks existence without
// delivering an actual signal. Returns nil if the process is alive.
func pidSignalZero(proc *os.Process) error {
	return proc.Signal(syscall.Signal(0))
}
