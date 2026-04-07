//go:build linux

package routing

import (
	"os"
	"testing"
)

func TestNewNftablesFirewall(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root (nftables)")
	}
	// No t.Parallel() — nftables tests share a single kernel table (awg_mesh).
	fw, err := NewNftablesFirewall()
	if err != nil {
		t.Skipf("nftables not available: %v", err)
	}
	if fw == nil {
		t.Fatal("expected non-nil firewall")
	}
	// Clean up
	_ = fw.TeardownNAT()
}

func TestNftablesSetupAndTeardownNAT(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root (nftables)")
	}
	// No t.Parallel() — nftables tests share a single kernel table (awg_mesh).
	fw, err := NewNftablesFirewall()
	if err != nil {
		t.Skipf("nftables not available: %v", err)
	}
	if err := fw.SetupNAT("lo"); err != nil {
		t.Fatalf("SetupNAT: %v", err)
	}

	// Teardown should succeed
	if err := fw.TeardownNAT(); err != nil {
		t.Fatalf("TeardownNAT: %v", err)
	}
}

func TestNftablesClampMSSToPMTU(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root (nftables)")
	}
	// No t.Parallel() — nftables tests share a single kernel table (awg_mesh).
	fw, err := NewNftablesFirewall()
	if err != nil {
		t.Skipf("nftables not available: %v", err)
	}
	defer func() { _ = fw.TeardownNAT() }()

	if err := fw.ClampMSSToPMTU(); err != nil {
		t.Fatalf("ClampMSSToPMTU: %v", err)
	}
}

func TestNftablesEnableStickyECMP(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root (nftables)")
	}
	// No t.Parallel() — nftables tests share a single kernel table (awg_mesh).
	fw, err := NewNftablesFirewall()
	if err != nil {
		t.Skipf("nftables not available: %v", err)
	}
	defer func() { _ = fw.TeardownNAT() }()

	if err := fw.EnableStickyECMP("172.20.70.1/32"); err != nil {
		t.Fatalf("EnableStickyECMP: %v", err)
	}
}

func TestNftablesTeardownIdempotent(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root (nftables)")
	}
	// No t.Parallel() — nftables tests share a single kernel table (awg_mesh).
	fw, err := NewNftablesFirewall()
	if err != nil {
		t.Skipf("nftables not available: %v", err)
	}

	// Teardown without setup should be safe
	if err := fw.TeardownNAT(); err != nil {
		t.Fatalf("TeardownNAT (no setup): %v", err)
	}

	// Double teardown
	if err := fw.TeardownNAT(); err != nil {
		t.Fatalf("TeardownNAT (double): %v", err)
	}
}
