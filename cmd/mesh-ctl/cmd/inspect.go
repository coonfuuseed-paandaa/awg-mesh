package cmd

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	diskKey      string // hex pubkey from node's transport.yml (disk state)
	runtimeKey   string // hex pubkey from live wg/tunnel state (runtime)
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
  key_mismatch          — peer public key differs between admin and runtime
  admin_pubkey_missing  — admin pubkey file missing; re-run 'endpoint init' or restore pubkey
  disk_runtime_diverge  — disk key (transport.yml) differs from live runtime key
  missing_peer          — peer present in admin view but absent from node
  stale_allowed_ips     — allowed IPs do not match admin expectation
  extra_peer            — peer present on node but not in admin view

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

			// Build node-reported peer maps keyed by runtime pubkey and by name.
			nodePeersByKey := make(map[string]*proto.TransportPeerState, len(tsResp.GetPeers()))
			nodePeersByName := make(map[string]*proto.TransportPeerState, len(tsResp.GetPeers()))
			for _, p := range tsResp.GetPeers() {
				nodePeersByKey[p.PublicKeyHex] = p
				nodePeersByName[p.Name] = p
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

				// Locate node peer: prefer exact key match, fall back to name match.
				np := nodePeersByKey[ap.pubkeyHex]
				if np == nil {
					np = nodePeersByName[ap.name]
				}

				if np == nil {
					row.driftReasons = append(row.driftReasons, "missing_peer")
				} else {
					seenNodeKeys[np.PublicKeyHex] = true

					// Populate disk vs runtime columns from the split fields (v1.10.1+).
					// For pre-split nodes DiskPublicKeyHex is empty; fall back to PublicKeyHex.
					diskKey := np.GetDiskPublicKeyHex()
					if diskKey == "" {
						diskKey = np.PublicKeyHex
					}
					diskIPs := np.GetDiskAllowedIps()
					if len(diskIPs) == 0 {
						diskIPs = np.AllowedIps
					}

					row.diskKey = diskKey
					row.runtimeKey = np.PublicKeyHex
					row.diskIPs = strings.Join(diskIPs, ",")
					row.runtimeIPs = strings.Join(np.AllowedIps, ",")

					// Fix C: distinguish missing admin pubkey from key_mismatch.
					switch {
					case ap.pubkeyHex == "":
						// readAdminPubkey returned empty — pubkey file missing or unreadable.
						row.driftReasons = append(row.driftReasons, "admin_pubkey_missing")
					case np.PublicKeyHex != ap.pubkeyHex:
						row.driftReasons = append(row.driftReasons, "key_mismatch")
					}

					// Fix B: report disk/runtime key divergence when they differ.
					if diskKey != np.PublicKeyHex {
						row.driftReasons = append(row.driftReasons, "disk_runtime_diverge")
					}

					if len(ap.allowedIPs) > 0 {
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
					diskKey := np.GetDiskPublicKeyHex()
					if diskKey == "" {
						diskKey = np.PublicKeyHex
					}
					diskIPs := np.GetDiskAllowedIps()
					if len(diskIPs) == 0 {
						diskIPs = np.AllowedIps
					}
					rows = append(rows, driftRow{
						peerName:     np.Name,
						diskKey:      diskKey,
						runtimeKey:   np.PublicKeyHex,
						diskIPs:      strings.Join(diskIPs, ","),
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
// The empty-string sentinel is used by the caller to distinguish admin_pubkey_missing (empty)
// from key_mismatch (non-empty but differs from node key).
func readAdminPubkey(cfgDir, name string) string {
	path := filepath.Join(nodeDir(cfgDir, name), "pubkey")
	data, err := os.ReadFile(path)
	if err != nil {
		return "" // not yet initialized — caller reports admin_pubkey_missing
	}
	// Fix D: trim trailing whitespace/newlines (common editor behaviour) BEFORE length check.
	// A file written as "<64 hex chars>\n" is 65 bytes and would fail len==64 without trimming.
	trimmed := strings.TrimSpace(string(data))
	// pubkey file is raw 32-byte WireGuard public key (written by resp.NodePublicKey).
	if len(data) == 32 {
		return hex.EncodeToString(data)
	}
	// Some callers write hex-encoded pubkeys; support both with and without trailing newline.
	if len(trimmed) == 64 {
		return strings.ToLower(trimmed)
	}
	return ""
}

// ipsMatch returns true when expected and actual contain the same unique set of CIDRs
// (order-independent, duplicate-tolerant). Empty expected slice = no admin expectation → always match.
//
// Fix E: converts both slices to sorted unique sets before comparing so that
// duplicates (e.g. expected=[a,b], actual=[a,a]) are correctly detected as mismatches.
func ipsMatch(expected, actual []string) bool {
	if len(expected) == 0 {
		return true
	}
	return sortedUniqueIPs(expected) == sortedUniqueIPs(actual)
}

// sortedUniqueIPs returns a canonical comma-joined string of the unique, sorted, trimmed CIDRs
// in s. Used by ipsMatch to detect duplicates and order differences in a single comparison.
func sortedUniqueIPs(s []string) string {
	seen := make(map[string]struct{}, len(s))
	for _, ip := range s {
		seen[strings.TrimSpace(ip)] = struct{}{}
	}
	unique := make([]string, 0, len(seen))
	for ip := range seen {
		unique = append(unique, ip)
	}
	sort.Strings(unique)
	return strings.Join(unique, ",")
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
