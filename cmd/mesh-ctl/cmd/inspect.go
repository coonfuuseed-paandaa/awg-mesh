package cmd

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	grpcclient "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// driftRow holds one row of the inspect comparison table.
type driftRow struct {
	peerName     string
	adminKey     string // hex pubkey known to admin (from disk pubkey file)
	diskKey      string // hex pubkey from node's GetTransportState (disk+runtime combined)
	runtimeKey   string // hex pubkey from ListPeers / ListTunnels
	adminIPs     string // allowed IPs per topology
	diskIPs      string
	runtimeIPs   string
	driftReasons []string
}

// adminPeerView holds what admin believes a peer should look like.
type adminPeerView struct {
	name      string
	pubkeyHex string
	allowedIPs []string
}

func newInspectCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <node>",
		Short: "Inspect a node's state and report drift between admin, disk, and runtime",
		Long: `inspect <node> fetches the node's in-memory transport state via GetTransportState
and compares it against admin's expected configuration (from topology + local admin state).

Drift reasons:
  key_mismatch       — peer public key differs between admin/disk/runtime
  missing_peer       — peer present in admin view but absent from node
  stale_allowed_ips  — allowed IPs do not match admin expectation
  extra_peer         — peer present on node but not in admin view

Exit code: 0 if no drift found, 1 if drift detected.
Pre-v1.10.1 nodes (returning codes.Unimplemented) are reported gracefully.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeName := args[0]

			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology: %w", err)
			}

			// Locate node in topology.
			nd := nodeDir(configDir, nodeName)
			token, tokenErr := loadToken(nd)
			if tokenErr != nil {
				return fmt.Errorf("load token for %q: %w", nodeName, tokenErr)
			}

			// Determine gRPC address.
			var grpcAddr string
			master := topo.FindMaster(nodeName)
			ep := topo.FindEndpoint(nodeName)
			switch {
			case master != nil:
				grpcAddr = master.GRPCAddr()
			case ep != nil:
				grpcAddr = ep.GRPCAddr()
			default:
				return fmt.Errorf("node %q not found in topology (not a master or endpoint)", nodeName)
			}

			client, err := grpcclient.NewClient(grpcclient.ClientConfig{
				Target:     grpcAddr,
				CACertPath: caPath(configDir),
				Token:      token,
			})
			if err != nil {
				return fmt.Errorf("connect to %q: %w", nodeName, err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client for %s: %v\n", nodeName, closeErr)
				}
			}()

			// Fetch node transport state (disk+runtime combined).
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			tsResp, tsErr := client.Agent().GetTransportState(ctx, &proto.Empty{})
			cancel()

			if tsErr != nil {
				if code := status.Code(tsErr); code == codes.Unimplemented {
					fmt.Fprintf(os.Stderr, "node %q is running pre-v1.10.1 — upgrade node to use 'inspect'\n", nodeName)
					return nil
				}
				return fmt.Errorf("GetTransportState for %q: %w", nodeName, tsErr)
			}

			// Build admin expected view.
			adminPeers := buildAdminView(topo, nodeName, configDir, master, ep)

			// Build node-reported peer map (keyed by pubkey hex).
			nodePeersByKey := make(map[string]*proto.TransportPeerState, len(tsResp.GetPeers()))
			for _, p := range tsResp.GetPeers() {
				nodePeersByKey[p.PublicKeyHex] = p
			}

			// Drift analysis: admin view vs node-reported.
			var rows []driftRow
			seenNodeKeys := make(map[string]bool)

			for _, ap := range adminPeers {
				row := driftRow{
					peerName: ap.name,
					adminKey: ap.pubkeyHex,
					adminIPs: strings.Join(ap.allowedIPs, ","),
				}

				np, found := nodePeersByKey[ap.pubkeyHex]
				if !found {
					// Try to find a peer with the same name but different key (key_mismatch).
					for _, candidate := range tsResp.GetPeers() {
						if candidate.Name == ap.name {
							np = candidate
							break
						}
					}
				}

				if np == nil {
					row.driftReasons = append(row.driftReasons, "missing_peer")
				} else {
					seenNodeKeys[np.PublicKeyHex] = true
					row.diskKey = np.PublicKeyHex
					row.runtimeKey = np.PublicKeyHex // GetTransportState merges disk+runtime
					row.diskIPs = strings.Join(np.AllowedIps, ",")
					row.runtimeIPs = row.diskIPs

					if ap.pubkeyHex != "" && np.PublicKeyHex != ap.pubkeyHex {
						row.driftReasons = append(row.driftReasons, "key_mismatch")
					}
					if ap.allowedIPs != nil && len(ap.allowedIPs) > 0 {
						if !ipsMatch(ap.allowedIPs, np.AllowedIps) {
							row.driftReasons = append(row.driftReasons, "stale_allowed_ips")
						}
					}
				}

				rows = append(rows, row)
			}

			// Extra peers: present on node but not in admin view.
			for _, np := range tsResp.GetPeers() {
				if !seenNodeKeys[np.PublicKeyHex] {
					rows = append(rows, driftRow{
						peerName:     np.Name,
						diskKey:      np.PublicKeyHex,
						runtimeKey:   np.PublicKeyHex,
						diskIPs:      strings.Join(np.AllowedIps, ","),
						runtimeIPs:   strings.Join(np.AllowedIps, ","),
						driftReasons: []string{"extra_peer"},
					})
				}
			}

			// Print report.
			hasDrift := printInspectReport(nodeName, tsResp, rows)

			if hasDrift {
				return fmt.Errorf("drift detected on node %q", nodeName)
			}
			return nil
		},
	}
}

// buildAdminView builds admin's expected peer list for a given node.
// For masters: each endpoint bound to this master is a peer (admin knows the endpoint's pubkey).
// For endpoints: each master the endpoint is bound to is a peer.
func buildAdminView(
	topo *topology.Topology,
	nodeName string,
	cfgDir string,
	master *topology.MasterNode,
	ep *topology.EndpointNode,
) []adminPeerView {
	var peers []adminPeerView

	if master != nil {
		// Admin expects one peer per bound endpoint.
		for _, epName := range master.Endpoints {
			epNode := topo.FindEndpoint(epName)
			if epNode == nil {
				continue
			}
			pubkeyHex := readAdminPubkey(cfgDir, epName)
			peers = append(peers, adminPeerView{
				name:      epName,
				pubkeyHex: pubkeyHex,
				allowedIPs: []string{epNode.OverlayIP + "/32"},
			})
		}
	} else if ep != nil {
		// Admin expects one peer per master that this endpoint is bound to.
		for _, m := range topo.Masters {
			if !containsName(m.Endpoints, nodeName) {
				continue
			}
			pubkeyHex := readAdminPubkey(cfgDir, m.Name)
			peers = append(peers, adminPeerView{
				name:      m.Name,
				pubkeyHex: pubkeyHex,
				allowedIPs: []string{m.OverlayIP + "/32"},
			})
		}
	}

	return peers
}

// readAdminPubkey reads the 32-byte raw public key file saved by 'endpoint init' / 'master init'
// and returns it as lowercase hex. Returns empty string if the file is missing or malformed.
func readAdminPubkey(cfgDir, name string) string {
	path := nodeDir(cfgDir, name) + "/pubkey"
	data, err := os.ReadFile(path)
	if err != nil {
		return "" // not yet initialized
	}
	// pubkey file is raw 32-byte WireGuard public key (written by resp.NodePublicKey).
	if len(data) == 32 {
		return hex.EncodeToString(data)
	}
	// Some callers write hex-encoded pubkeys; support both.
	if len(data) == 64 {
		return strings.ToLower(strings.TrimSpace(string(data)))
	}
	return ""
}

// ipsMatch returns true when expected and actual contain the same set of CIDRs
// (order-independent). Empty expected slice = no admin expectation → always match.
func ipsMatch(expected, actual []string) bool {
	if len(expected) == 0 {
		return true
	}
	if len(expected) != len(actual) {
		return false
	}
	set := make(map[string]bool, len(expected))
	for _, ip := range expected {
		set[strings.TrimSpace(ip)] = true
	}
	for _, ip := range actual {
		if !set[strings.TrimSpace(ip)] {
			return false
		}
	}
	return true
}

// printInspectReport renders the 3-column drift report. Returns true if drift was found.
func printInspectReport(nodeName string, state *proto.TransportStateResponse, rows []driftRow) bool {
	fmt.Printf("Node: %s  Mode: %s  Overlay: %s\n\n", nodeName, state.GetMode(), state.GetOverlayIp())

	if len(rows) == 0 {
		fmt.Println("No peers configured — nothing to inspect.")
		return false
	}

	const (
		colWidth  = 18
		ipWidth   = 22
	)

	// Header.
	fmt.Printf("%-20s %-20s %-20s %-20s %-24s %-24s %-24s %s\n",
		"PEER", "ADMIN_KEY", "NODE_KEY", "RUNTIME_KEY",
		"ADMIN_IPS", "DISK_IPS", "RUNTIME_IPS", "STATUS")
	fmt.Println(strings.Repeat("-", 160))

	hasDrift := false
	for _, r := range rows {
		statusStr := "OK"
		if len(r.driftReasons) > 0 {
			statusStr = "DRIFT: " + strings.Join(r.driftReasons, ",")
			hasDrift = true
		}

		adminKey := truncate(r.adminKey, colWidth)
		diskKey := truncate(r.diskKey, colWidth)
		runtimeKey := truncate(r.runtimeKey, colWidth)
		adminIPs := truncate(r.adminIPs, ipWidth)
		diskIPs := truncate(r.diskIPs, ipWidth)
		runtimeIPs := truncate(r.runtimeIPs, ipWidth)

		fmt.Printf("%-20s %-20s %-20s %-20s %-24s %-24s %-24s %s\n",
			truncate(r.peerName, 20),
			adminKey, diskKey, runtimeKey,
			adminIPs, diskIPs, runtimeIPs,
			statusStr)
	}

	fmt.Println()
	if hasDrift {
		fmt.Printf("RESULT: drift detected on %s\n", nodeName)
	} else {
		fmt.Printf("RESULT: %s is in sync\n", nodeName)
	}

	return hasDrift
}

// truncate shortens s to at most n characters, adding "…" suffix when truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return s[:n-1] + "…"
}
