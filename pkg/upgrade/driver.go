package upgrade

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	grpcclient "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
)

// SSHOpts configures the optional SSH-based deploy trigger.
// When Enabled is false, the driver prints the compose path and polls gRPC Ready.
type SSHOpts struct {
	Enabled        bool
	User           string
	Port           int
	KeyPath        string
	Passphrase     string // --ssh-passphrase or MESH_SSH_KEY_PASSPHRASE env var
	AcceptNewHosts bool
}

// SSHDeployer executes a remote shell command via SSH and returns an error on
// non-zero exit or connection failure.  The signature matches the behaviour of
// runSSHCommand in bootstrap.go.  Inject a real implementation from the cmd
// package to avoid a circular import; inject a fake in tests.
//
// Parameters:
//
//	addr — "host:port"
//	user — SSH username (e.g. "root")
//	keyPath — path to the private key file; empty = use ssh-agent
//	acceptNewHosts — when true, unknown host keys are accepted (TOFU)
//	remoteCmd — shell command to execute on the remote host
type SSHDeployer func(addr, user, keyPath string, acceptNewHosts bool, remoteCmd string) error

// SSHUploader uploads a local file to a remote path via SFTP over an existing SSH connection,
// then executes remoteCmd on the same host. This avoids a second TCP connection.
//
// Parameters:
//
//	addr          — "host:port"
//	user          — SSH username
//	keyPath       — path to private key; empty = ssh-agent
//	acceptNewHosts — TOFU for unknown host keys
//	adminPath     — local (admin-side) file path to upload
//	remotePath    — remote absolute path to write (parent dir created if missing)
//	remoteCmd     — shell command to execute after upload
type SSHUploader func(addr, user, keyPath string, acceptNewHosts bool, adminPath, remotePath, remoteCmd string) error

// DataPlaneProber is a function type that matches the signature of
// runDataPlaneProbes from cmd/mesh-ctl/cmd/status.go.  Using a function type
// here lets tests inject a fake implementation without a full gRPC server.
type DataPlaneProber func(
	topo *topology.Topology,
	cfgDir string,
	probeTimeout time.Duration,
	maxConcurrency int,
) []PairProbeResult

// PairProbeResult mirrors pairProbeResult from status.go but is exported so
// the driver can reference it without a circular import.
type PairProbeResult struct {
	MasterName   string
	EndpointName string
	Reason       string // empty = healthy
}

// Reconciler is a function type that reconciles a single master node.
// Injected by the upgrade command so the driver can call reconcile logic
// without importing the cmd package.
type Reconciler func(
	topo *topology.Topology,
	cfgDir string,
) error

// ComposeRenderer generates a docker-compose file for a node and writes it to
// outputPath.  Injected by the upgrade command; lets tests replace the
// template rendering without file-system embedding.
type ComposeRenderer func(nodeName, role, newImage, cfgDir, outputPath string) error

// DriverConfig configures a Driver instance.
type DriverConfig struct {
	ConfigDir      string
	Topology       *topology.Topology
	ProbeTimeout   time.Duration
	DowntimeBudget time.Duration // per-node gRPC Ready poll budget (default 60 s)
	DeployWait     time.Duration // manual-deploy gRPC poll window (default 120 s)
	// Version is the target upgrade version string (e.g. "v1.12.11").
	// B27 fix: was hardcoded as "" in appendLog; now stored here so every log
	// entry records the version that was being installed when the event fired.
	Version string
	Logger  *Logger
	SSHOpts SSHOpts
	// SSHDeploy executes a remote shell command when SSHOpts.Enabled is true.
	// Must be set when SSHOpts.Enabled == true; ignored otherwise.
	// Inject from the cmd package via upgrade.go to avoid a circular import.
	SSHDeploy SSHDeployer
	// SSHUpload uploads a compose file via SFTP and then runs remoteCmd.
	// When SSHOpts.Enabled is true and SSHUpload is set, phaseDeploy and rollbackNode
	// use SSHUpload instead of SSHDeploy. Must be set alongside SSHDeploy when SSH is enabled.
	SSHUpload SSHUploader
	// RemoteComposeDir is the remote directory where compose files are uploaded
	// during SSH-mode deploy. Default: DefaultRemoteComposeDir ("/etc/docker/compose").
	RemoteComposeDir string
	// Prober is the data-plane probe function; nil = skip verify (tests).
	Prober DataPlaneProber
	// Reconcile is called during rollback to restore peer state.
	Reconcile Reconciler
	// RenderCompose generates the compose file for a node.
	RenderCompose ComposeRenderer
}

// Driver executes the per-node upgrade state machine.
// It is not safe for concurrent use on the same node — the orchestrating
// upgrade command drives nodes sequentially.
type Driver struct {
	cfg DriverConfig
}

// NewDriver creates a Driver with the given configuration.
// All DriverConfig fields must be set before calling UpgradeNode.
func NewDriver(cfg DriverConfig) *Driver {
	if cfg.DowntimeBudget <= 0 {
		cfg.DowntimeBudget = 60 * time.Second
	}
	if cfg.DeployWait <= 0 {
		cfg.DeployWait = 120 * time.Second
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 5 * time.Second
	}
	return &Driver{cfg: cfg}
}

// UpgradeNode executes the 5-phase upgrade pipeline for one node:
//
//	prepare → deploy → wait_ready → init → verify
//
// If the verify phase fails, rollbackNode is called automatically.
// Each phase appends two UpgradeLogEntry values (start + end) to Logger.
func (d *Driver) UpgradeNode(ctx context.Context, step *NodeUpgradeStep) error {
	step.Status = StatusRunning

	// --- Phase: prepare ---
	if err := d.phaseWithLog(ctx, step, "prepare", func() error {
		return d.phasePrepare(ctx, step)
	}); err != nil {
		step.Status = StatusFailed
		return fmt.Errorf("upgrade %s: prepare: %w", step.Name, err)
	}

	// --- Phase: deploy ---
	composePath := liveComposePath(d.cfg, step.Name)
	if err := d.phaseWithLog(ctx, step, "deploy", func() error {
		return d.phaseDeploy(ctx, step, composePath)
	}); err != nil {
		step.Status = StatusFailed
		return fmt.Errorf("upgrade %s: deploy: %w", step.Name, err)
	}

	// --- Phase: wait_ready ---
	if err := d.phaseWithLog(ctx, step, "wait_ready", func() error {
		return d.phaseWaitReady(ctx, step)
	}); err != nil {
		step.Status = StatusFailed
		return fmt.Errorf("upgrade %s: wait_ready: %w", step.Name, err)
	}

	// --- Phase: init ---
	if err := d.phaseWithLog(ctx, step, "init", func() error {
		return d.phaseInit(ctx, step)
	}); err != nil {
		step.Status = StatusFailed
		return fmt.Errorf("upgrade %s: init: %w", step.Name, err)
	}

	// --- Phase: verify ---
	verifyErr := d.phaseWithLog(ctx, step, "verify", func() error {
		return d.phaseVerify(ctx, step)
	})
	if verifyErr != nil {
		// Trigger rollback before returning the verify error.
		_ = d.appendLog(step, "rollback", "running", "", 0)
		rbStart := time.Now()
		rbErr := rollbackNode(ctx, d.cfg, step)
		rbDur := time.Since(rbStart)
		if rbErr != nil {
			_ = d.appendLog(step, "rollback", "failed", rbErr.Error(), rbDur.Milliseconds())
			step.Status = StatusFailed
			return fmt.Errorf("upgrade %s: verify failed (%w); rollback also failed: %v — snapshot at %s — manual recovery: mesh-ctl inspect %s && mesh-ctl reconcile",
				step.Name, verifyErr, rbErr, backupComposePath(d.cfg, step.Name), step.Name)
		}
		_ = d.appendLog(step, "rollback", "ok", "", rbDur.Milliseconds())
		step.Status = StatusRolledBack
		return fmt.Errorf("upgrade %s: verify failed (%w); node rolled back to previous version — snapshot at %s",
			step.Name, verifyErr, backupComposePath(d.cfg, step.Name))
	}

	step.Status = StatusDone
	return nil
}

// phaseWithLog wraps a phase function with before/after log entries.
func (d *Driver) phaseWithLog(ctx context.Context, step *NodeUpgradeStep, phase string, fn func() error) error {
	_ = d.appendLog(step, phase, "running", "", 0)
	start := time.Now()
	err := fn()
	dur := time.Since(start)
	if err != nil {
		_ = d.appendLog(step, phase, "failed", err.Error(), dur.Milliseconds())
		return err
	}
	_ = d.appendLog(step, phase, "ok", "", dur.Milliseconds())
	return nil
}

func (d *Driver) appendLog(step *NodeUpgradeStep, phase, status, reason string, durationMs int64) error {
	if d.cfg.Logger == nil {
		return nil
	}
	return d.cfg.Logger.Append(UpgradeLogEntry{
		Version:    d.cfg.Version, // B27 fix: was always ""; now set from DriverConfig.Version
		NodeName:   step.Name,
		Phase:      phase,
		Status:     status,
		Reason:     reason,
		DurationMs: durationMs,
		Timestamp:  time.Now(),
	})
}

// phasePrepare regenerates the compose file with the new image tag and snapshots
// the current compose to a .bak file.
func (d *Driver) phasePrepare(_ context.Context, step *NodeUpgradeStep) error {
	currentCompose := liveComposePath(d.cfg, step.Name)
	backupCompose := backupComposePath(d.cfg, step.Name)
	upgradeCompose := liveComposePath(d.cfg, step.Name)

	// Snapshot current compose to .bak (best-effort: may not exist on first deploy).
	if existing, err := os.ReadFile(currentCompose); err == nil {
		if err := os.WriteFile(backupCompose, existing, 0600); err != nil {
			return fmt.Errorf("snapshot current compose to %s: %w", backupCompose, err)
		}
	}

	// Render new compose with the target image.
	if d.cfg.RenderCompose == nil {
		return fmt.Errorf("RenderCompose is not configured")
	}
	return d.cfg.RenderCompose(step.Name, step.Role, step.NewImage, d.cfg.ConfigDir, upgradeCompose)
}

// phaseDeploy deploys the new compose file to the node.
// When SSHOpts.Enabled: SSH-trigger `docker compose up -d`.
// Otherwise: print the compose path and poll gRPC Ready for DeployWait.
func (d *Driver) phaseDeploy(_ context.Context, step *NodeUpgradeStep, composePath string) error {
	if d.cfg.SSHOpts.Enabled {
		return d.sshDeploy(step, composePath)
	}
	// Manual-deploy: print path and return (gRPC polling happens in wait_ready).
	fmt.Printf("  [%s] compose file ready: %s\n  Deploy with: docker compose -f %s up -d\n",
		step.Name, composePath, composePath)
	return nil
}

// sshDeploy triggers `docker compose up -d` via SSH using the injected SSHDeployer.
// The image ref is validated before constructing the remote command.
func (d *Driver) sshDeploy(step *NodeUpgradeStep, composePath string) error {
	if d.cfg.SSHDeploy == nil && d.cfg.SSHUpload == nil {
		return fmt.Errorf("SSH deploy: neither SSHDeploy nor SSHUpload is configured")
	}

	// Validate the image ref to prevent shell injection.
	if err := validateImageRef(step.NewImage); err != nil {
		return fmt.Errorf("SSH deploy: %w", err)
	}

	node := d.cfg.Topology.FindMaster(step.Name)
	var host string
	if node != nil {
		host = node.Host
	} else {
		ep := d.cfg.Topology.FindEndpoint(step.Name)
		if ep != nil {
			host = ep.Host
		}
	}
	if host == "" {
		return fmt.Errorf("SSH deploy: cannot determine host for node %q", step.Name)
	}

	opts := d.cfg.SSHOpts
	user := opts.User
	if user == "" {
		user = "root"
	}
	port := opts.Port
	if port == 0 {
		port = 22
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	rPath := remoteComposePath(d.cfg.RemoteComposeDir, step.Name)

	// Force a fresh image pull: remove any cached layer first so Docker cannot
	// silently reuse a stale image when the pull fails mid-transfer (issue #104).
	// "docker image rm" exits 1 when the image is absent (first deploy) — that
	// is expected and harmless, so we suppress it with "|| true".
	remoteCmd := "docker image rm " + shellQuote(step.NewImage) + " 2>/dev/null || true" +
		" && docker pull " + shellQuote(step.NewImage) +
		" && docker compose -f " + shellQuote(rPath) + " up -d"

	if d.cfg.SSHUpload != nil {
		return d.cfg.SSHUpload(addr, user, opts.KeyPath, opts.AcceptNewHosts, composePath, rPath, remoteCmd)
	}
	// Fallback: SSHUpload not wired (legacy / test-only) — passes admin path to remote.
	return d.cfg.SSHDeploy(addr, user, opts.KeyPath, opts.AcceptNewHosts, remoteCmd)
}

// phaseWaitReady polls GetStatus until the node reports Ready or the downtime
// budget is exhausted.  Delegates to the shared waitReady helper in rollback.go.
func (d *Driver) phaseWaitReady(ctx context.Context, step *NodeUpgradeStep) error {
	return waitReady(ctx, d.cfg, step)
}

// phaseInit runs the node init sequence via gRPC.
// For endpoints: always run init (ensures key propagation to masters).
// For masters: run init only if drift is detected (inspect shows missing_peer or key_mismatch).
func (d *Driver) phaseInit(ctx context.Context, step *NodeUpgradeStep) error {
	if step.Role != "endpoint" {
		// Masters do not need init after a same-config upgrade unless drifted.
		// Drift detection is handled by reconcile in rollback; skip init for masters.
		return nil
	}
	// For endpoints: re-run init to propagate the possibly-rotated pubkey to masters.
	grpcAddr := grpcAddrFor(d.cfg, step)
	if grpcAddr == "" {
		return fmt.Errorf("cannot determine gRPC address for node %q", step.Name)
	}

	nd := nodeDir(d.cfg.ConfigDir, step.Name)
	token, err := loadToken(nd)
	if err != nil {
		return fmt.Errorf("load token for %s: %w", step.Name, err)
	}
	ca := caPath(d.cfg.ConfigDir)

	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	client, err := grpcclient.NewClient(grpcclient.ClientConfig{
		Target:     grpcAddr,
		CACertPath: ca,
		Token:      token,
	})
	if err != nil {
		return fmt.Errorf("connect to %s for init: %w", step.Name, err)
	}
	defer func() { _ = client.Close() }()

	caCertPEM, err := os.ReadFile(ca)
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}

	ep := d.cfg.Topology.FindEndpoint(step.Name)
	if ep == nil {
		return fmt.Errorf("endpoint %q not found in topology", step.Name)
	}

	// Engram #131: phaseInit previously sent only CaCert + Config, omitting
	// NodeCert + NodeKey. Init handler at pkg/grpc/handlers.go (introduced in
	// PR #15 / v1.6.0) rejects any request missing them with InvalidArgument,
	// so every guided upgrade since v1.6.0 failed at phase 4. Mint a
	// per-node cert the same way the standalone `mesh-ctl endpoint init`
	// path does (cmd/mesh-ctl/cmd/endpoint.go).
	caCert, caKey, err := pkgtls.LoadCA(d.cfg.ConfigDir)
	if err != nil {
		return fmt.Errorf("load CA key material for %s init: %w", step.Name, err)
	}
	certPEM, keyPEM, err := pkgtls.IssueCert(caCert, caKey, ep.Name, []string{ep.Host})
	if err != nil {
		return fmt.Errorf("issue node cert for %s init: %w", step.Name, err)
	}

	_, initErr := client.Agent().Init(initCtx, &proto.InitRequest{
		CaCert:   caCertPEM,
		NodeCert: certPEM,
		NodeKey:  keyPEM,
		Config: &proto.NodeConfig{
			Name:       ep.Name,
			Mode:       "endpoint",
			OverlayIp:  ep.OverlayIP,
			ListenPort: int32(ep.ListenPort),
		},
	})
	if initErr != nil {
		return fmt.Errorf("init RPC for %s: %w", step.Name, initErr)
	}
	return nil
}

// phaseVerify runs data-plane probes for all pairs involving this node.
//
// B18 fix: the previous implementation probed immediately after wait_ready,
// which races with WireGuard tunnel establishment and reconcile propagation.
// Handshakes and AllowedIPs updates require a short settle window after the
// node reports gRPC Ready. We now wait verifySettleDelay before the first
// probe attempt and retry up to verifyMaxAttempts times with verifyRetryDelay
// between attempts so transient "tunnel not yet up" failures are not mistaken
// for real data-plane failures that trigger rollback.
const (
	verifySettleDelay = 5 * time.Second
	verifyRetryDelay  = 3 * time.Second
	verifyMaxAttempts = 3
)

func (d *Driver) phaseVerify(_ context.Context, step *NodeUpgradeStep) error {
	if d.cfg.Prober == nil {
		// No prober configured — skip verification (used in tests).
		return nil
	}

	// Settle: give WireGuard handshakes and reconcile propagation time to
	// complete before the first probe attempt.
	time.Sleep(verifySettleDelay)

	var lastErr error
	for attempt := 1; attempt <= verifyMaxAttempts; attempt++ {
		results := d.cfg.Prober(d.cfg.Topology, d.cfg.ConfigDir, d.cfg.ProbeTimeout, 4)
		var broken []string
		for _, r := range results {
			if r.Reason == "" {
				continue
			}
			if r.MasterName == step.Name || r.EndpointName == step.Name {
				broken = append(broken, fmt.Sprintf("master=%s endpoint=%s reason=%s",
					r.MasterName, r.EndpointName, r.Reason))
			}
		}
		if len(broken) == 0 {
			return nil
		}
		lastErr = fmt.Errorf("data-plane verification failed for %s: %s",
			step.Name, strings.Join(broken, "; "))
		if attempt < verifyMaxAttempts {
			fmt.Printf("  [%s] verify attempt %d/%d failed — retrying in %s\n",
				step.Name, attempt, verifyMaxAttempts, verifyRetryDelay)
			time.Sleep(verifyRetryDelay)
		}
	}
	return lastErr
}

// loadToken reads the auth token for a node from its node directory.
// This is a package-level helper shared with rollback.go.
func loadToken(nd string) (string, error) {
	data, err := os.ReadFile(nd + "/token")
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// validateImageRef is a thin wrapper around the same validation used in bootstrap.go.
// Prevents shell metacharacter injection when building SSH commands.
func validateImageRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("image reference must not be empty")
	}
	for _, r := range ref {
		if !isImageRefChar(r) {
			return fmt.Errorf("image reference %q contains invalid character %q", ref, r)
		}
	}
	return nil
}

func isImageRefChar(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '.' || r == '-' || r == '_' || r == '/' || r == ':' || r == '@'
}

// shellQuote wraps s in single quotes for safe inclusion in a remote shell command.
// Handles embedded single quotes via the POSIX `'\”` escape sequence so any
// input (including unusual node names or compose paths) is safe.
func shellQuote(s string) string {
	// End the quoted string, emit an escaped single quote, resume quoting.
	escaped := strings.ReplaceAll(s, "'", `'\''`)
	return "'" + escaped + "'"
}
