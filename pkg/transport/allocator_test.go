package transport

import (
	"net/netip"
	"testing"
)

func TestAllocate(t *testing.T) {
	t.Parallel()

	pool := netip.MustParsePrefix("10.100.0.0/24")
	allocator := NewAllocator(pool, 30)

	allocationOne, err := allocator.Allocate("master1", "ep1")
	if err != nil {
		t.Fatalf("allocate master1/ep1: %v", err)
	}

	allocationTwo, err := allocator.Allocate("master1", "ep2")
	if err != nil {
		t.Fatalf("allocate master1/ep2: %v", err)
	}

	allocationThree, err := allocator.Allocate("master2", "ep1")
	if err != nil {
		t.Fatalf("allocate master2/ep1: %v", err)
	}

	if allocationOne.Subnet == allocationTwo.Subnet || allocationOne.Subnet == allocationThree.Subnet || allocationTwo.Subnet == allocationThree.Subnet {
		t.Fatalf("expected unique subnets, got %v %v %v", allocationOne.Subnet, allocationTwo.Subnet, allocationThree.Subnet)
	}

	assertAllocationIPs(t, allocationOne)
	assertAllocationIPs(t, allocationTwo)
	assertAllocationIPs(t, allocationThree)

	assertNoOverlap(t, allocationOne.Subnet, allocationTwo.Subnet)
	assertNoOverlap(t, allocationOne.Subnet, allocationThree.Subnet)
	assertNoOverlap(t, allocationTwo.Subnet, allocationThree.Subnet)
}

func TestAllocateIdempotent(t *testing.T) {
	t.Parallel()

	pool := netip.MustParsePrefix("10.100.0.0/24")
	allocator := NewAllocator(pool, 30)

	first, err := allocator.Allocate("master1", "ep1")
	if err != nil {
		t.Fatalf("first allocation failed: %v", err)
	}

	second, err := allocator.Allocate("master1", "ep1")
	if err != nil {
		t.Fatalf("second allocation failed: %v", err)
	}

	if first != second {
		t.Fatalf("expected idempotent allocation, first=%+v second=%+v", first, second)
	}
}

func TestAllocateExhaust(t *testing.T) {
	t.Parallel()

	pool := netip.MustParsePrefix("10.100.0.0/28")
	allocator := NewAllocator(pool, 30)

	pairs := [][2]string{
		{"master1", "ep1"},
		{"master1", "ep2"},
		{"master2", "ep1"},
		{"master2", "ep2"},
	}

	for _, pair := range pairs {
		if _, err := allocator.Allocate(pair[0], pair[1]); err != nil {
			t.Fatalf("unexpected allocation failure for %s/%s: %v", pair[0], pair[1], err)
		}
	}

	if _, err := allocator.Allocate("master3", "ep1"); err == nil {
		t.Fatalf("expected pool exhaustion error")
	}
}

func TestFind(t *testing.T) {
	t.Parallel()

	pool := netip.MustParsePrefix("10.100.0.0/24")
	allocator := NewAllocator(pool, 30)

	expected, err := allocator.Allocate("master1", "ep1")
	if err != nil {
		t.Fatalf("allocate master1/ep1: %v", err)
	}

	found, ok := allocator.Find("master1", "ep1")
	if !ok {
		t.Fatalf("expected allocation to be found")
	}

	if expected != found {
		t.Fatalf("found allocation mismatch, expected=%+v found=%+v", expected, found)
	}

	if _, ok := allocator.Find("master1", "ep2"); ok {
		t.Fatalf("did not expect master1/ep2 to be found")
	}
}

func TestFindMissing(t *testing.T) {
	t.Parallel()

	pool := netip.MustParsePrefix("10.100.0.0/24")
	allocator := NewAllocator(pool, 30)

	if _, ok := allocator.Find("x", "y"); ok {
		t.Fatalf("did not expect allocation to be found")
	}
}

func TestDeallocate(t *testing.T) {
	t.Parallel()

	pool := netip.MustParsePrefix("10.100.0.0/28")
	allocator := NewAllocator(pool, 30)

	// Allocate 2 pairs
	_, err := allocator.Allocate("master1", "ep1")
	if err != nil {
		t.Fatalf("allocate master1/ep1: %v", err)
	}
	_, err = allocator.Allocate("master1", "ep2")
	if err != nil {
		t.Fatalf("allocate master1/ep2: %v", err)
	}

	// Deallocate ep1
	if !allocator.Deallocate("master1", "ep1") {
		t.Fatal("expected Deallocate to return true")
	}

	// ep1 should no longer be found
	if _, ok := allocator.Find("master1", "ep1"); ok {
		t.Fatal("ep1 should not be found after Deallocate")
	}

	// ep2 should still exist
	if _, ok := allocator.Find("master1", "ep2"); !ok {
		t.Fatal("ep2 should still be found")
	}

	// Re-allocate ep1 — should get the freed subnet back (or a new one)
	realloc, err := allocator.Allocate("master1", "ep1")
	if err != nil {
		t.Fatalf("re-allocate master1/ep1: %v", err)
	}
	if realloc.Tunnel != "master1/ep1" {
		t.Fatalf("unexpected tunnel name: %s", realloc.Tunnel)
	}

	// Deallocate non-existent pair
	if allocator.Deallocate("master1", "ep99") {
		t.Fatal("expected Deallocate to return false for non-existent pair")
	}
}

func assertAllocationIPs(t *testing.T, allocation Allocation) {
	t.Helper()

	expectedMaster := allocation.Subnet.Addr().Next()
	if allocation.MasterIP != expectedMaster {
		t.Fatalf("unexpected master IP for %s: got %s want %s", allocation.Tunnel, allocation.MasterIP, expectedMaster)
	}

	expectedEndpoint := expectedMaster.Next()
	if allocation.EndpointIP != expectedEndpoint {
		t.Fatalf("unexpected endpoint IP for %s: got %s want %s", allocation.Tunnel, allocation.EndpointIP, expectedEndpoint)
	}
}

func assertNoOverlap(t *testing.T, left, right netip.Prefix) {
	t.Helper()

	if left.Overlaps(right) || right.Overlaps(left) {
		t.Fatalf("expected non-overlapping subnets, got %s and %s", left, right)
	}
}
