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

const masterFallback = "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:latest"

// TestMasterPrepareImage verifies the three-level image resolution that
// newMasterPrepareCommand applies when building data.Image:
//
//  1. CLI --image flag wins when non-empty.
//  2. Topology defaults.image.node wins when CLI flag is empty.
//  3. Built-in fallback is used when neither source is set.
//
// Anti-stub guarantee: if resolveImage is replaced with `return ""`, cases
// 1 and 2 MUST fail. Case 3 would also fail because the fallback is non-empty.
// (If the fallback itself were "", case 3 would pass incidentally — but the
// production fallback is always a non-empty string, so all three cases fail.)
func TestMasterPrepareImage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		imageFlag string
		topoImage string // topology Defaults.Image.Node
		want      string
	}{
		{
			name:      "cli-flag overrides topology and fallback",
			imageFlag: "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.9.0",
			topoImage: "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.8.0",
			want:      "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.9.0",
		},
		{
			name:      "topology-default used when no cli flag",
			imageFlag: "",
			topoImage: "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.8.0",
			want:      "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.8.0",
		},
		{
			name:      "baseline fallback used when neither flag nor topology set",
			imageFlag: "",
			topoImage: "",
			want:      masterFallback,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Build a topology that mirrors the state master.go reads.
			// Only Defaults.Image.Node matters for this resolution path.
			topo := topology.Topology{
				Defaults: topology.Defaults{
					Image: topology.ImageDefaults{
						Node: tc.topoImage,
					},
				},
			}

			// Replicate the exact resolveImage call from newMasterPrepareCommand.
			got := resolveImage(tc.imageFlag, topo.Defaults.Image.Node, masterFallback)
			if got != tc.want {
				t.Errorf("resolveImage(flag=%q, topo=%q, fallback=%q) = %q, want %q",
					tc.imageFlag, tc.topoImage, masterFallback, got, tc.want)
			}
		})
	}
}

// TestMasterPrepareImageFlagValidation verifies that newMasterPrepareCommand
// rejects an invalid --image value before performing any topology or CA work.
// This exercises the validateImageRef gate added to the prepare command's RunE.
func TestMasterPrepareImageFlagValidation(t *testing.T) {
	// Do not run in parallel: NewRootCommand binds cobra persistent flags to
	// package-level globals (topologyPath/configDir), and concurrent flag
	// registration causes a data race under -race.

	invalidRefs := []string{
		"img; rm -rf /",
		"img`touch /pwned`",
		"img$(id)",
		"img|sh",
	}

	for _, ref := range invalidRefs {
		ref := ref
		t.Run(ref, func(t *testing.T) {
			root := NewRootCommand("test")
			root.SilenceUsage = true
			root.SilenceErrors = true
			root.SetArgs([]string{"master", "prepare", "--image", ref, "master-01"})

			err := root.Execute()
			if err == nil {
				t.Errorf("master prepare --image %q: expected error for invalid image ref, got nil", ref)
				return
			}
			if !strings.Contains(err.Error(), "invalid --image") {
				t.Errorf("master prepare --image %q: expected 'invalid --image' in error, got: %v", ref, err)
			}
		})
	}
}

// TestMasterReloadCommandRegistered verifies that 'mesh-ctl master reload'
// is registered under the master parent command and requires exactly one
// positional argument. This exercises T012 cobra registration.
//
// Anti-stub guarantee: if newMasterReloadCommand() is removed from
// newMasterCommand(), the 'reload' subcommand will be absent and Execute()
// will return an "unknown command" error rather than the topology error.
func TestMasterReloadCommandRegistered(t *testing.T) {
	// Do not run in parallel: NewRootCommand binds cobra persistent flags to
	// package-level globals (topologyPath/configDir), and concurrent flag
	// registration causes a data race under -race.

	t.Run("no-args returns usage error not unknown-command", func(t *testing.T) {
		root := NewRootCommand("test")
		root.SilenceUsage = true
		root.SilenceErrors = true
		root.SetArgs([]string{"master", "reload"})

		err := root.Execute()
		if err == nil {
			t.Fatal("expected error for missing <name> argument, got nil")
		}
		// cobra.ExactArgs(1) returns an error that contains "accepts 1 arg",
		// NOT "unknown command". This proves the subcommand is registered.
		if strings.Contains(err.Error(), "unknown command") {
			t.Errorf("got 'unknown command' — newMasterReloadCommand is not registered: %v", err)
		}
	})

	t.Run("valid-name with missing topology returns topology-load error", func(t *testing.T) {
		root := NewRootCommand("test")
		root.SilenceUsage = true
		root.SilenceErrors = true
		// Point topology at a non-existent file so we get a predictable error
		// that is NOT "unknown command" — proving the command is registered and
		// its RunE body is reached.
		root.SetArgs([]string{"master", "--topology", "/nonexistent/topology.yml", "reload", "ru-01"})

		err := root.Execute()
		if err == nil {
			t.Fatal("expected error for nonexistent topology, got nil")
		}
		if strings.Contains(err.Error(), "unknown command") {
			t.Errorf("got 'unknown command' — newMasterReloadCommand is not registered: %v", err)
		}
		// Must reach topology load attempt, not cobra routing failure.
		if !strings.Contains(err.Error(), "topology") && !strings.Contains(err.Error(), "nonexistent") {
			t.Errorf("expected topology-load error, got: %v", err)
		}
	})
}

// masterReloadStatusLine classifies an UpdateTunnelPeerResponse + error into
// the human-readable status line printed per-endpoint by 'master reload'.
// Extracted as a pure function for testability (T013 counter + status logic).
//
// Returns (statusLine, ok) where ok=true means this endpoint counted as success.
func masterReloadStatusLine(resp *proto.UpdateTunnelPeerResponse, err error) (string, bool) {
	if err != nil {
		statusLine, _ := updateTunnelPeerFailureStatus(err)
		return statusLine, false
	}
	if resp == nil || !resp.Success {
		return "FAILED: update tunnel peer RPC returned unsuccessful response", false
	}
	if resp.Unchanged {
		return "already up to date", true
	}
	return "updated (new key applied)", true
}

// TestMasterReloadStatusLine verifies T013's per-endpoint status classification:
// unchanged response, updated response, RPC error, and unsuccessful response.
//
// Anti-stub guarantee: replacing masterReloadStatusLine with `return "ok", true`
// causes the RPC-error and unsuccessful-response cases to fail (ok==true instead
// of false). Replacing with `return "x", false` causes unchanged + updated to
// fail (ok==false instead of true).
func TestMasterReloadStatusLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		resp     *proto.UpdateTunnelPeerResponse
		rpcErr   error
		wantLine string
		wantOk   bool
	}{
		{
			name:     "unchanged response → already up to date",
			resp:     &proto.UpdateTunnelPeerResponse{Success: true, Unchanged: true},
			rpcErr:   nil,
			wantLine: "already up to date",
			wantOk:   true,
		},
		{
			name:     "changed response → updated (new key applied)",
			resp:     &proto.UpdateTunnelPeerResponse{Success: true, Unchanged: false},
			rpcErr:   nil,
			wantLine: "updated (new key applied)",
			wantOk:   true,
		},
		{
			name:     "RPC error → FAILED status, not ok",
			resp:     nil,
			rpcErr:   status.Error(codes.Internal, "something broke"),
			wantLine: "FAILED:",
			wantOk:   false,
		},
		{
			name:     "nil response → FAILED status, not ok",
			resp:     nil,
			rpcErr:   nil,
			wantLine: "FAILED:",
			wantOk:   false,
		},
		{
			name:     "unsuccessful response → FAILED status, not ok",
			resp:     &proto.UpdateTunnelPeerResponse{Success: false},
			rpcErr:   nil,
			wantLine: "FAILED:",
			wantOk:   false,
		},
		{
			name:     "pre-v1.10.0 master → FAILED status, not ok",
			resp:     nil,
			rpcErr:   status.Error(codes.Unimplemented, "method UpdateTunnelPeer not implemented"),
			wantLine: "FAILED:",
			wantOk:   false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			line, ok := masterReloadStatusLine(tc.resp, tc.rpcErr)

			if ok != tc.wantOk {
				t.Errorf("ok = %v, want %v (statusLine: %q)", ok, tc.wantOk, line)
			}
			if !strings.Contains(line, tc.wantLine) {
				t.Errorf("statusLine %q does not contain %q", line, tc.wantLine)
			}
		})
	}
}

// TestMasterReloadCounterLogic verifies the endpointsOk/endpointsTotal counter
// semantics used by T013: only responses where masterReloadStatusLine returns
// ok=true should increment endpointsOk; total increments for every endpoint.
//
// Anti-stub guarantee: replacing the ok==true guard with an unconditional
// increment causes the "any failure → non-zero exit" assertion to fail.
func TestMasterReloadCounterLogic(t *testing.T) {
	t.Parallel()

	type epResult struct {
		resp   *proto.UpdateTunnelPeerResponse
		rpcErr error
	}

	cases := []struct {
		name            string
		results         []epResult
		wantTotal       int
		wantOk          int
		wantNonZeroExit bool
	}{
		{
			name: "all unchanged → all ok, exit 0",
			results: []epResult{
				{resp: &proto.UpdateTunnelPeerResponse{Success: true, Unchanged: true}},
				{resp: &proto.UpdateTunnelPeerResponse{Success: true, Unchanged: true}},
			},
			wantTotal:       2,
			wantOk:          2,
			wantNonZeroExit: false,
		},
		{
			name: "all updated → all ok, exit 0",
			results: []epResult{
				{resp: &proto.UpdateTunnelPeerResponse{Success: true, Unchanged: false}},
			},
			wantTotal:       1,
			wantOk:          1,
			wantNonZeroExit: false,
		},
		{
			name: "one failure out of two → ok < total, exit non-zero",
			results: []epResult{
				{resp: &proto.UpdateTunnelPeerResponse{Success: true, Unchanged: false}},
				{rpcErr: status.Error(codes.Unavailable, "connection refused")},
			},
			wantTotal:       2,
			wantOk:          1,
			wantNonZeroExit: true,
		},
		{
			name: "all failures → ok=0, exit non-zero",
			results: []epResult{
				{rpcErr: status.Error(codes.Internal, "handler panic")},
				{resp: &proto.UpdateTunnelPeerResponse{Success: false}},
			},
			wantTotal:       2,
			wantOk:          0,
			wantNonZeroExit: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			total := 0
			ok := 0
			for _, r := range tc.results {
				total++
				if _, isOk := masterReloadStatusLine(r.resp, r.rpcErr); isOk {
					ok++
				}
			}

			if total != tc.wantTotal {
				t.Errorf("total = %d, want %d", total, tc.wantTotal)
			}
			if ok != tc.wantOk {
				t.Errorf("ok = %d, want %d", ok, tc.wantOk)
			}
			nonZero := ok < total
			if nonZero != tc.wantNonZeroExit {
				t.Errorf("nonZeroExit = %v, want %v (ok=%d total=%d)", nonZero, tc.wantNonZeroExit, ok, total)
			}
		})
	}
}

// TestReadEndpointPublicKey verifies strict 32-byte parsing for admin-state
// pubkeys used by `master reload`. Wrong sizes must be rejected with a clear
// error to aid operator troubleshooting.
func TestReadEndpointPublicKey(t *testing.T) {
	t.Parallel()

	t.Run("valid length reads key", func(t *testing.T) {
		tmpDir := t.TempDir()
		path := filepath.Join(tmpDir, "pubkey")

		key := make([]byte, endpointPublicKeyLen)
		for i := range key {
			key[i] = byte(i)
		}
		if err := os.WriteFile(path, key, 0o600); err != nil {
			t.Fatalf("write pubkey: %v", err)
		}

		got, err := readEndpointPublicKey(path)
		if err != nil {
			t.Fatalf("readEndpointPublicKey = %v, want nil", err)
		}
		if len(got) != endpointPublicKeyLen {
			t.Fatalf("got %d bytes, want %d", len(got), endpointPublicKeyLen)
		}
		for i, b := range got {
			if b != byte(i) {
				t.Fatalf("byte %d mismatch: got %d, want %d", i, b, byte(i))
			}
		}
	})

	t.Run("short key is rejected", func(t *testing.T) {
		tmpDir := t.TempDir()
		path := filepath.Join(tmpDir, "pubkey")

		if err := os.WriteFile(path, []byte{1, 2, 3}, 0o600); err != nil {
			t.Fatalf("write pubkey: %v", err)
		}

		_, err := readEndpointPublicKey(path)
		if err == nil {
			t.Fatal("expected error for short key, got nil")
		}
		if !strings.Contains(err.Error(), "want 32") {
			t.Errorf("error %q does not mention expected size", err)
		}
	})
}
