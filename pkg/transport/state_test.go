package transport

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSaveLoad(t *testing.T) {
	t.Parallel()

	pool := mustParsePrefix(t, "10.200.0.0/24")
	allocator := NewAllocator(pool, 30)

	first, err := allocator.Allocate("master1", "ep1")
	if err != nil {
		t.Fatalf("allocate first tunnel: %v", err)
	}

	second, err := allocator.Allocate("master2", "ep2")
	if err != nil {
		t.Fatalf("allocate second tunnel: %v", err)
	}

	statePath := filepath.Join(t.TempDir(), "transport-state.yaml")
	if err := allocator.SaveState(statePath); err != nil {
		t.Fatalf("save state: %v", err)
	}

	loadedAllocator := NewAllocator(pool, 30)
	if err := loadedAllocator.LoadState(statePath); err != nil {
		t.Fatalf("load state: %v", err)
	}

	loaded := loadedAllocator.Allocations()
	if len(loaded) != 2 {
		t.Fatalf("expected two allocations after load, got %d", len(loaded))
	}

	expected := map[string]Allocation{
		first.Tunnel:  first,
		second.Tunnel: second,
	}

	for _, allocation := range loaded {
		want, ok := expected[allocation.Tunnel]
		if !ok {
			t.Fatalf("unexpected allocation after load: %s", allocation.Tunnel)
		}
		if want.Subnet != allocation.Subnet || want.MasterIP != allocation.MasterIP || want.EndpointIP != allocation.EndpointIP {
			t.Fatalf("loaded allocation mismatch for %s: got=%+v want=%+v", allocation.Tunnel, allocation, want)
		}
	}
}

func TestLoadMissing(t *testing.T) {
	t.Parallel()

	pool := mustParsePrefix(t, "10.200.0.0/24")
	allocator := NewAllocator(pool, 30)

	path := filepath.Join(t.TempDir(), "missing", "state.yaml")
	if err := allocator.LoadState(path); err == nil {
		t.Fatalf("expected error for missing state file")
	}
}

func TestSaveAtomic(t *testing.T) {
	t.Parallel()

	pool := mustParsePrefix(t, "10.200.0.0/24")
	allocator := NewAllocator(pool, 30)
	if _, err := allocator.Allocate("master1", "ep1"); err != nil {
		t.Fatalf("allocate tunnel: %v", err)
	}

	statePath := filepath.Join(t.TempDir(), "transport-state.yaml")
	if err := allocator.SaveState(statePath); err != nil {
		t.Fatalf("save state: %v", err)
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read saved state: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("saved state file is empty")
	}

	var decoded TransportState
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("saved state is not valid YAML: %v", err)
	}
	if len(decoded.Allocations) != 1 {
		t.Fatalf("expected one saved allocation, got %d", len(decoded.Allocations))
	}
	if decoded.Allocations[0].AllocatedAt.IsZero() {
		t.Fatalf("expected allocated_at to be set")
	}
}

func mustParsePrefix(t *testing.T, raw string) netip.Prefix {
	t.Helper()

	prefix, err := netip.ParsePrefix(raw)
	if err != nil {
		t.Fatalf("parse prefix %q: %v", raw, err)
	}
	return prefix
}
