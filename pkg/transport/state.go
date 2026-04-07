package transport

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// AllocationState is the persisted representation of Allocation.
type AllocationState struct {
	Tunnel      string    `yaml:"tunnel"`
	Subnet      string    `yaml:"subnet"`
	MasterIP    string    `yaml:"master_ip"`
	EndpointIP  string    `yaml:"endpoint_ip"`
	AllocatedAt time.Time `yaml:"allocated_at"`
}

// TransportState is the persisted transport allocator state.
type TransportState struct {
	Pool         string            `yaml:"pool"`
	PrefixLength int               `yaml:"prefix_length"`
	Allocations  []AllocationState `yaml:"allocations"`
}

// LoadState loads transport allocations from a YAML state file and merges them into memory.
func (a *Allocator) LoadState(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read transport state file %q: %w", path, err)
	}

	var state TransportState
	if err := yaml.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("unmarshal transport state file %q: %w", path, err)
	}

	parsed := make([]Allocation, 0, len(state.Allocations))
	for _, rawAllocation := range state.Allocations {
		subnet, err := netip.ParsePrefix(rawAllocation.Subnet)
		if err != nil {
			return fmt.Errorf("parse subnet for tunnel %q: %w", rawAllocation.Tunnel, err)
		}
		masterIP, err := netip.ParseAddr(rawAllocation.MasterIP)
		if err != nil {
			return fmt.Errorf("parse master ip for tunnel %q: %w", rawAllocation.Tunnel, err)
		}
		endpointIP, err := netip.ParseAddr(rawAllocation.EndpointIP)
		if err != nil {
			return fmt.Errorf("parse endpoint ip for tunnel %q: %w", rawAllocation.Tunnel, err)
		}

		parsed = append(parsed, Allocation{
			Tunnel:      rawAllocation.Tunnel,
			Subnet:      subnet,
			MasterIP:    masterIP,
			EndpointIP:  endpointIP,
			AllocatedAt: rawAllocation.AllocatedAt,
		})
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	existingByTunnel := make(map[string]struct{}, len(a.allocations))
	for _, allocation := range a.allocations {
		existingByTunnel[allocation.Tunnel] = struct{}{}
	}

	for _, allocation := range parsed {
		if _, exists := existingByTunnel[allocation.Tunnel]; exists {
			continue
		}
		a.allocations = append(a.allocations, allocation)
		existingByTunnel[allocation.Tunnel] = struct{}{}
	}

	return nil
}

// SaveState saves transport allocations to a YAML state file via atomic rename.
func (a *Allocator) SaveState(path string) error {
	a.mu.Lock()
	pool := a.pool.String()
	prefixLen := a.prefixLen
	allocations := make([]Allocation, len(a.allocations))
	copy(allocations, a.allocations)
	a.mu.Unlock()

	stateAllocations := make([]AllocationState, 0, len(allocations))
	for _, allocation := range allocations {
		stateAllocations = append(stateAllocations, AllocationState{
			Tunnel:      allocation.Tunnel,
			Subnet:      allocation.Subnet.String(),
			MasterIP:    allocation.MasterIP.String(),
			EndpointIP:  allocation.EndpointIP.String(),
			AllocatedAt: allocation.AllocatedAt,
		})
	}

	state := TransportState{
		Pool:         pool,
		PrefixLength: prefixLen,
		Allocations:  stateAllocations,
	}

	encoded, err := yaml.Marshal(&state)
	if err != nil {
		return fmt.Errorf("marshal transport state %q: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create transport state directory for %q: %w", path, err)
	}

	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, encoded, 0o600); err != nil {
		return fmt.Errorf("write temporary transport state %q: %w", tempPath, err)
	}

	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replace transport state %q: %w", path, err)
	}

	return nil
}
