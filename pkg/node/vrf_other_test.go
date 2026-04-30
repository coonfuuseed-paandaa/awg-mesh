//go:build !linux

package node

import (
	"net"
	"testing"
)

// TestVRFManagerStubBehavior verifies that all VRFManager methods return the
// expected "not supported" error on non-Linux platforms and that the build
// compiles without privilege or kernel access.
func TestVRFManagerStubBehavior(t *testing.T) {
	mgr := NewVRFManager("vrf_overlay", 100, net.ParseIP("172.21.92.1"))
	if mgr == nil {
		t.Fatal("NewVRFManager returned nil")
	}

	if err := mgr.Setup(); err == nil {
		t.Error("Setup() = nil, want error on non-Linux platform")
	}
	if err := mgr.EnslaveInterface("dummy0"); err == nil {
		t.Error("EnslaveInterface() = nil, want error on non-Linux platform")
	}
	if err := mgr.UnslaveInterface("dummy0"); err == nil {
		t.Error("UnslaveInterface() = nil, want error on non-Linux platform")
	}
	if err := mgr.Teardown(); err == nil {
		t.Error("Teardown() = nil, want error on non-Linux platform")
	}
	if mgr.IsCreated() {
		t.Error("IsCreated() = true, want false on non-Linux platform")
	}
	if mgr.Name() != "" {
		t.Errorf("Name() = %q, want empty string on non-Linux platform", mgr.Name())
	}
	if mgr.Table() != 0 {
		t.Errorf("Table() = %d, want 0 on non-Linux platform", mgr.Table())
	}
	if err := IsVRFSupported(); err == nil {
		t.Error("IsVRFSupported() = nil, want error on non-Linux platform")
	}
}
