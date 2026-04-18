//go:build windows

package cmd

import "os"

// pidSignalZero on Windows: signal(0) is not supported.
// isPIDAlive handles the Windows case separately (returns true conservatively),
// so this stub is only here to satisfy the compiler.
func pidSignalZero(_ *os.Process) error {
	return nil
}
