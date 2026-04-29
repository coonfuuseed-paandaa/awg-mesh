package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/upgrade"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// newUpgradeCommand returns the `mesh-ctl upgrade <version>` command (F1).
// It computes an upgrade plan from the current topology, prints a preview,
// then executes the rolling upgrade node-by-node.
func newUpgradeCommand() *cobra.Command {
	var (
		orderFlag        []string
		dryRun           bool
		sshEnabled       bool
		sshUser          string
		sshPort          int
		sshKey           string
		sshPassphrase    string
		acceptNewHosts   bool
		downtimeSecs     int
		deployWaitSecs   int
		remoteComposeDir string
	)

	cmd := &cobra.Command{
		Use:   "upgrade <version>",
		Short: "Rolling upgrade all cluster nodes to the specified version",
		Long: `upgrade executes a guided rolling upgrade of every awg-mesh node to the
target version tag (e.g. v1.10.2).

Nodes are upgraded in order: endpoints first (grouped by region, sorted
alphabetically within each group), masters last (sorted alphabetically).
Use --order to override the computed sequence.

For each node the driver runs five phases:
  1. prepare   — render new compose file with target image; snapshot .bak
  2. deploy    — SSH-trigger docker compose up -d, OR print path for manual deploy
  3. wait_ready — poll gRPC GetStatus until node reports ready
  4. init      — for endpoints: re-run Init RPC to propagate keys to masters
  5. verify    — run data-plane probes for all tunnel pairs involving this node

On verify failure: automatic per-node rollback (restore .bak → redeploy → reconcile).

All phase events are recorded in a JSONL audit log:
  ~/.mesh-ctl/upgrade-<version>-<timestamp>.log

Exit code: 0 = all nodes succeeded; 1 = one or more nodes failed or rolled back.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			version := args[0]
			if strings.TrimSpace(version) == "" {
				return fmt.Errorf("version argument must not be empty")
			}

			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology: %w", err)
			}

			plan, err := upgrade.ComputePlan(topo, version, upgrade.PlanOptions{
				ManualOrder:        orderFlag,
				NodeDowntimeBudget: time.Duration(downtimeSecs) * time.Second,
			})
			if err != nil {
				return fmt.Errorf("compute upgrade plan: %w", err)
			}

			// Print plan preview.
			fmt.Printf("\nUpgrade plan — version %s — %d nodes\n\n", plan.Version, len(plan.Nodes))
			fmt.Printf("%-20s %-10s %-12s %s\n", "NODE", "ROLE", "REGION", "TARGET IMAGE")
			fmt.Println(strings.Repeat("-", 75))
			for _, n := range plan.Nodes {
				region := n.Region
				if region == "" {
					region = "-"
				}
				fmt.Printf("%-20s %-10s %-12s %s\n", n.Name, n.Role, region, n.NewImage)
			}
			fmt.Println()

			if dryRun {
				fmt.Println("Dry run — no changes made.")
				return nil
			}

			// Confirm with the operator unless stdout is not a terminal.
			if isTerminal(os.Stdout) {
				fmt.Printf("Proceed with upgrade? [y/N] ")
				var answer string
				_, _ = fmt.Scanln(&answer)
				if !strings.EqualFold(strings.TrimSpace(answer), "y") {
					fmt.Println("Upgrade cancelled.")
					return nil
				}
			}

			// Open audit log.
			logPath := upgrade.LogPath(configDir, version, time.Now())
			logger, logErr := upgrade.NewLogger(logPath)
			if logErr != nil {
				return fmt.Errorf("open upgrade log: %w", logErr)
			}
			defer func() { _ = logger.Close() }()
			fmt.Printf("Audit log: %s\n\n", logPath)

			sshOpts := upgrade.SSHOpts{
				Enabled:        sshEnabled,
				User:           sshUser,
				Port:           sshPort,
				KeyPath:        sshKey,
				Passphrase:     sshPassphrase,
				AcceptNewHosts: acceptNewHosts,
			}

			cfg := upgrade.DriverConfig{
				ConfigDir:        configDir,
				Topology:         topo,
				Version:          version, // B27 fix: populate so log entries record the target version
				Logger:           logger,
				SSHOpts:          sshOpts,
				SSHDeploy:        buildSSHDeployer(sshOpts),
				SSHUpload:        buildSSHUploader(sshOpts),
				RemoteComposeDir: remoteComposeDir,
				DowntimeBudget:   time.Duration(downtimeSecs) * time.Second,
				DeployWait:       time.Duration(deployWaitSecs) * time.Second,
				Prober:           buildProberAdapter(topo),
				Reconcile:        buildReconcileAdapter(topo),
				RenderCompose:    buildComposeRenderer(),
			}
			drv := upgrade.NewDriver(cfg)

			var failed []string
			for i := range plan.Nodes {
				step := &plan.Nodes[i]
				fmt.Printf("[%d/%d] Upgrading %s (%s)...\n", i+1, len(plan.Nodes), step.Name, step.Role)
				if err := drv.UpgradeNode(context.Background(), step); err != nil {
					fmt.Fprintf(os.Stderr, "  ERROR: %v\n", err)
					failed = append(failed, step.Name)
					// Stop the rolling upgrade on first failure to preserve mesh stability.
					break
				}
				fmt.Printf("  [%s] %s\n\n", step.Status, step.Name)
			}

			// Print summary.
			fmt.Println()
			fmt.Printf("Upgrade summary — version %s\n", version)
			fmt.Println(strings.Repeat("-", 40))
			done := 0
			for _, n := range plan.Nodes {
				symbol := "✓"
				switch n.Status {
				case upgrade.StatusFailed:
					symbol = "✗"
				case upgrade.StatusRolledBack:
					symbol = "↩"
				case upgrade.StatusSkipped:
					symbol = "-"
				case upgrade.StatusPlanned:
					symbol = " "
				}
				fmt.Printf("  %s  %s (%s)\n", symbol, n.Name, n.Status)
				if n.Status == upgrade.StatusDone {
					done++
				}
			}
			fmt.Println()
			fmt.Printf("Done: %d/%d nodes succeeded\n", done, len(plan.Nodes))
			fmt.Printf("Log:  %s\n\n", logPath)

			if len(failed) > 0 {
				return fmt.Errorf("upgrade failed: %d node(s) did not complete: %s",
					len(failed), strings.Join(failed, ", "))
			}
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&orderFlag, "order", nil, "Override node upgrade order (comma-separated names)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print plan without executing")
	cmd.Flags().BoolVar(&sshEnabled, "ssh", false, "SSH-trigger docker compose up -d on each node")
	cmd.Flags().StringVar(&sshUser, "ssh-user", "root", "SSH username")
	cmd.Flags().IntVar(&sshPort, "ssh-port", 22, "SSH port")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "Path to SSH private key (default: ssh-agent or ~/.ssh/id_ed25519)")
	cmd.Flags().StringVar(&sshPassphrase, "ssh-passphrase", "", "Passphrase for --ssh-key (falls back to MESH_SSH_KEY_PASSPHRASE env var)")
	cmd.Flags().BoolVar(&acceptNewHosts, "accept-new-host-key", false, "Accept unknown SSH host keys (TOFU; use only on first contact)")
	cmd.Flags().IntVar(&downtimeSecs, "downtime-budget", 60, "Per-node gRPC ready poll budget in seconds")
	cmd.Flags().IntVar(&deployWaitSecs, "deploy-wait", 120, "Manual-deploy gRPC poll window in seconds")
	cmd.Flags().StringVar(&remoteComposeDir, "remote-compose-dir", upgrade.DefaultRemoteComposeDir,
		"Remote directory where compose files are uploaded during SSH-mode deploy (default: /etc/docker/compose)")

	cmd.AddCommand(newUpgradeStatusCommand())

	return cmd
}

// newUpgradeStatusCommand returns `mesh-ctl upgrade status` which prints the
// most recent upgrade log entries.
func newUpgradeStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the most recent upgrade log",
		RunE: func(cmd *cobra.Command, args []string) error {
			logPath, err := upgrade.MostRecentLogPath(configDir)
			if err != nil {
				return fmt.Errorf("find upgrade log: %w", err)
			}
			if logPath == "" {
				fmt.Println("No upgrade log found.")
				return nil
			}

			l, err := upgrade.NewLogger(logPath)
			if err != nil {
				return fmt.Errorf("open upgrade log: %w", err)
			}
			defer func() { _ = l.Close() }()

			entries, err := l.ReadAll()
			if err != nil {
				return fmt.Errorf("read upgrade log: %w", err)
			}

			fmt.Printf("Upgrade log: %s\n\n", logPath)
			fmt.Printf("%-30s %-10s %-15s %-8s %s\n", "TIMESTAMP", "NODE", "PHASE", "STATUS", "REASON")
			fmt.Println(strings.Repeat("-", 85))
			for _, e := range entries {
				ts := e.Timestamp.UTC().Format("2006-01-02T15:04:05Z")
				fmt.Printf("%-30s %-10s %-15s %-8s %s\n",
					ts, e.NodeName, e.Phase, e.Status, e.Reason)
			}
			return nil
		},
	}
}

// ─── adapters ──────────────────────────────────────────────────────────────────

// buildSSHDeployer returns an upgrade.SSHDeployer that reuses the bootstrap.go
// SSH machinery (dialSSH + sess.Run).
func buildSSHDeployer(opts upgrade.SSHOpts) upgrade.SSHDeployer {
	if !opts.Enabled {
		return nil
	}
	return func(addr, user, keyPath string, acceptNewHosts bool, remoteCmd string) error {
		host, _, err := parseHostPort(addr)
		if err != nil {
			host = addr
		}
		port := opts.Port
		if port == 0 {
			port = 22
		}

		sshClientOpts := bootstrapOpts{
			host:             host,
			user:             user,
			port:             port,
			sshKey:           keyPath,
			sshPassphrase:    opts.Passphrase,
			acceptNewHostKey: acceptNewHosts,
		}
		client, dialErr := dialSSH(sshClientOpts, log.With().Str("node", host).Logger())
		if dialErr != nil {
			return fmt.Errorf("SSH dial %s: %w", addr, dialErr)
		}
		defer func() { _ = client.Close() }()

		sess, sessErr := client.NewSession()
		if sessErr != nil {
			return fmt.Errorf("SSH new session: %w", sessErr)
		}
		defer func() { _ = sess.Close() }()

		sess.Stdout = os.Stdout
		sess.Stderr = os.Stderr
		if runErr := sess.Run(remoteCmd); runErr != nil {
			if exitErr, ok := runErr.(*ssh.ExitError); ok {
				return fmt.Errorf("remote command exited %d: %s", exitErr.ExitStatus(), remoteCmd)
			}
			return fmt.Errorf("remote command failed: %w", runErr)
		}
		return nil
	}
}

// buildSSHUploader returns an upgrade.SSHUploader that dials SSH once, uploads
// the compose file via SFTP subchannel, then executes remoteCmd on the same client.
// This avoids a second TCP connection and reuses the authenticated session.
func buildSSHUploader(opts upgrade.SSHOpts) upgrade.SSHUploader {
	if !opts.Enabled {
		return nil
	}
	return func(addr, user, keyPath string, acceptNewHosts bool, adminPath, remotePath, remoteCmd string) error {
		host, _, err := parseHostPort(addr)
		if err != nil {
			host = addr
		}
		port := opts.Port
		if port == 0 {
			port = 22
		}

		sshClientOpts := bootstrapOpts{
			host:             host,
			user:             user,
			port:             port,
			sshKey:           keyPath,
			sshPassphrase:    opts.Passphrase,
			acceptNewHostKey: acceptNewHosts,
		}
		client, dialErr := dialSSH(sshClientOpts, log.With().Str("node", host).Logger())
		if dialErr != nil {
			return fmt.Errorf("SSH dial %s: %w", addr, dialErr)
		}
		defer func() { _ = client.Close() }()

		// Upload compose via SFTP subchannel on the same TCP connection (FR-1).
		if uploadErr := upgrade.UploadComposeFile(client, adminPath, remotePath); uploadErr != nil {
			return fmt.Errorf("SFTP upload compose to %s:%s: %w", addr, remotePath, uploadErr)
		}

		// Run docker compose command using the remote path (FR-4).
		sess, sessErr := client.NewSession()
		if sessErr != nil {
			return fmt.Errorf("SSH new session: %w", sessErr)
		}
		defer func() { _ = sess.Close() }()
		sess.Stdout = os.Stdout
		sess.Stderr = os.Stderr
		if runErr := sess.Run(remoteCmd); runErr != nil {
			if exitErr, ok := runErr.(*ssh.ExitError); ok {
				return fmt.Errorf("remote command exited %d: %s", exitErr.ExitStatus(), remoteCmd)
			}
			return fmt.Errorf("remote command failed: %w", runErr)
		}
		return nil
	}
}

// buildProberAdapter wraps runDataPlaneProbes (from status.go) to match
// upgrade.DataPlaneProber.
func buildProberAdapter(_ *topology.Topology) upgrade.DataPlaneProber {
	return func(
		topo *topology.Topology,
		cfgDir string,
		probeTimeout time.Duration,
		maxConcurrency int,
	) []upgrade.PairProbeResult {
		raw := runDataPlaneProbes(topo, cfgDir, probeTimeout, maxConcurrency)
		out := make([]upgrade.PairProbeResult, len(raw))
		for i, r := range raw {
			out[i] = upgrade.PairProbeResult{
				MasterName:   r.masterName,
				EndpointName: r.endpointName,
				Reason:       r.reason,
			}
		}
		return out
	}
}

// buildReconcileAdapter wraps the reconcile logic to match upgrade.Reconciler.
func buildReconcileAdapter(topo *topology.Topology) upgrade.Reconciler {
	return func(t *topology.Topology, cfgDir string) error {
		// Compute balancer IP ranges once.
		parsedRanges := make([]topology.Range, 0, len(t.Overlay.Ranges))
		for _, nr := range t.Overlay.Ranges {
			r, err := topology.ParseRange(nr)
			if err != nil {
				log.Warn().Str("name", nr.Name).Str("cidr", nr.CIDR).Err(err).Msg("skipping invalid overlay range during reconcile")
				continue
			}
			parsedRanges = append(parsedRanges, r)
		}

		// Reconcile all masters.
		for _, m := range t.Masters {
			master := m
			result := reconcileMasterNode(t, &master, cfgDir, parsedRanges)
			if result.failed > 0 {
				return fmt.Errorf("reconcile master %s: %d peer(s) failed", m.Name, result.failed)
			}
		}
		return nil
	}
}

// buildComposeRenderer returns a ComposeRenderer that re-renders the
// docker-compose file for a node using the existing template machinery.
func buildComposeRenderer() upgrade.ComposeRenderer {
	return func(nodeName, role, newImage, cfgDir, outputPath string) error {
		// Load the node's existing compose (if any) to detect current schema and
		// determine template to use.  Failure to read means first-time render.
		var tmplName string
		switch role {
		case "master":
			tmplName = "docker-compose.master.yml.tmpl"
		default:
			tmplName = "docker-compose.endpoint.yml.tmpl"
		}

		tmplContent, err := loadTemplate(tmplName)
		if err != nil {
			// Template not found — try to migrate the existing compose instead.
			return renderByMigration(nodeName, cfgDir, newImage, outputPath)
		}

		nd := nodeDir(cfgDir, nodeName)
		tok, tokErr := loadToken(nd)
		if tokErr != nil {
			return fmt.Errorf("load token for %s: %w", nodeName, tokErr)
		}

		// Re-read the topology to get node details for template rendering.
		topo, topoErr := topology.LoadTopology(topologyPath)
		if topoErr != nil {
			return fmt.Errorf("load topology for compose render: %w", topoErr)
		}

		data, renderErr := buildComposeData(topo, nodeName, role, newImage, tok, cfgDir)
		if renderErr != nil {
			return fmt.Errorf("build compose data for %s: %w", nodeName, renderErr)
		}

		out, execErr := execTemplate(tmplContent, data)
		if execErr != nil {
			return fmt.Errorf("render compose template for %s: %w", nodeName, execErr)
		}

		if err := os.MkdirAll(filepath.Dir(outputPath), 0700); err != nil {
			return fmt.Errorf("create output dir: %w", err)
		}
		return os.WriteFile(outputPath, []byte(out), 0600)
	}
}

// renderByMigration migrates the existing compose to current schema, then
// patches the image tag to newImage.  Fallback when no template is found.
func renderByMigration(nodeName, cfgDir, newImage, outputPath string) error {
	nd := nodeDir(cfgDir, nodeName)
	composePath := filepath.Join(nd, nodeName+"-docker-compose.yml")
	data, err := os.ReadFile(composePath)
	if err != nil {
		return fmt.Errorf("read existing compose for %s: %w", nodeName, err)
	}

	schema, err := upgrade.DetectSchema(data)
	if err != nil {
		return fmt.Errorf("detect compose schema for %s: %w", nodeName, err)
	}

	migrated, err := upgrade.MigrateCompose(data, schema)
	if err != nil {
		return fmt.Errorf("migrate compose for %s: %w", nodeName, err)
	}

	// Patch the image line to use the new image.
	out := patchImageLine(string(migrated), newImage)
	return os.WriteFile(outputPath, []byte(out), 0600)
}

// patchImageLine replaces the `image: ...` line of the awg-mesh-node service
// only (not every image: line). Scans for the service header and rewrites the
// first image: line inside that service scope; leaves unrelated sidecars untouched.
func patchImageLine(compose, newImage string) string {
	lines := strings.Split(compose, "\n")
	inAwgService := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "awg-mesh-node:") {
			inAwgService = true
			continue
		}
		if inAwgService && strings.HasPrefix(trimmed, "image:") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			lines[i] = indent + "image: " + newImage
			inAwgService = false
		}
	}
	return strings.Join(lines, "\n")
}

// ─── utilities ────────────────────────────────────────────────────────────────

// isTerminal returns true when f is connected to an interactive terminal.
// Uses a simple heuristic: if the file mode has the ModeCharDevice bit set.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// parseHostPort splits "host:port" and returns host.
func parseHostPort(addr string) (string, string, error) {
	colon := strings.LastIndex(addr, ":")
	if colon < 0 {
		return addr, "", nil
	}
	return addr[:colon], addr[colon+1:], nil
}

// buildComposeData constructs the template data map for compose rendering.
// It pulls node-specific fields (overlay IP, listen port, etc.) from the topology.
//
// B14 fix: the previous implementation stored the raw token under "Token" but
// the compose templates expect {{.TokenHash}} (a v2 hash, charset [A-Za-z0-9._-],
// no dollar signs — no Compose escaping needed). Missing key → text/template
// renders <no value> → node bootstraps an empty MESH_TOKEN_HASH and rejects
// every auth attempt.
func buildComposeData(topo *topology.Topology, nodeName, role, newImage, token, cfgDir string) (map[string]interface{}, error) {
	// Hash the raw token. v2 hashes use charset [A-Za-z0-9._-] — no $ to escape.
	hash, err := pkgtls.HashToken(token)
	if err != nil {
		return nil, fmt.Errorf("hash token for %s: %w", nodeName, err)
	}

	data := map[string]interface{}{
		"Image":     newImage,
		"Token":     token, // kept for backward compat; templates should use TokenHash
		"TokenHash": hash,  // B14 fix: what compose templates actually reference
		"ConfigDir": cfgDir,
	}

	switch role {
	case "master":
		m := topo.FindMaster(nodeName)
		if m == nil {
			return nil, fmt.Errorf("master %q not found in topology", nodeName)
		}
		data["Name"] = m.Name
		data["Mode"] = "master"
		data["OverlayIP"] = m.OverlayIP
		data["ListenPort"] = m.ListenPort
		data["Host"] = m.Host
	case "endpoint":
		e := topo.FindEndpoint(nodeName)
		if e == nil {
			return nil, fmt.Errorf("endpoint %q not found in topology", nodeName)
		}
		data["Name"] = e.Name
		data["Mode"] = "endpoint"
		data["OverlayIP"] = e.OverlayIP
		data["ListenPort"] = e.ListenPort
		data["Host"] = e.Host
	}
	return data, nil
}

// execTemplate executes a text template with the given data and returns the result.
// Uses text/template stdlib; no custom funcs for now (compose templates are simple key
// substitutions; if we need sprig-style helpers later, extend here).
func execTemplate(tmplContent string, data map[string]interface{}) (string, error) {
	tmpl, err := template.New("compose").Parse(tmplContent)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}
