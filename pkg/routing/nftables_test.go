//go:build linux

package routing

import (
	"net"
	"os"
	"testing"

	"github.com/google/nftables"
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

// TestEnableStickyECMP_ScopedToCIDR verifies that EnableStickyECMP installs rules
// with a CIDR filter expression in the first position.
func TestEnableStickyECMP_ScopedToCIDR(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root (nftables)")
	}
	// No t.Parallel() — nftables tests share a single kernel table (awg_mesh).
	fw, err := NewNftablesFirewall()
	if err != nil {
		t.Skipf("nftables not available: %v", err)
	}
	defer func() { _ = fw.TeardownNAT() }()

	const cidr = "192.0.2.0/24"
	if err := fw.EnableStickyECMP(cidr); err != nil {
		t.Fatalf("EnableStickyECMP: %v", err)
	}

	// List rules in both chains and verify CIDR filter is present.
	fw.mu.Lock()
	table := fw.table
	conn := fw.conn
	fw.mu.Unlock()

	for _, chainName := range []string{"mangle_prerouting", "mangle_postrouting"} {
		chain := &nftables.Chain{Name: chainName, Table: table}
		rules, err := conn.GetRules(table, chain)
		if err != nil {
			t.Fatalf("GetRules(%s): %v", chainName, err)
		}
		found := false
		for _, rule := range rules {
			_, ipNet, _ := net.ParseCIDR(cidr)
			if ruleMatchesCIDR(rule, ipNet) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("chain %s: no rule with CIDR filter for %s", chainName, cidr)
		}
	}
}

// TestDisableStickyECMP_OnlyTargetCIDR verifies that DisableStickyECMP removes
// rules only for the target CIDR and leaves rules for other CIDRs intact.
func TestDisableStickyECMP_OnlyTargetCIDR(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root (nftables)")
	}
	// No t.Parallel() — nftables tests share a single kernel table (awg_mesh).
	fw, err := NewNftablesFirewall()
	if err != nil {
		t.Skipf("nftables not available: %v", err)
	}
	defer func() { _ = fw.TeardownNAT() }()

	const cidrA = "192.0.2.0/24"
	const cidrB = "198.51.100.0/24"

	if err := fw.EnableStickyECMP(cidrA); err != nil {
		t.Fatalf("EnableStickyECMP A: %v", err)
	}
	if err := fw.EnableStickyECMP(cidrB); err != nil {
		t.Fatalf("EnableStickyECMP B: %v", err)
	}

	// Disable only A.
	if err := fw.DisableStickyECMP(cidrA); err != nil {
		t.Fatalf("DisableStickyECMP A: %v", err)
	}

	// Check that B's rules remain and A's are gone.
	fw.mu.Lock()
	table := fw.table
	conn := fw.conn
	fw.mu.Unlock()

	_, ipNetA, _ := net.ParseCIDR(cidrA)
	_, ipNetB, _ := net.ParseCIDR(cidrB)

	for _, chainName := range []string{"mangle_prerouting", "mangle_postrouting"} {
		chain := &nftables.Chain{Name: chainName, Table: table}
		rules, err := conn.GetRules(table, chain)
		if err != nil {
			t.Fatalf("GetRules(%s): %v", chainName, err)
		}
		for _, rule := range rules {
			if ruleMatchesCIDR(rule, ipNetA) {
				t.Errorf("chain %s: rule for %s still present after Disable", chainName, cidrA)
			}
		}
		foundB := false
		for _, rule := range rules {
			if ruleMatchesCIDR(rule, ipNetB) {
				foundB = true
				break
			}
		}
		if !foundB {
			t.Errorf("chain %s: rule for %s unexpectedly removed", chainName, cidrB)
		}
	}
}

// TestDisableStickyECMP_NeverEnabled verifies DisableStickyECMP is idempotent
// when called without a prior EnableStickyECMP.
func TestDisableStickyECMP_NeverEnabled(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root (nftables)")
	}
	// No t.Parallel() — nftables tests share a single kernel table (awg_mesh).
	fw, err := NewNftablesFirewall()
	if err != nil {
		t.Skipf("nftables not available: %v", err)
	}
	defer func() { _ = fw.TeardownNAT() }()

	if err := fw.DisableStickyECMP("192.0.2.0/24"); err != nil {
		t.Fatalf("DisableStickyECMP on never-enabled CIDR: %v", err)
	}
}

// TestDisableStickyECMP_Idempotent verifies that DisableStickyECMP called twice
// for the same CIDR does not return an error.
func TestDisableStickyECMP_Idempotent(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root (nftables)")
	}
	// No t.Parallel() — nftables tests share a single kernel table (awg_mesh).
	fw, err := NewNftablesFirewall()
	if err != nil {
		t.Skipf("nftables not available: %v", err)
	}
	defer func() { _ = fw.TeardownNAT() }()

	const cidr = "192.0.2.0/24"
	if err := fw.EnableStickyECMP(cidr); err != nil {
		t.Fatalf("EnableStickyECMP: %v", err)
	}
	if err := fw.DisableStickyECMP(cidr); err != nil {
		t.Fatalf("DisableStickyECMP (first): %v", err)
	}
	if err := fw.DisableStickyECMP(cidr); err != nil {
		t.Fatalf("DisableStickyECMP (second, idempotent): %v", err)
	}
}
