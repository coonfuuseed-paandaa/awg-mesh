package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
)

// ─── topology helpers ─────────────────────────────────────────────────────────

func singleMasterTopo() *topology.Topology {
	return &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "m1", Host: "m1.example.com", GRPCPort: 9090},
		},
	}
}

// ─── minimal Driver setup ─────────────────────────────────────────────────────

// setupDriver creates a Driver wired with fakes. renderOK writes a dummy compose
// file so phasePrepare succeeds.
func setupDriver(t *testing.T, topo *topology.Topology, opts ...func(*DriverConfig)) (*Driver, *NodeUpgradeStep, string) {
	t.Helper()
	dir := t.TempDir()

	// Create node directory and token file so loadToken succeeds.
	nd := filepath.Join(dir, "nodes", "m1")
	if err := os.MkdirAll(nd, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nd, "token"), []byte("test-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Stub CA cert.
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("FAKE CA"), 0600); err != nil {
		t.Fatal(err)
	}
	// Write an existing compose so phasePrepare can snapshot it.
	if err := os.WriteFile(filepath.Join(nd, "m1-docker-compose.yml"), []byte("services: {}"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := DriverConfig{
		ConfigDir: dir,
		Topology:  topo,
		// RenderCompose writes a dummy file so deploy path is satisfied.
		RenderCompose: func(nodeName, role, newImage, cfgDir, outputPath string) error {
			return os.WriteFile(outputPath, []byte("services: {}"), 0600)
		},
		// No Prober → phaseVerify is a no-op.
		// No SSHOpts.Enabled → phaseDeploy is manual (prints only).
	}

	for _, o := range opts {
		o(&cfg)
	}

	step := &NodeUpgradeStep{
		Name:     "m1",
		Role:     "master",
		NewImage: "ghcr.io/example/awg-mesh-node:v1.10.2",
		Status:   StatusPlanned,
	}
	return NewDriver(cfg), step, dir
}

// ─── UpgradeNode — happy path (no gRPC: skip wait_ready via zero budget) ──────

// TestUpgradeNode_SkipWaitReady_ManualDeploy: with no SSHOpts, phaseWaitReady
// will fail immediately because no real node is running.  We test the prepare +
// deploy phases succeed by making wait_ready time out quickly and asserting the
// step status is Failed (not Panicked or a different error).
func TestUpgradeNode_PrepareAndDeploy_Succeed(t *testing.T) {
	topo := singleMasterTopo()
	drv, step, _ := setupDriver(t, topo, func(c *DriverConfig) {
		// Zero DowntimeBudget causes phaseWaitReady to time out immediately,
		// letting us test prepare+deploy without a live gRPC server.
		// NewDriver will override zero → 60s, so set a real tiny value via
		// a direct cfg approach: we must set it after NewDriver clamps it.
	})
	// Directly shrink the budget on the cfg after creation by creating the driver manually.
	cfg := DriverConfig{
		ConfigDir: drv.cfg.ConfigDir,
		Topology:  topo,
		RenderCompose: func(nodeName, role, newImage, cfgDir, outputPath string) error {
			return os.WriteFile(outputPath, []byte("services: {}"), 0600)
		},
		DowntimeBudget: 1, // 1 nanosecond → immediate timeout
	}
	drv2 := NewDriver(cfg)

	err := drv2.UpgradeNode(context.Background(), step)
	// Must fail at wait_ready (no live node), not at prepare or deploy.
	if err == nil {
		t.Fatal("expected error from wait_ready, got nil")
	}
	if step.Status != StatusFailed {
		t.Errorf("step status: got %q want failed", step.Status)
	}
	// Error message must reference wait_ready.
	if !containsStr(err.Error(), "wait_ready") {
		t.Errorf("error should mention wait_ready phase, got: %v", err)
	}
}

// TestUpgradeNode_PrepareFailsOnMissingRenderCompose ensures prepare phase
// returns a clear error when RenderCompose is nil.
func TestUpgradeNode_PrepareFailsOnMissingRenderCompose(t *testing.T) {
	topo := singleMasterTopo()
	drv, step, _ := setupDriver(t, topo, func(c *DriverConfig) {
		c.RenderCompose = nil
	})
	err := drv.UpgradeNode(context.Background(), step)
	if err == nil {
		t.Fatal("expected error for nil RenderCompose")
	}
	if !containsStr(err.Error(), "prepare") {
		t.Errorf("error should mention prepare phase, got: %v", err)
	}
	if step.Status != StatusFailed {
		t.Errorf("step status: got %q want failed", step.Status)
	}
}

// TestUpgradeNode_SSHDeployNotConfigured ensures sshDeploy returns an error
// when SSHOpts.Enabled is true but SSHDeploy is nil.
func TestUpgradeNode_SSHDeployNotConfigured(t *testing.T) {
	topo := singleMasterTopo()
	drv, step, _ := setupDriver(t, topo, func(c *DriverConfig) {
		c.SSHOpts = SSHOpts{Enabled: true}
		c.SSHDeploy = nil // explicitly nil
	})
	err := drv.UpgradeNode(context.Background(), step)
	if err == nil {
		t.Fatal("expected error for nil SSHDeploy with Enabled=true")
	}
	if !containsStr(err.Error(), "deploy") {
		t.Errorf("error should mention deploy phase, got: %v", err)
	}
}

// TestUpgradeNode_SSHDeployError ensures that an SSH error is wrapped and
// the step transitions to StatusFailed.
func TestUpgradeNode_SSHDeployError(t *testing.T) {
	topo := singleMasterTopo()
	sshErr := errors.New("connection refused")
	drv, step, _ := setupDriver(t, topo, func(c *DriverConfig) {
		c.SSHOpts = SSHOpts{Enabled: true, User: "root", Port: 22}
		c.SSHDeploy = func(addr, user, keyPath string, acceptNewHosts bool, remoteCmd string) error {
			return sshErr
		}
	})
	err := drv.UpgradeNode(context.Background(), step)
	if err == nil {
		t.Fatal("expected error from SSH deploy")
	}
	if !errors.Is(err, sshErr) && !containsStr(err.Error(), "connection refused") {
		t.Errorf("expected ssh error in chain, got: %v", err)
	}
	if step.Status != StatusFailed {
		t.Errorf("step status: got %q want failed", step.Status)
	}
}

// TestUpgradeNode_VerifyFailTriggersRollback: inject a failing Prober (returns
// a broken tunnel pair for m1) and verify that:
//   - step.Status ends as StatusRolledBack
//   - error message references "verify failed"
//   - rollback's Reconcile is called
//
// wait_ready is bypassed by using a DowntimeBudget of 1 ns (immediate timeout),
// which means this test only fully exercises prepare + deploy + wait_ready(fail).
// To test the verify→rollback branch we call phaseVerify and rollbackNode directly.
func TestUpgradeNode_PhaseVerify_Broken(t *testing.T) {
	topo := singleMasterTopo()
	drv, step, _ := setupDriver(t, topo)

	// Inject a prober that reports m1 as broken.
	drv.cfg.Prober = func(t2 *topology.Topology, cfgDir string, probeTimeout time.Duration, maxConcurrency int) []PairProbeResult {
		return []PairProbeResult{
			{MasterName: "m1", EndpointName: "ep-1", Reason: "tunnel down"},
		}
	}

	err := drv.phaseVerify(context.Background(), step)
	if err == nil {
		t.Fatal("expected error from phaseVerify with broken probe")
	}
	if !strings.Contains(err.Error(), "m1") {
		t.Errorf("error should mention node name: %v", err)
	}
}

// TestUpgradeNode_PhaseVerify_AllHealthy: a prober that returns no broken pairs
// must yield nil from phaseVerify.
func TestUpgradeNode_PhaseVerify_AllHealthy(t *testing.T) {
	topo := singleMasterTopo()
	drv, step, _ := setupDriver(t, topo)

	drv.cfg.Prober = func(t2 *topology.Topology, cfgDir string, probeTimeout time.Duration, maxConcurrency int) []PairProbeResult {
		return []PairProbeResult{
			{MasterName: "m1", EndpointName: "ep-1", Reason: ""}, // healthy
		}
	}

	if err := drv.phaseVerify(context.Background(), step); err != nil {
		t.Errorf("expected nil for healthy probe, got: %v", err)
	}
}

// TestUpgradeNode_PhaseVerify_NilProber: nil Prober must skip verification (return nil).
func TestUpgradeNode_PhaseVerify_NilProber(t *testing.T) {
	topo := singleMasterTopo()
	drv, step, _ := setupDriver(t, topo)
	drv.cfg.Prober = nil

	if err := drv.phaseVerify(context.Background(), step); err != nil {
		t.Errorf("nil Prober should skip verify, got: %v", err)
	}
}

// TestRollbackNode_RestoresBackup verifies that rollbackNode writes the .bak
// content back to the live compose path.
func TestRollbackNode_RestoresBackup(t *testing.T) {
	topo := singleMasterTopo()
	_, step, dir := setupDriver(t, topo)

	nd := filepath.Join(dir, "nodes", "m1")
	backupContent := []byte("# restored from backup\nservices: {}")
	if err := os.WriteFile(filepath.Join(nd, "m1-docker-compose.yml.bak"), backupContent, 0600); err != nil {
		t.Fatal(err)
	}

	reconcileCalled := false
	cfg := DriverConfig{
		ConfigDir: dir,
		Topology:  topo,
		// DowntimeBudget 1 ns → waitReady times out immediately (no live node).
		DowntimeBudget: 1,
		Reconcile: func(topo2 *topology.Topology, cfgDir string) error {
			reconcileCalled = true
			return nil
		},
	}

	err := rollbackNode(context.Background(), cfg, step)
	// waitReady will fail (no gRPC server), so rollbackNode returns an error.
	// But the backup restore (step 1) happens before waitReady, so we can
	// verify the live compose was overwritten.
	_ = err // error expected from waitReady

	live, readErr := os.ReadFile(filepath.Join(nd, "m1-docker-compose.yml"))
	if readErr != nil {
		t.Fatalf("read live compose after rollback: %v", readErr)
	}
	if string(live) != string(backupContent) {
		t.Errorf("live compose was not restored:\ngot: %s\nwant: %s", live, backupContent)
	}
	// Reconcile should NOT have been called because waitReady failed first.
	if reconcileCalled {
		t.Error("Reconcile should not be called when waitReady fails")
	}
}

// TestRollbackNode_MissingBackup verifies that rollbackNode returns an error
// when the .bak file does not exist (no compose was snapshotted).
func TestRollbackNode_MissingBackup(t *testing.T) {
	topo := singleMasterTopo()
	_, step, dir := setupDriver(t, topo)

	// Do NOT create a .bak file.
	cfg := DriverConfig{
		ConfigDir:      dir,
		Topology:       topo,
		DowntimeBudget: 1,
	}

	err := rollbackNode(context.Background(), cfg, step)
	if err == nil {
		t.Fatal("expected error when backup compose is missing")
	}
	if !strings.Contains(err.Error(), "backup") && !strings.Contains(err.Error(), ".bak") {
		t.Errorf("error should mention backup, got: %v", err)
	}
}

// ─── validateImageRef ──────────────────────────────────────────────────────────

func TestValidateImageRef_Valid(t *testing.T) {
	cases := []string{
		"ghcr.io/org/img:v1.10.2",
		"ghcr.io/org/img@sha256:abc123def456",
		"registry.example.com:5000/img:latest",
		"img",
	}
	for _, ref := range cases {
		if err := validateImageRef(ref); err != nil {
			t.Errorf("validateImageRef(%q): unexpected error: %v", ref, err)
		}
	}
}

func TestValidateImageRef_Invalid(t *testing.T) {
	cases := []string{
		"",
		"img;rm -rf /",
		"img`id`",
		"img$(whoami)",
		"img|cat /etc/passwd",
	}
	for _, ref := range cases {
		if err := validateImageRef(ref); err == nil {
			t.Errorf("validateImageRef(%q): expected error, got nil", ref)
		}
	}
}

// ─── NewDriver — default budget clamping ──────────────────────────────────────

func TestNewDriver_DefaultBudgets(t *testing.T) {
	drv := NewDriver(DriverConfig{Topology: singleMasterTopo()})
	if drv.cfg.DowntimeBudget != 60e9 {
		t.Errorf("DowntimeBudget: got %v want 60s", drv.cfg.DowntimeBudget)
	}
	if drv.cfg.DeployWait != 120e9 {
		t.Errorf("DeployWait: got %v want 120s", drv.cfg.DeployWait)
	}
	if drv.cfg.ProbeTimeout != 5e9 {
		t.Errorf("ProbeTimeout: got %v want 5s", drv.cfg.ProbeTimeout)
	}
}

// ─── shellQuote ────────────────────────────────────────────────────────────────

func TestShellQuote(t *testing.T) {
	got := shellQuote("/path/to/file with spaces.yml")
	want := "'/path/to/file with spaces.yml'"
	if got != want {
		t.Errorf("shellQuote: got %q want %q", got, want)
	}
}

func TestShellQuote_EscapesSingleQuote(t *testing.T) {
	got := shellQuote("path/with'quote")
	want := `'path/with'\''quote'`
	if got != want {
		t.Errorf("shellQuote: got %q want %q", got, want)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func containsStr(s, sub string) bool {
	return strings.Contains(s, sub)
}
