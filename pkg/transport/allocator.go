package transport

import (
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"
)

var (
	errAllocatorIPv6Unsupported = errors.New("allocator currently supports IPv4 pools only")
	errAllocatorInvalidPrefix   = errors.New("allocator prefix length must be between pool bits and /30")
)

// Allocation is a transport addressing assignment for one master-endpoint tunnel.
type Allocation struct {
	Tunnel      string
	Subnet      netip.Prefix
	MasterIP    netip.Addr
	EndpointIP  netip.Addr
	AllocatedAt time.Time
}

// Allocator assigns non-overlapping subnets from a shared address pool.
type Allocator struct {
	mu          sync.Mutex
	pool        netip.Prefix
	prefixLen   int
	allocations []Allocation
}

// NewAllocator creates a transport allocator for the given pool and subnet prefix length.
func NewAllocator(pool netip.Prefix, prefixLen int) *Allocator {
	return &Allocator{
		pool:        pool.Masked(),
		prefixLen:   prefixLen,
		allocations: []Allocation{},
	}
}

// Allocate allocates a transport subnet for a master-endpoint pair.
func (a *Allocator) Allocate(master, endpoint string) (Allocation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	tunnel := master + "/" + endpoint
	for _, allocation := range a.allocations {
		if allocation.Tunnel == tunnel {
			return allocation, nil
		}
	}

	if !a.pool.Addr().Is4() {
		return Allocation{}, errAllocatorIPv6Unsupported
	}
	if a.prefixLen < a.pool.Bits() || a.prefixLen > 30 {
		return Allocation{}, fmt.Errorf("%w: pool=%s requested=/%d", errAllocatorInvalidPrefix, a.pool, a.prefixLen)
	}

	start := ipv4ToUint32(a.pool.Addr())
	step := uint32(1) << uint32(32-a.prefixLen)
	limit := uint64(1) << uint64(32-a.pool.Bits())
	maxCandidates := limit / uint64(step)

	for index := uint64(0); index < maxCandidates; index++ {
		candidateAddr := start + uint32(index)*step
		candidate := netip.PrefixFrom(uint32ToIPv4(candidateAddr), a.prefixLen)
		if !a.pool.Contains(candidate.Addr()) {
			break
		}

		if overlapsAny(candidate, a.allocations) {
			continue
		}

		masterIP := candidate.Addr().Next()
		endpointIP := masterIP.Next()
		allocation := Allocation{
			Tunnel:      tunnel,
			Subnet:      candidate,
			MasterIP:    masterIP,
			EndpointIP:  endpointIP,
			AllocatedAt: time.Now().UTC(),
		}
		a.allocations = append(a.allocations, allocation)
		return allocation, nil
	}

	return Allocation{}, fmt.Errorf("transport pool exhausted for %s (pool=%s, prefix_length=%d)", tunnel, a.pool, a.prefixLen)
}

// Find finds an allocation by master-endpoint pair.
func (a *Allocator) Find(master, endpoint string) (Allocation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	tunnel := master + "/" + endpoint
	for _, allocation := range a.allocations {
		if allocation.Tunnel == tunnel {
			return allocation, true
		}
	}
	return Allocation{}, false
}

// Deallocate returns a /30 subnet back to the pool for the given master-endpoint pair.
// Returns true if the allocation was found and removed, false if it didn't exist.
func (a *Allocator) Deallocate(master, endpoint string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	tunnel := master + "/" + endpoint
	for i, allocation := range a.allocations {
		if allocation.Tunnel == tunnel {
			a.allocations = append(a.allocations[:i], a.allocations[i+1:]...)
			return true
		}
	}
	return false
}

// Allocations returns a copy of all known allocations.
func (a *Allocator) Allocations() []Allocation {
	a.mu.Lock()
	defer a.mu.Unlock()

	copied := make([]Allocation, len(a.allocations))
	copy(copied, a.allocations)
	return copied
}

func overlapsAny(candidate netip.Prefix, allocations []Allocation) bool {
	for _, allocation := range allocations {
		if candidate.Overlaps(allocation.Subnet) {
			return true
		}
	}
	return false
}

func ipv4ToUint32(addr netip.Addr) uint32 {
	parts := addr.As4()
	return uint32(parts[0])<<24 | uint32(parts[1])<<16 | uint32(parts[2])<<8 | uint32(parts[3])
}

func uint32ToIPv4(value uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{
		byte(value >> 24),
		byte(value >> 16),
		byte(value >> 8),
		byte(value),
	})
}
