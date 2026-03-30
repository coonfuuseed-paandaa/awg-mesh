//go:build linux

package routing

import (
	"testing"
)

func TestNewNftablesFirewall(t *testing.T) {
	t.Parallel()
	fw, err := NewNftablesFirewall()
	if err != nil {
		t.Skipf("nftables not available: %v", err)
	}
	if fw == nil {
		t.Fatal("expected non-nil firewall")
	}
	// Clean up
	fw.TeardownNAT()
}

func TestNftablesSetupAndTeardownNAT(t *testing.T) {
	t.Parallel()
	fw, err := NewNftablesFirewall()
	if err != nil {
		t.Skipf("nftables not available: %v", err)
	}
	defer fw.TeardownNAT()

	if err := fw.SetupNAT("lo"); err != nil {
		t.Fatalf("SetupNAT: %v", err)
	}

	// Teardown should succeed
	if err := fw.TeardownNAT(); err != nil {
		t.Fatalf("TeardownNAT: %v", err)
	}
}

func TestNftablesClampMSSToPMTU(t *testing.T) {
	t.Parallel()
	fw, err := NewNftablesFirewall()
	if err != nil {
		t.Skipf("nftables not available: %v", err)
	}
	defer fw.TeardownNAT()

	if err := fw.ClampMSSToPMTU(); err != nil {
		t.Fatalf("ClampMSSToPMTU: %v", err)
	}
}

func TestNftablesEnableStickyECMP(t *testing.T) {
	t.Parallel()
	fw, err := NewNftablesFirewall()
	if err != nil {
		t.Skipf("nftables not available: %v", err)
	}
	defer fw.TeardownNAT()

	if err := fw.EnableStickyECMP("172.20.70.1/32"); err != nil {
		t.Fatalf("EnableStickyECMP: %v", err)
	}
}

func TestNftablesTeardownIdempotent(t *testing.T) {
	t.Parallel()
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
