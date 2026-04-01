//go:build linux

package ebpf

import (
	"net"
	"testing"
)

func TestNewForwarder(t *testing.T) {
	t.Parallel()

	f, err := NewForwarder()
	if err != nil {
		t.Skipf("eBPF not available (need CAP_BPF or root): %v", err)
	}
	defer func() { _ = f.Close() }()

	if f.fwdMap == nil {
		t.Fatal("expected fwd_map to be created")
	}
}

func TestForwarderSetDeleteRoute(t *testing.T) {
	t.Parallel()

	f, err := NewForwarder()
	if err != nil {
		t.Skipf("eBPF not available: %v", err)
	}
	defer func() { _ = f.Close() }()

	ip := net.ParseIP("172.20.70.34").To4()
	ifindex := 5

	if err := f.SetRoute(ip, ifindex); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}

	// Verify entry exists via lookup
	var value uint32
	key := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	if err := f.fwdMap.Lookup(key, &value); err != nil {
		t.Fatalf("fwd_map lookup after SetRoute: %v", err)
	}
	if value != uint32(ifindex) {
		t.Fatalf("expected ifindex %d, got %d", ifindex, value)
	}

	if err := f.DeleteRoute(ip); err != nil {
		t.Fatalf("DeleteRoute: %v", err)
	}
}

func TestForwarderSetRouteInvalidIP(t *testing.T) {
	t.Parallel()

	f, err := NewForwarder()
	if err != nil {
		t.Skipf("eBPF not available: %v", err)
	}
	defer func() { _ = f.Close() }()

	// IPv6 address should fail
	if err := f.SetRoute(net.ParseIP("::1"), 1); err == nil {
		t.Fatal("expected error for IPv6 address")
	}
}

func TestForwarderAttachWithoutProgram(t *testing.T) {
	t.Parallel()

	f, err := NewForwarder()
	if err != nil {
		t.Skipf("eBPF not available: %v", err)
	}
	defer f.Close()

	// Attach without program loaded should be no-op (graceful degradation)
	if err := f.Attach("lo"); err != nil {
		t.Fatalf("Attach without program should succeed (no-op): %v", err)
	}
}
