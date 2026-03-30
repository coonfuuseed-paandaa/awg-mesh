//go:build linux

package routing

import (
	"fmt"
	"os"
	"strings"
)

// ProcSysctl implements Sysctl by writing to /proc/sys/.
type ProcSysctl struct{}

// NewProcSysctl creates a new procfs-based sysctl manager.
func NewProcSysctl() *ProcSysctl {
	return &ProcSysctl{}
}

// EnableForwarding enables IPv4 packet forwarding.
func (s *ProcSysctl) EnableForwarding() error {
	return writeSysctl("/proc/sys/net/ipv4/ip_forward", "1")
}

// EnableL4Hash sets ECMP hash policy to L4 (src/dst port) for better flow distribution.
func (s *ProcSysctl) EnableL4Hash() error {
	return writeSysctl("/proc/sys/net/ipv4/fib_multipath_hash_policy", "1")
}

func writeSysctl(path string, value string) error {
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read sysctl %s: %w", path, err)
	}
	if strings.TrimSpace(string(current)) == value {
		return nil // already set
	}
	if err := os.WriteFile(path, []byte(value), 0644); err != nil {
		return fmt.Errorf("write sysctl %s=%s: %w", path, value, err)
	}
	return nil
}
