package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
)

// TestInspectCommandRegistered verifies that 'mesh-ctl inspect' is registered
// and requires exactly one positional argument.
//
// Anti-stub: if newInspectCommand() is not added to NewRootCommand, Execute()
// returns "unknown command" instead of "accepts 1 arg".
func TestInspectCommandRegistered(t *testing.T) {
	// No t.Parallel — cobra persistent flags bind to package-level globals.

	t.Run("no-args returns usage error not unknown-command", func(t *testing.T) {
		root := NewRootCommand("test")
		root.SilenceUsage = true
		root.SilenceErrors = true
		root.SetArgs([]string{"inspect"})

		err := root.Execute()
		if err == nil {
			t.Fatal("expected error for missing <node> argument, got nil")
		}
		if strings.Contains(err.Error(), "unknown command") {
			t.Errorf("got 'unknown command' — newInspectCommand not registered: %v", err)
		}
	})

	t.Run("valid node with missing topology returns topology error", func(t *testing.T) {
		root := NewRootCommand("test")
		root.SilenceUsage = true
		root.SilenceErrors = true
		root.SetArgs([]string{"inspect", "--topology", "/nonexistent/topology.yml", "master-01"})

		err := root.Execute()
		if err == nil {
			t.Fatal("expected error for nonexistent topology, got nil")
		}
		if strings.Contains(err.Error(), "unknown command") {
			t.Errorf("got 'unknown command' — command not registered: %v", err)
		}
		if !strings.Contains(err.Error(), "topology") && !strings.Contains(err.Error(), "nonexistent") {
			t.Errorf("expected topology-load error, got: %v", err)
		}
	})
}

// TestBuildAdminView verifies that buildAdminView correctly builds the expected
// peer list for masters and endpoints.
//
// Anti-stub: if buildAdminView returns empty slice unconditionally, all
// wantLen assertions fail.
func TestBuildAdminView(t *testing.T) {
	t.Parallel()

	masterPubkey := make([]byte, 32)
	masterPubkey[0] = 0xAA
	epPubkey := make([]byte, 32)
	epPubkey[0] = 0xBB

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-1", Host: "10.0.0.1", OverlayIP: "10.1.0.1", ListenPort: 51820, Endpoints: []string{"ep-1", "ep-2"}},
		},
		Endpoints: []topology.EndpointNode{
			{Name: "ep-1", Host: "10.0.0.2", OverlayIP: "10.1.0.2", ListenPort: 51820},
			{Name: "ep-2", Host: "10.0.0.3", OverlayIP: "10.1.0.3", ListenPort: 51820},
		},
	}

	t.Run("master view: one entry per bound endpoint", func(t *testing.T) {
		t.Parallel()

		cfgDir := t.TempDir()

		// Write pubkeys for both endpoints.
		for _, epName := range []string{"ep-1", "ep-2"} {
			nd := nodeDir(cfgDir, epName)
			if err := os.MkdirAll(nd, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(nd, "pubkey"), epPubkey, 0600); err != nil {
				t.Fatal(err)
			}
		}

		master := &topo.Masters[0]
		peers := buildAdminView(topo, "master-1", cfgDir, master, nil)

		if len(peers) != 2 {
			t.Fatalf("want 2 admin peers for master with 2 endpoints, got %d", len(peers))
		}
		for _, p := range peers {
			if p.pubkeyHex == "" {
				t.Errorf("peer %q: expected non-empty pubkeyHex", p.name)
			}
			if len(p.allowedIPs) == 0 {
				t.Errorf("peer %q: expected non-empty allowedIPs", p.name)
			}
		}
	})

	t.Run("endpoint view: one entry per bound master", func(t *testing.T) {
		t.Parallel()

		cfgDir := t.TempDir()

		// Write pubkey for master.
		nd := nodeDir(cfgDir, "master-1")
		if err := os.MkdirAll(nd, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nd, "pubkey"), masterPubkey, 0600); err != nil {
			t.Fatal(err)
		}

		ep1 := &topo.Endpoints[0]
		peers := buildAdminView(topo, "ep-1", cfgDir, nil, ep1)

		if len(peers) != 1 {
			t.Fatalf("want 1 admin peer for endpoint bound to 1 master, got %d", len(peers))
		}
		if peers[0].name != "master-1" {
			t.Errorf("want peer name 'master-1', got %q", peers[0].name)
		}
	})

	t.Run("master with no pubkey file: returns peer with empty pubkeyHex", func(t *testing.T) {
		t.Parallel()

		cfgDir := t.TempDir() // no pubkey files written

		ep1 := &topo.Endpoints[0]
		peers := buildAdminView(topo, "ep-1", cfgDir, nil, ep1)

		if len(peers) != 1 {
			t.Fatalf("want 1 peer even when pubkey missing, got %d", len(peers))
		}
		if peers[0].pubkeyHex != "" {
			t.Errorf("want empty pubkeyHex when file missing, got %q", peers[0].pubkeyHex)
		}
	})
}

// TestIPsMatch verifies the ipsMatch helper for set-equality of allowed-IP slices.
//
// Anti-stub: replacing ipsMatch with `return true` makes the mismatch cases
// fail; replacing with `return false` makes the match cases fail.
func TestIPsMatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		expected []string
		actual   []string
		want     bool
	}{
		{"empty expected = wildcard match", nil, []string{"10.0.0.0/8"}, true},
		{"identical single IP", []string{"10.0.0.1/32"}, []string{"10.0.0.1/32"}, true},
		{"identical multiple IPs same order", []string{"10.0.0.0/8", "192.168.0.0/16"}, []string{"10.0.0.0/8", "192.168.0.0/16"}, true},
		{"different lengths", []string{"10.0.0.0/8", "192.168.0.0/16"}, []string{"10.0.0.0/8"}, false},
		{"different IPs", []string{"10.0.0.1/32"}, []string{"10.0.0.2/32"}, false},
		{"extra IP in actual", []string{"10.0.0.0/8"}, []string{"10.0.0.0/8", "192.168.0.0/16"}, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ipsMatch(tc.expected, tc.actual)
			if got != tc.want {
				t.Errorf("ipsMatch(%v, %v) = %v, want %v", tc.expected, tc.actual, got, tc.want)
			}
		})
	}
}

// TestTruncate verifies the truncate helper for column width limiting.
//
// Anti-stub: replacing truncate with `return s` makes the long-string case fail.
func TestTruncate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		n     int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"toolongstring", 8, "toolong…"},
		{"", 5, ""},
		{"abc", 1, "…"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.input+"/"+string(rune('0'+tc.n)), func(t *testing.T) {
			t.Parallel()
			got := truncate(tc.input, tc.n)
			if got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.input, tc.n, got, tc.want)
			}
		})
	}
}

// TestPrintInspectReportNoDrift verifies that printInspectReport returns false
// when there are no drift reasons on any row.
//
// Anti-stub: replacing printInspectReport with `return true` makes the
// hasDrift assertion fail.
func TestPrintInspectReportNoDrift(t *testing.T) {
	t.Parallel()

	state := &proto.TransportStateResponse{
		NodeName:  "master-1",
		Mode:      "master",
		OverlayIp: "10.0.0.1/32",
	}

	rows := []driftRow{
		{peerName: "ep-1", adminKey: "aabb", diskKey: "aabb", runtimeKey: "aabb",
			adminIPs: "10.0.1.0/24", diskIPs: "10.0.1.0/24", runtimeIPs: "10.0.1.0/24"},
	}

	hasDrift := printInspectReport("master-1", state, rows)
	if hasDrift {
		t.Error("expected hasDrift=false for rows with no driftReasons, got true")
	}
}

// TestPrintInspectReportWithDrift verifies that printInspectReport returns true
// when any row has drift reasons.
//
// Anti-stub: replacing printInspectReport with `return false` makes this fail.
func TestPrintInspectReportWithDrift(t *testing.T) {
	t.Parallel()

	state := &proto.TransportStateResponse{
		NodeName:  "master-1",
		Mode:      "master",
		OverlayIp: "10.0.0.1/32",
	}

	rows := []driftRow{
		{peerName: "ep-1", adminKey: "aabb", diskKey: "ccdd",
			driftReasons: []string{"key_mismatch"}},
	}

	hasDrift := printInspectReport("master-1", state, rows)
	if !hasDrift {
		t.Error("expected hasDrift=true for row with key_mismatch, got false")
	}
}
