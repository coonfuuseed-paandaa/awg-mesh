package cmd

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	grpcclient "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/transport"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// reconcileNodeResult records the outcome of reconciling one node.
type reconcileNodeResult struct {
	name        string
	role        string
	updated     int
	unchanged   int
	failed      int
	skipped     int
	driftHealed int // FR-6: tunnels auto-healed via RemoveTunnel+AddTunnel downgrade
}

func newReconcileCommand() *cobra.Command {
	var forceUnlock bool

	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Force-sync admin state to every node in the topology (idempotent)",
		Long: `reconcile walks every master and endpoint node in the topology and pushes
admin's expected configuration via gRPC.

For each master:  calls UpdateTunnelPeer for every bound endpoint.
For each endpoint: calls AddPeer for every master it is bound to.

The command is idempotent — safe to re-run after manual intervention or
post-recovery. Unchanged peers are reported but do not count as failures.

Exit code: 0 if all nodes acknowledged, 1 if any gRPC failed.

Use --force-unlock to remove a stale reconcile.lock left by a crashed process.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			lockPath := filepath.Join(configDir, "reconcile.lock")

			// --force-unlock: remove the lock file and exit (operator recovery).
			if forceUnlock {
				if err := os.Remove(lockPath); err != nil {
					if os.IsNotExist(err) {
						fmt.Println("reconcile: no lock file found — nothing to remove")
						return nil
					}
					return fmt.Errorf("reconcile: force-unlock: %w", err)
				}
				fmt.Printf("reconcile: removed stale lock file %s\n", lockPath)
				return nil
			}

			// Acquire advisory file lock so concurrent reconcile runs do not race.
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
			fmt.Printf("%-20s %-10s %8s %10s %7s %8s %12s\n", "NODE", "ROLE", "UPDATED", "UNCHANGED", "FAILED", "SKIPPED", "DRIFT_HEALED")
			fmt.Println(strings.Repeat("-", 84))
			totalDriftHealed := 0
			for _, r := range results {
				fmt.Printf("%-20s %-10s %8d %10d %7d %8d %12d\n",
					r.name, r.role, r.updated, r.unchanged, r.failed, r.skipped, r.driftHealed)
				totalDriftHealed += r.driftHealed
			}
			fmt.Println()

			if totalDriftHealed > 0 {
				fmt.Fprintf(os.Stderr, "WARNING: drift auto-healed on %d tunnel(s) via RemoveTunnel+AddTunnel — investigate root cause in logs\n", totalDriftHealed)
			}

			if anyFailed {
				return fmt.Errorf("reconcile: one or more nodes reported failures — see above for details")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&forceUnlock, "force-unlock", false,
		"Remove a stale reconcile.lock left by a crashed process and exit")

	return cmd
}

// reconcileMasterNode pushes admin's endpoint key state to a single master.
// Pure function w.r.t. external state (reads configDir, calls gRPC) — extracted for testability.
//
// FR-6: when UpdateTunnelPeer fails due to admin-state drift (key mismatch detected
// by the error message from the handler), reconcile downgrades to RemoveTunnel +
// AddTunnel for that specific tunnel.  This is the safety-net for operator-induced
// drift (manual pubkey file edits). Option-D's FR-1..3 prevents drift in the normal
// flow; FR-6 handles the abnormal case.
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

	// Load transport allocator once — needed for the FR-6 AddTunnel downgrade path.
	alloc, allocErr := loadOrCreateAllocator(cfgDir, topo)

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

		var allowedIPs []string
		if allocErr == nil {
			if allocation, aErr := alloc.Allocate(master.Name, ep.Name); aErr == nil {
				if aips, aipErr := topology.BuildAllowedIPsForMasterPeer(topo, ep.Name, ep.OverlayIP, allocation.Subnet.String()); aipErr == nil {
					allowedIPs = aips
				} else {
					fmt.Fprintf(os.Stderr, "master %s: endpoint %s: build master-side allowed_ips: %v\n", master.Name, epName, aipErr)
				}
			} else {
				fmt.Fprintf(os.Stderr, "master %s: endpoint %s: allocate transport for allowed_ips: %v\n", master.Name, epName, aErr)
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, rpcErr := client.Agent().UpdateTunnelPeer(ctx, &proto.UpdateTunnelPeerRequest{
			Name:          epName,
			PeerPublicKey: pubkeyBytes,
			BalancerIp:    balancerIP,
			AllowedIps:    allowedIPs,
		})
		cancel()

		if rpcErr != nil {
			statusLine, isPreV110 := updateTunnelPeerFailureStatus(rpcErr)
			if isPreV110 {
				fmt.Fprintf(os.Stderr, "master %s is pre-v1.10.0 — upgrade master before using 'reconcile'\n", master.Name)
				result.failed++
				continue
			}

			// FR-6: detect admin-state drift from the structured error message
			// emitted by the handler (FR-5).  On drift, downgrade to
			// RemoveTunnel + AddTunnel so reconcile self-heals this tunnel.
			if isDriftError(rpcErr) && allocErr == nil {
				fmt.Fprintf(os.Stderr, "master %s: endpoint %s: drift detected — attempting self-heal (RemoveTunnel + AddTunnel)\n", master.Name, epName)
				if healed := reconcileSelfHeal(client, topo, master, ep, pubkeyBytes, balancerIP, alloc, cfgDir); healed {
					result.driftHealed++
					fmt.Fprintf(os.Stderr, "master %s: endpoint %s: drift self-healed successfully\n", master.Name, epName)
					continue
				}
				fmt.Fprintf(os.Stderr, "master %s: endpoint %s: self-heal failed — manual recovery needed\n", master.Name, epName)
			} else {
				fmt.Fprintf(os.Stderr, "master %s: endpoint %s: %s\n", master.Name, epName, statusLine)
			}
			result.failed++
			continue
		}

		if resp == nil || !resp.Success {
			fmt.Fprintf(os.Stderr, "master %s: endpoint %s: UpdateTunnelPeer returned unsuccessful\n", master.Name, epName)
			result.failed++
			continue
		}

		if resp.Unchanged {
			// T006: even when pubkeys match, check for empty AllowedIPs on the master.
			// Empty AllowedIPs is a drift condition introduced by Pattern X
			// (pre-v1.12.3 transport.yml written without AllowedIPs). Re-push via
			// UpdateTunnelPeer with the correct AllowedIPs to force resync.
			if allocErr == nil {
				if resynced := reconcileCheckEmptyAllowedIPs(client, master, ep, pubkeyBytes, balancerIP, alloc, cfgDir, topo); resynced {
					result.driftHealed++
					continue
				}
			}
			result.unchanged++
		} else {
			result.updated++
		}
	}

	return result
}

// isDriftError returns true when the gRPC error from UpdateTunnelPeer indicates
// an admin-state drift condition (key mismatch).  Detection uses the structured
// error message from the FR-5 handler — specifically the phrases injected there.
// Pre-v1.10.0 errors are excluded (they use codes.Unimplemented, not Internal).
func isDriftError(err error) bool {
	if err == nil {
		return false
	}
	if status.Code(err) == codes.Unimplemented {
		return false // pre-v1.10.0 master — not a drift, just old binary
	}
	errMsg := strings.ToLower(err.Error())
	return strings.Contains(errMsg, "key mismatch") ||
		strings.Contains(errMsg, "admin state has drifted") ||
		strings.Contains(errMsg, "wgctrl peer-replace")
}

// reconcileSelfHeal attempts a FR-6 downgrade heal: RemoveTunnel then AddTunnel
// with the current admin pubkey.  Returns true only if BOTH calls succeed.
func reconcileSelfHeal(
	client *grpcclient.Client,
	topo *topology.Topology,
	master *topology.MasterNode,
	ep *topology.EndpointNode,
	pubkeyBytes []byte,
	balancerIP string,
	alloc *transport.Allocator,
	cfgDir string,
) bool {
	// Step 1: RemoveTunnel.
	rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
	rmResp, rmErr := client.Agent().RemoveTunnel(rmCtx, &proto.RemoveTunnelRequest{Name: ep.Name})
	rmCancel()
	if rmErr != nil || rmResp == nil || !rmResp.Success {
		fmt.Fprintf(os.Stderr, "self-heal: RemoveTunnel %s on master %s failed: %v\n", ep.Name, master.Name, rmErr)
		return false
	}

	// Step 2: AddTunnel with current admin pubkey.
	allocation, allocateErr := alloc.Allocate(master.Name, ep.Name)
	if allocateErr != nil {
		fmt.Fprintf(os.Stderr, "self-heal: allocate transport for %s/%s: %v\n", master.Name, ep.Name, allocateErr)
		return false
	}

	masterAllowedIPs, maipErr := topology.BuildAllowedIPsForMasterPeer(topo, ep.Name, ep.OverlayIP, allocation.Subnet.String())
	if maipErr != nil {
		fmt.Fprintf(os.Stderr, "self-heal: build master allowed_ips for %s/%s: %v\n", master.Name, ep.Name, maipErr)
		masterAllowedIPs = []string{allocation.Subnet.String(), ep.OverlayIP + "/32"}
	}
	addCtx, addCancel := context.WithTimeout(context.Background(), 30*time.Second)
	addResp, addErr := client.Agent().AddTunnel(addCtx, &proto.AddTunnelRequest{
		Name:                ep.Name,
		EndpointHost:        ep.PeerAddr(),
		OverlayIp:           ep.OverlayIP,
		BalancerIp:          balancerIP,
		PeerPublicKey:       pubkeyBytes,
		Weight:              1,
		TransportSubnet:     allocation.Subnet.String(),
		MasterTransportIp:   allocation.MasterIP.String(),
		EndpointTransportIp: allocation.EndpointIP.String(),
		AllowedIps:          masterAllowedIPs,
	})
	addCancel()
	if addErr != nil || addResp == nil || !addResp.Success {
		fmt.Fprintf(os.Stderr, "self-heal: AddTunnel %s on master %s failed: %v\n", ep.Name, master.Name, addErr)
		return false
	}

	// Persist the reallocated transport state so future reconciles and node
	// restarts see the correct subnet assignments.
	if saveErr := saveTransportState(alloc, cfgDir); saveErr != nil {
		fmt.Fprintf(os.Stderr, "self-heal: save transport state: %v (non-fatal)\n", saveErr)
	}

	return true
}

// reconcileCheckEmptyAllowedIPs detects empty AllowedIPs drift on a master peer
// (T006 — Pattern X regression gate). When UpdateTunnelPeer reports Unchanged
// (pubkeys match), this function fetches the master's transport state via
// GetTransportState and checks whether the named peer has empty AllowedIPs.
// If empty AllowedIPs are detected, it re-pushes via UpdateTunnelPeer with the
// allocator-computed AllowedIPs. Returns true if a resync was performed.
func reconcileCheckEmptyAllowedIPs(
	client *grpcclient.Client,
	master *topology.MasterNode,
	ep *topology.EndpointNode,
	pubkeyBytes []byte,
	balancerIP string,
	alloc *transport.Allocator,
	cfgDir string,
	topo *topology.Topology,
) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	tsResp, tsErr := client.Agent().GetTransportState(ctx, &proto.Empty{})
	cancel()
	if tsErr != nil {
		// Pre-v1.10.1 masters do not implement GetTransportState; ignore gracefully.
		return false
	}
	if tsResp == nil {
		return false
	}

	// Locate the peer entry for this endpoint tunnel.
	var emptyAllowedIPs bool
	for _, peer := range tsResp.GetPeers() {
		if peer.GetName() != ep.Name {
			continue
		}
		// disk_allowed_ips is authoritative; fall back to runtime allowed_ips.
		diskIPs := peer.GetDiskAllowedIps()
		runtimeIPs := peer.GetAllowedIps()
		if len(diskIPs) == 0 || len(runtimeIPs) == 0 {
			emptyAllowedIPs = true
		}
		break
	}

	if !emptyAllowedIPs {
		return false
	}

	fmt.Fprintf(os.Stderr, "master %s: endpoint %s: drift detected (empty allowed_ips), forcing resync\n", master.Name, ep.Name)

	// Compute AllowedIPs from the allocator.
	allocation, allocateErr := alloc.Allocate(master.Name, ep.Name)
	if allocateErr != nil {
		fmt.Fprintf(os.Stderr, "master %s: endpoint %s: compute AllowedIPs for resync: %v\n", master.Name, ep.Name, allocateErr)
		return false
	}
	allowedIPs, buildErr := topology.BuildAllowedIPsForMasterPeer(topo, ep.Name, ep.OverlayIP, allocation.Subnet.String())
	if buildErr != nil {
		fmt.Fprintf(os.Stderr, "master %s: endpoint %s: build AllowedIPs for resync: %v\n", master.Name, ep.Name, buildErr)
		return false
	}

	syncCtx, syncCancel := context.WithTimeout(context.Background(), 30*time.Second)
	syncResp, syncErr := client.Agent().UpdateTunnelPeer(syncCtx, &proto.UpdateTunnelPeerRequest{
		Name:          ep.Name,
		PeerPublicKey: pubkeyBytes,
		BalancerIp:    balancerIP,
		AllowedIps:    allowedIPs,
	})
	syncCancel()
	if syncErr != nil {
		fmt.Fprintf(os.Stderr, "master %s: endpoint %s: AllowedIPs resync failed: %v\n", master.Name, ep.Name, syncErr)
		return false
	}
	if syncResp == nil || !syncResp.Success {
		fmt.Fprintf(os.Stderr, "master %s: endpoint %s: AllowedIPs resync returned unsuccessful\n", master.Name, ep.Name)
		return false
	}

	fmt.Fprintf(os.Stderr, "master %s: endpoint %s: AllowedIPs resync succeeded\n", master.Name, ep.Name)
	return true
}

// reconcileEndpointNode pushes admin's master peer state to a single endpoint.
// Pure function — extracted for testability.
func reconcileEndpointNode(
	topo *topology.Topology,
	ep *topology.EndpointNode,
	cfgDir string,
) reconcileNodeResult {
	result := reconcileNodeResult{name: ep.Name, role: "endpoint"}

	// Find all masters bound to this endpoint in deterministic order.
	boundMasters := topo.MastersForEndpoint(ep.Name)

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

	// Load transport allocator once — used to look up transport subnets per
	// (master, endpoint) pair so that AddPeer includes the full allowed_ips list
	// (FR-1: transport /30 + master overlay /32 + all overlay range CIDRs).
	alloc, allocErr := loadOrCreateAllocator(cfgDir, topo)

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

		// Build the full allowed_ips list: transport /30 + master overlay /32 +
		// all overlay range CIDRs. Fall back to master overlay /32 only when the
		// allocator is unavailable (e.g. transport.yml not yet written).
		var allowedIPs []string
		var transportSubnet string
		var localTransportIP, peerTransportIP string
		if allocErr == nil {
			if allocation, aErr := alloc.Allocate(m.Name, ep.Name); aErr == nil {
				transportSubnet = allocation.Subnet.String()
				localTransportIP = allocation.EndpointIP.String()
				peerTransportIP = allocation.MasterIP.String()
				if aips, bErr := topology.BuildAllowedIPsForEndpoint(topo, m.OverlayIP, transportSubnet); bErr == nil {
					allowedIPs = aips
				}
			}
		}
		if len(allowedIPs) == 0 {
			// Minimal fallback: at least send the master overlay /32 so the
			// endpoint has a route to reach the master on the overlay network.
			allowedIPs = []string{m.OverlayIP + "/32"}
			fmt.Fprintf(os.Stderr, "endpoint %s: master %s: transport allocator unavailable, using fallback allowed_ips\n", ep.Name, m.Name)
		}

		fmt.Printf("reconcile: AddPeer to endpoint %s (master %s) with allowed_ips=%v\n", ep.Name, m.Name, allowedIPs)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, rpcErr := client.Agent().AddPeer(ctx, &proto.AddPeerRequest{
			PublicKey:           masterPubkey,
			AllowedIps:          allowedIPs,
			EndpointHost:        m.PeerAddr(),
			PersistentKeepalive: 25,
			TransportSubnet:     transportSubnet,
			LocalTransportIp:    localTransportIP,
			PeerTransportIp:     peerTransportIP,
			PeerName:            m.Name,
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

// readAdminPubkeyBytes reads the raw 32-byte WireGuard public key from the
// admin-state directory.  Supports both storage formats:
//   - 32 raw bytes  (legacy — written by pre-v1.11.2 init commands)
//   - 64 hex chars + optional newline (current — written by adminstate.SetPubkey)
//
// Returns nil if the file is missing, unreadable, or in an unrecognised format.
func readAdminPubkeyBytes(cfgDir, name string) []byte {
	path := filepath.Join(nodeDir(cfgDir, name), "pubkey")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	// Legacy: raw 32-byte binary.
	if len(data) == 32 {
		return data
	}
	// Current: 64 hex chars (+ optional newline).
	trimmed := strings.TrimSpace(string(data))
	if len(trimmed) == 64 {
		b, hexErr := hex.DecodeString(trimmed)
		if hexErr != nil {
			return nil
		}
		return b
	}
	return nil
}
