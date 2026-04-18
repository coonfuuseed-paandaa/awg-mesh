package cmd

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	grpcclient "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// reconcileNodeResult records the outcome of reconciling one node.
type reconcileNodeResult struct {
	name      string
	role      string
	updated   int
	unchanged int
	failed    int
	skipped   int
}

func newReconcileCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile",
		Short: "Force-sync admin state to every node in the topology (idempotent)",
		Long: `reconcile walks every master and endpoint node in the topology and pushes
admin's expected configuration via gRPC.

For each master:  calls UpdateTunnelPeer for every bound endpoint.
For each endpoint: calls AddPeer for every master it is bound to.

The command is idempotent — safe to re-run after manual intervention or
post-recovery. Unchanged peers are reported but do not count as failures.

Exit code: 0 if all nodes acknowledged, 1 if any gRPC failed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Acquire advisory file lock so concurrent reconcile runs do not race.
			lockPath := filepath.Join(configDir, "reconcile.lock")
			release, lockErr := acquireFileLock(lockPath)
			if lockErr != nil {
				return fmt.Errorf("reconcile: %w", lockErr)
			}
			defer release()

			// Snapshot topology into memory once at start (R2 mitigation).
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology: %w", err)
			}

			// Pre-compute balancer IP lookup table from overlay ranges.
			parsedRanges := make([]topology.Range, 0, len(topo.Overlay.Ranges))
			for _, nr := range topo.Overlay.Ranges {
				r, rErr := topology.ParseRange(nr)
				if rErr != nil {
					fmt.Fprintf(os.Stderr, "warning: skipping unparseable overlay range %q: %v\n", nr.Name, rErr)
					continue
				}
				parsedRanges = append(parsedRanges, r)
			}

			var results []reconcileNodeResult
			anyFailed := false

			// --- Reconcile each master ---
			for _, m := range topo.Masters {
				master := m // local copy
				result := reconcileMasterNode(topo, &master, configDir, parsedRanges)
				results = append(results, result)
				if result.failed > 0 {
					anyFailed = true
				}
			}

			// --- Reconcile each endpoint ---
			for _, e := range topo.Endpoints {
				ep := e // local copy
				result := reconcileEndpointNode(topo, &ep, configDir)
				results = append(results, result)
				if result.failed > 0 {
					anyFailed = true
				}
			}

			// Print summary.
			fmt.Println()
			fmt.Printf("%-20s %-10s %8s %10s %7s %8s\n", "NODE", "ROLE", "UPDATED", "UNCHANGED", "FAILED", "SKIPPED")
			fmt.Println(strings.Repeat("-", 70))
			for _, r := range results {
				fmt.Printf("%-20s %-10s %8d %10d %7d %8d\n",
					r.name, r.role, r.updated, r.unchanged, r.failed, r.skipped)
			}
			fmt.Println()

			if anyFailed {
				return fmt.Errorf("reconcile: one or more nodes reported failures — see above for details")
			}
			return nil
		},
	}
}

// reconcileMasterNode pushes admin's endpoint key state to a single master.
// Pure function w.r.t. external state (reads configDir, calls gRPC) — extracted for testability.
func reconcileMasterNode(
	topo *topology.Topology,
	master *topology.MasterNode,
	cfgDir string,
	parsedRanges []topology.Range,
) reconcileNodeResult {
	result := reconcileNodeResult{name: master.Name, role: "master"}

	nd := nodeDir(cfgDir, master.Name)
	token, tokenErr := loadToken(nd)
	if tokenErr != nil {
		fmt.Fprintf(os.Stderr, "master %s: no token (%v) — skipping\n", master.Name, tokenErr)
		result.skipped = len(master.Endpoints)
		return result
	}

	client, err := grpcclient.NewClient(grpcclient.ClientConfig{
		Target:   master.GRPCAddr(),
		Token:    token,
		Insecure: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "master %s: connect failed (%v) — skipping\n", master.Name, err)
		result.skipped = len(master.Endpoints)
		return result
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: close grpc client for master %s: %v\n", master.Name, closeErr)
		}
	}()

	for _, epName := range master.Endpoints {
		ep := topo.FindEndpoint(epName)
		if ep == nil {
			fmt.Fprintf(os.Stderr, "master %s: endpoint %q not found in topology — skipping\n", master.Name, epName)
			result.skipped++
			continue
		}

		pubkeyPath := filepath.Join(nodeDir(cfgDir, epName), "pubkey")
		pubkeyBytes, pkErr := readEndpointPublicKey(pubkeyPath)
		if pkErr != nil {
			fmt.Fprintf(os.Stderr, "master %s: endpoint %s: read pubkey (%v) — skipping\n", master.Name, epName, pkErr)
			result.skipped++
			continue
		}

		// Compute balancer IP for this endpoint overlay address.
		var balancerIP string
		if epAddr, addrErr := netip.ParseAddr(ep.OverlayIP); addrErr == nil {
			if bip := topology.BalancerIPForAddr(parsedRanges, epAddr); bip.IsValid() {
				balancerIP = bip.String()
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, rpcErr := client.Agent().UpdateTunnelPeer(ctx, &proto.UpdateTunnelPeerRequest{
			Name:          epName,
			PeerPublicKey: pubkeyBytes,
			BalancerIp:    balancerIP,
		})
		cancel()

		if rpcErr != nil {
			statusLine, isPreV110 := updateTunnelPeerFailureStatus(rpcErr)
			if isPreV110 {
				fmt.Fprintf(os.Stderr, "master %s is pre-v1.10.0 — upgrade master before using 'reconcile'\n", master.Name)
			}
			fmt.Fprintf(os.Stderr, "master %s: endpoint %s: %s\n", master.Name, epName, statusLine)
			result.failed++
			continue
		}

		if resp == nil || !resp.Success {
			fmt.Fprintf(os.Stderr, "master %s: endpoint %s: UpdateTunnelPeer returned unsuccessful\n", master.Name, epName)
			result.failed++
			continue
		}

		if resp.Unchanged {
			result.unchanged++
		} else {
			result.updated++
		}
	}

	return result
}

// reconcileEndpointNode pushes admin's master peer state to a single endpoint.
// Pure function — extracted for testability.
func reconcileEndpointNode(
	topo *topology.Topology,
	ep *topology.EndpointNode,
	cfgDir string,
) reconcileNodeResult {
	result := reconcileNodeResult{name: ep.Name, role: "endpoint"}

	// Find all masters bound to this endpoint.
	var boundMasters []topology.MasterNode
	for _, m := range topo.Masters {
		if containsName(m.Endpoints, ep.Name) {
			boundMasters = append(boundMasters, m)
		}
	}

	if len(boundMasters) == 0 {
		return result
	}

	nd := nodeDir(cfgDir, ep.Name)
	token, tokenErr := loadToken(nd)
	if tokenErr != nil {
		fmt.Fprintf(os.Stderr, "endpoint %s: no token (%v) — skipping\n", ep.Name, tokenErr)
		result.skipped = len(boundMasters)
		return result
	}

	client, err := grpcclient.NewClient(grpcclient.ClientConfig{
		Target:   ep.GRPCAddr(),
		Token:    token,
		Insecure: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "endpoint %s: connect failed (%v) — skipping\n", ep.Name, err)
		result.skipped = len(boundMasters)
		return result
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: close grpc client for endpoint %s: %v\n", ep.Name, closeErr)
		}
	}()

	for _, m := range boundMasters {
		masterPubkey := readAdminPubkeyBytes(cfgDir, m.Name)
		if masterPubkey == nil {
			fmt.Fprintf(os.Stderr, "endpoint %s: master %s: no pubkey — skipping\n", ep.Name, m.Name)
			result.skipped++
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, rpcErr := client.Agent().AddPeer(ctx, &proto.AddPeerRequest{
			PublicKey:           masterPubkey,
			AllowedIps:          []string{m.OverlayIP + "/32"},
			EndpointHost:        m.PeerAddr(),
			PersistentKeepalive: 25,
		})
		cancel()

		if rpcErr != nil {
			// AlreadyExists means the peer is already configured — treat as unchanged.
			if status.Code(rpcErr) == codes.AlreadyExists ||
				strings.Contains(strings.ToLower(rpcErr.Error()), "already exists") {
				result.unchanged++
				continue
			}
			fmt.Fprintf(os.Stderr, "endpoint %s: master %s: AddPeer failed: %v\n", ep.Name, m.Name, rpcErr)
			result.failed++
			continue
		}

		if resp == nil || !resp.Success {
			fmt.Fprintf(os.Stderr, "endpoint %s: master %s: AddPeer returned unsuccessful\n", ep.Name, m.Name)
			result.failed++
			continue
		}

		result.updated++
	}

	return result
}

// readAdminPubkeyBytes reads the raw 32-byte pubkey from the admin state directory.
// Returns nil if the file is missing or does not contain exactly 32 bytes.
func readAdminPubkeyBytes(cfgDir, name string) []byte {
	path := filepath.Join(nodeDir(cfgDir, name), "pubkey")
	data, err := os.ReadFile(path)
	if err != nil || len(data) != 32 {
		return nil
	}
	return data
}
