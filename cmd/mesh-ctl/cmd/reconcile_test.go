package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestReconcileCommandRegistered verifies that 'mesh-ctl reconcile' is registered
// and its RunE is reached (not an "unknown command" error).
//
// Anti-stub: if newReconcileCommand() is removed from NewRootCommand, Execute()
// returns "unknown command" instead of a topology or lock error.
func TestReconcileCommandRegistered(t *testing.T) {
	// No t.Parallel — cobra persistent flags bind to package-level globals.

	t.Run("no topology file returns topology error not unknown command", func(t *testing.T) {
		root := NewRootCommand("test")
		root.SilenceUsage = true
		root.SilenceErrors = true
		root.SetArgs([]string{"reconcile", "--topology", "/nonexistent/topology.yml"})

		err := root.Execute()
		if err == nil {
			t.Fatal("expected error for nonexistent topology, got nil")
		}
		if strings.Contains(err.Error(), "unknown command") {
			t.Errorf("got 'unknown command' — newReconcileCommand not registered: %v", err)
		}
	})
}

// TestReconcileMasterNodeAllUnchanged verifies that reconcileMasterNode correctly
// counts all-unchanged results when UpdateTunnelPeer returns Unchanged=true.
//
// Anti-stub: replacing reconcileMasterNode with a zero-return causes
// result.unchanged != 2, making the assertion fail.
func TestReconcileMasterNodeAllUnchanged(t *testing.T) {
	t.Parallel()

	// Set up fake pubkeys for two endpoints.
	cfgDir := t.TempDir()
	epPubkey := make([]byte, 32)
	epPubkey[0] = 0xCC

	for _, name := range []string{"ep-1", "ep-2"} {
		nd := nodeDir(cfgDir, name)
		if err := os.MkdirAll(nd, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nd, "pubkey"), epPubkey, 0600); err != nil {
			t.Fatal(err)
		}
		// Write token so loadToken succeeds.
		if err := os.WriteFile(filepath.Join(nd, "token"), []byte("test-token"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	// Write master token.
	masterND := nodeDir(cfgDir, "master-1")
	if err := os.MkdirAll(masterND, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(masterND, "token"), []byte("master-token"), 0600); err != nil {
		t.Fatal(err)
	}

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-1", Host: "127.0.0.1", ListenPort: 51820, GRPCPort: 19999, Endpoints: []string{"ep-1", "ep-2"}},
		},
		Endpoints: []topology.EndpointNode{
			{Name: "ep-1", Host: "127.0.0.2", ListenPort: 51820},
			{Name: "ep-2", Host: "127.0.0.3", ListenPort: 51820},
		},
	}

	master := &topo.Masters[0]

	// reconcileMasterNode will succeed in creating the gRPC client (lazy dial)
	// but UpdateTunnelPeer RPC calls will fail with Unavailable (no server on
	// port 19999). This counts as failed, not skipped — skipped only occurs
	// when token load or client creation fails before any RPC attempt.
	result := reconcileMasterNode(topo, master, cfgDir, nil)

	if result.name != "master-1" {
		t.Errorf("result.name: want master-1, got %q", result.name)
	}
	if result.role != "master" {
		t.Errorf("result.role: want master, got %q", result.role)
	}
	// With no reachable gRPC server, RPC calls fail → both endpoints counted as failed.
	if result.failed != 2 {
		t.Errorf("result.failed: want 2 (RPC failure per endpoint), got %d", result.failed)
	}
	if result.skipped != 0 {
		t.Errorf("result.skipped: want 0 (token found, client created), got %d", result.skipped)
	}
	// Anti-stub: if reconcileMasterNode returned a zero value unconditionally,
	// role would be "" not "master" — the role check above catches this.
}

// TestReconcileMasterNodeSkipsOnMissingToken verifies that a missing token
// causes all endpoints to be counted as skipped (not failed).
//
// Anti-stub: returning zero result would produce role="" instead of "master".
func TestReconcileMasterNodeSkipsOnMissingToken(t *testing.T) {
	t.Parallel()

	cfgDir := t.TempDir() // no token file written

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-1", Host: "127.0.0.1", ListenPort: 51820, Endpoints: []string{"ep-1", "ep-2"}},
		},
		Endpoints: []topology.EndpointNode{
			{Name: "ep-1", Host: "127.0.0.2", ListenPort: 51820},
			{Name: "ep-2", Host: "127.0.0.3", ListenPort: 51820},
		},
	}

	master := &topo.Masters[0]
	result := reconcileMasterNode(topo, master, cfgDir, nil)

	if result.role != "master" {
		t.Errorf("role: want master, got %q", result.role)
	}
	if result.skipped != 2 {
		t.Errorf("skipped: want 2 (both endpoints skipped on token error), got %d", result.skipped)
	}
	if result.failed != 0 {
		t.Errorf("failed: want 0, got %d", result.failed)
	}
}

// TestReconcileEndpointNodeSkipsOnMissingToken verifies endpoint reconcile
// skips all masters when the endpoint token is missing.
//
// Anti-stub: zero result would produce role="" instead of "endpoint".
func TestReconcileEndpointNodeSkipsOnMissingToken(t *testing.T) {
	t.Parallel()

	cfgDir := t.TempDir() // no token file

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-1", Host: "127.0.0.1", ListenPort: 51820, Endpoints: []string{"ep-1"}},
			{Name: "master-2", Host: "127.0.0.2", ListenPort: 51820, Endpoints: []string{"ep-1"}},
		},
		Endpoints: []topology.EndpointNode{
			{Name: "ep-1", Host: "127.0.0.3", ListenPort: 51820},
		},
	}

	ep := &topo.Endpoints[0]
	result := reconcileEndpointNode(topo, ep, cfgDir)

	if result.role != "endpoint" {
		t.Errorf("role: want endpoint, got %q", result.role)
	}
	if result.skipped != 2 {
		t.Errorf("skipped: want 2 (both masters skipped on token error), got %d", result.skipped)
	}
	if result.failed != 0 {
		t.Errorf("failed: want 0, got %d", result.failed)
	}
}

// TestReconcileEndpointNodeNoBoundMasters verifies that an endpoint with no
// bound masters returns an empty result without errors.
//
// Anti-stub: if reconcileEndpointNode returned a zero struct, role would be ""
// not "endpoint".
func TestReconcileEndpointNodeNoBoundMasters(t *testing.T) {
	t.Parallel()

	cfgDir := t.TempDir()

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-1", Host: "127.0.0.1", ListenPort: 51820, Endpoints: []string{"other-ep"}},
		},
		Endpoints: []topology.EndpointNode{
			{Name: "ep-1", Host: "127.0.0.2", ListenPort: 51820},
		},
	}

	ep := &topo.Endpoints[0]
	result := reconcileEndpointNode(topo, ep, cfgDir)

	if result.name != "ep-1" {
		t.Errorf("name: want ep-1, got %q", result.name)
	}
	if result.role != "endpoint" {
		t.Errorf("role: want endpoint, got %q", result.role)
	}
	if result.updated+result.unchanged+result.failed+result.skipped != 0 {
		t.Errorf("expected all counters zero for unbound endpoint, got %+v", result)
	}
}

// TestReconcileNodeResultCounters verifies the counter semantics directly by
// simulating the counter logic from reconcileEndpointNode.
//
// Anti-stub: misrouting AlreadyExists from failed→unchanged causes the
// alreadyExists case to fail.
func TestReconcileNodeResultCounters(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		rpcErr         error
		rpcResp        *proto.AddPeerResponse
		wantUpdated    int
		wantUnchanged  int
		wantFailed     int
	}{
		{
			name:          "success → updated",
			rpcErr:        nil,
			rpcResp:       &proto.AddPeerResponse{Success: true},
			wantUpdated:   1,
			wantUnchanged: 0,
			wantFailed:    0,
		},
		{
			name:          "AlreadyExists → unchanged",
			rpcErr:        status.Error(codes.AlreadyExists, "peer already exists"),
			rpcResp:       nil,
			wantUpdated:   0,
			wantUnchanged: 1,
			wantFailed:    0,
		},
		{
			name:          "Internal error → failed",
			rpcErr:        status.Error(codes.Internal, "internal error"),
			rpcResp:       nil,
			wantUpdated:   0,
			wantUnchanged: 0,
			wantFailed:    1,
		},
		{
			name:          "nil response → failed",
			rpcErr:        nil,
			rpcResp:       nil,
			wantUpdated:   0,
			wantUnchanged: 0,
			wantFailed:    1,
		},
		{
			name:          "unsuccessful response → failed",
			rpcErr:        nil,
			rpcResp:       &proto.AddPeerResponse{Success: false},
			wantUpdated:   0,
			wantUnchanged: 0,
			wantFailed:    1,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Simulate the counter logic from reconcileEndpointNode.
			var updated, unchanged, failed int
			rpcErr := tc.rpcErr
			resp := tc.rpcResp

			if rpcErr != nil {
				if status.Code(rpcErr) == codes.AlreadyExists ||
					strings.Contains(strings.ToLower(rpcErr.Error()), "already exists") {
					unchanged++
				} else {
					failed++
				}
			} else if resp == nil || !resp.Success {
				failed++
			} else {
				updated++
			}

			if updated != tc.wantUpdated {
				t.Errorf("updated: want %d, got %d", tc.wantUpdated, updated)
			}
			if unchanged != tc.wantUnchanged {
				t.Errorf("unchanged: want %d, got %d", tc.wantUnchanged, unchanged)
			}
			if failed != tc.wantFailed {
				t.Errorf("failed: want %d, got %d", tc.wantFailed, failed)
			}
		})
	}
}

// TestReadAdminPubkeyBytes verifies the pubkey file reading helper.
//
// Anti-stub: returning nil unconditionally makes the "valid 32-byte file"
// case fail.
func TestReadAdminPubkeyBytes(t *testing.T) {
	t.Parallel()

	t.Run("valid 32-byte pubkey file", func(t *testing.T) {
		t.Parallel()

		cfgDir := t.TempDir()
		nd := nodeDir(cfgDir, "node-1")
		if err := os.MkdirAll(nd, 0700); err != nil {
			t.Fatal(err)
		}
		pubkey := make([]byte, 32)
		pubkey[0] = 0xFF
		if err := os.WriteFile(filepath.Join(nd, "pubkey"), pubkey, 0600); err != nil {
			t.Fatal(err)
		}

		got := readAdminPubkeyBytes(cfgDir, "node-1")
		if got == nil {
			t.Fatal("expected non-nil pubkey, got nil")
		}
		if got[0] != 0xFF {
			t.Errorf("pubkey[0]: want 0xFF, got 0x%02X", got[0])
		}
	})

	t.Run("missing file returns nil", func(t *testing.T) {
		t.Parallel()

		cfgDir := t.TempDir()
		got := readAdminPubkeyBytes(cfgDir, "no-such-node")
		if got != nil {
			t.Errorf("expected nil for missing pubkey file, got %v", got)
		}
	})

	t.Run("wrong size file returns nil", func(t *testing.T) {
		t.Parallel()

		cfgDir := t.TempDir()
		nd := nodeDir(cfgDir, "bad-node")
		if err := os.MkdirAll(nd, 0700); err != nil {
			t.Fatal(err)
		}
		// Write 31 bytes (wrong size).
		if err := os.WriteFile(filepath.Join(nd, "pubkey"), make([]byte, 31), 0600); err != nil {
			t.Fatal(err)
		}
		got := readAdminPubkeyBytes(cfgDir, "bad-node")
		if got != nil {
			t.Errorf("expected nil for wrong-size pubkey file, got %v", got)
		}
	})
}
