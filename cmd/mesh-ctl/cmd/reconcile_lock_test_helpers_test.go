package cmd

import "runtime"

// isWindowsRuntime returns true when the test binary is compiled for Windows.
// Used by reconcile_lock_test.go to skip PID-probe tests that need Unix semantics.
func isWindowsRuntime() bool {
	return runtime.GOOS == "windows"
}
