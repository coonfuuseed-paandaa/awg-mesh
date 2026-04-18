package upgrade

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	grpcclient "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
)

// rollbackNode restores a node to its pre-upgrade state after a failed verify.
//
// Steps:
//  1. Restore the .bak compose file over the live compose file.
//  2. SSH-trigger or print the old compose path for manual redeploy.
//  3. Poll gRPC Ready for DowntimeBudget.
//  4. Call Reconcile to restore mesh peer state.
//
// The function is called by Driver.UpgradeNode and is not exported because
// rollback is always automatic — the upgrade command never calls it directly.
func rollbackNode(ctx context.Context, cfg DriverConfig, step *NodeUpgradeStep) error {
	backupPath := backupComposePath(cfg, step.Name)
	livePath := liveComposePath(cfg, step.Name)

	// --- Step 1: Restore backup compose file ---
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("rollback %s: read backup compose %s: %w", step.Name, backupPath, err)
	}
	if err := os.WriteFile(livePath, backup, 0600); err != nil {
		return fmt.Errorf("rollback %s: restore compose to %s: %w", step.Name, livePath, err)
	}

	// --- Step 2: Redeploy old image ---
	if cfg.SSHOpts.Enabled {
		if err := sshRollback(cfg, step, livePath); err != nil {
			return fmt.Errorf("rollback %s: SSH redeploy: %w", step.Name, err)
		}
	} else {
		fmt.Printf("  [%s] ROLLBACK — restore old compose: %s\n  Redeploy with: docker compose -f %s up -d\n",
			step.Name, livePath, livePath)
	}

	// --- Step 3: Poll gRPC Ready ---
	if err := waitReady(ctx, cfg, step); err != nil {
		return fmt.Errorf("rollback %s: node did not recover: %w", step.Name, err)
	}

	// --- Step 4: Reconcile mesh peer state ---
	if cfg.Reconcile != nil {
		if err := cfg.Reconcile(cfg.Topology, cfg.ConfigDir); err != nil {
			return fmt.Errorf("rollback %s: reconcile: %w", step.Name, err)
		}
	}

	return nil
}

// sshRollback triggers `docker compose up -d` with the old compose file via SSH.
func sshRollback(cfg DriverConfig, step *NodeUpgradeStep, composePath string) error {
	if cfg.SSHDeploy == nil {
		return fmt.Errorf("SSHDeploy function is not configured")
	}

	node := cfg.Topology.FindMaster(step.Name)
	var host string
	if node != nil {
		host = node.Host
	} else {
		ep := cfg.Topology.FindEndpoint(step.Name)
		if ep != nil {
			host = ep.Host
		}
	}
	if host == "" {
		return fmt.Errorf("cannot determine host for node %q", step.Name)
	}

	opts := cfg.SSHOpts
	user := opts.User
	if user == "" {
		user = "root"
	}
	port := opts.Port
	if port == 0 {
		port = 22
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	rPath := remoteBackupComposePath(cfg.RemoteComposeDir, step.Name)
	remoteCmd := "docker compose -f " + shellQuote(rPath) + " up -d"

	if cfg.SSHUpload != nil {
		return cfg.SSHUpload(addr, user, opts.KeyPath, opts.AcceptNewHosts, composePath, rPath, remoteCmd)
	}
	// Fallback for tests that inject only SSHDeploy.
	return cfg.SSHDeploy(addr, user, opts.KeyPath, opts.AcceptNewHosts, remoteCmd)
}

// waitReady polls GetStatus until the node reports gRPC Ready or the downtime
// budget is exhausted.  Shared by phaseWaitReady and rollbackNode.
func waitReady(ctx context.Context, cfg DriverConfig, step *NodeUpgradeStep) error {
	grpcAddr := grpcAddrFor(cfg, step)
	if grpcAddr == "" {
		return fmt.Errorf("cannot determine gRPC address for node %q", step.Name)
	}

	nd := nodeDir(cfg.ConfigDir, step.Name)
	token, err := loadToken(nd)
	if err != nil {
		return fmt.Errorf("load token for %s: %w", step.Name, err)
	}
	caPath := caPath(cfg.ConfigDir)

	deadline := time.Now().Add(cfg.DowntimeBudget)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pollCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		client, connErr := grpcclient.NewClient(grpcclient.ClientConfig{
			Target:     grpcAddr,
			CACertPath: caPath,
			Token:      token,
		})
		if connErr != nil {
			cancel()
			time.Sleep(2 * time.Second)
			continue
		}
		_, statusErr := client.Agent().GetStatus(pollCtx, &proto.Empty{})
		cancel()
		_ = client.Close()
		if statusErr == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("node %s did not become ready within %s", step.Name, cfg.DowntimeBudget)
}

// backupComposePath returns the path of the pre-upgrade compose snapshot.
func backupComposePath(cfg DriverConfig, name string) string {
	return filepath.Join(nodeDir(cfg.ConfigDir, name), name+"-docker-compose.yml.bak")
}

// liveComposePath returns the live compose file path for a node.
func liveComposePath(cfg DriverConfig, name string) string {
	return filepath.Join(nodeDir(cfg.ConfigDir, name), name+"-docker-compose.yml")
}

// nodeDir returns the config directory for one node.
func nodeDir(configDir, name string) string {
	return filepath.Join(configDir, "nodes", name)
}

// caPath returns the CA certificate path for a config directory.
func caPath(configDir string) string {
	return filepath.Join(configDir, "ca.crt")
}

// grpcAddrFor returns the gRPC address for a node from the topology.
func grpcAddrFor(cfg DriverConfig, step *NodeUpgradeStep) string {
	if m := cfg.Topology.FindMaster(step.Name); m != nil {
		return m.GRPCAddr()
	}
	if e := cfg.Topology.FindEndpoint(step.Name); e != nil {
		return e.GRPCAddr()
	}
	return ""
}
