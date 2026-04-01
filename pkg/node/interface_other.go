//go:build !linux

package node

// discoverWANInterface returns "eth0" on non-Linux platforms.
func discoverWANInterface() string {
	return "eth0"
}
