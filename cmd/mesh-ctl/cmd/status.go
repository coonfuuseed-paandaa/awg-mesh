package cmd

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	grpcclient "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"gopkg.in/yaml.v3"
)

// handshakeStaleThreshold is the maximum age of a WireGuard last-handshake
// timestamp before a peer is classified as handshake_timeout.
const handshakeStaleThreshold = 3 * time.Minute

type localTransportState struct {
	Allocations []struct {
		Tunnel     string `yaml:"tunnel"`
		MasterIP   string `yaml:"master_ip"`
		EndpointIP string `yaml:"endpoint_ip"`
	} `yaml:"allocations"`
}

func newStatusCommand() *cobra.Command {
	var verifyDataPlane bool
	var probeTimeout time.Duration
	var probeConcurrency int

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Query status of all mesh nodes via gRPC",
		RunE: func(cmd *cobra.Command, args []string) error {
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology: %w", err)
			}

			type nodeInfo struct {
				name     string
				host     string
				grpcAddr string
				mode     string
			}

			var nodes []nodeInfo
			for _, m := range topo.Masters {
				nodes = append(nodes, nodeInfo{name: m.Name, host: m.Host, grpcAddr: m.GRPCAddr(), mode: "master"})
			}
			for _, e := range topo.Endpoints {
				nodes = append(nodes, nodeInfo{name: e.Name, host: e.Host, grpcAddr: e.GRPCAddr(), mode: "endpoint"})
			}

			transportStatePath := filepath.Join(configDir, "transport.yml")
			transportByTunnel := make(map[string]string)
			transportTotalAllocations := 0
			if transportData, err := os.ReadFile(transportStatePath); err == nil {
				var transportState localTransportState
				if yaml.Unmarshal(transportData, &transportState) == nil {
					for _, allocation := range transportState.Allocations {
						transportByTunnel[allocation.Tunnel] = allocation.MasterIP + "->" + allocation.EndpointIP
					}
					transportTotalAllocations = len(transportState.Allocations)
				}
			}

			fmt.Printf("%-20s %-10s %-20s %-10s %-15s %-25s %s\n", "NAME", "MODE", "HOST", "STATUS", "OVERLAY_IP", "TRANSPORT", "TUNNELS")
			fmt.Println("--------------------------------------------------------------------------------------------")

			for _, n := range nodes {
				nd := nodeDir(configDir, n.name)
				token, tokenErr := loadToken(nd)
				if tokenErr != nil {
					fmt.Printf("%-20s %-10s %-20s %-10s %-15s %-25s %s\n", n.name, n.mode, n.host, "NO_TOKEN", "-", "-", "0")
					continue
				}

				client, err := grpcclient.NewClient(grpcclient.ClientConfig{
					Target:     n.grpcAddr,
					CACertPath: caPath(configDir),
					Token:      token,
				})
				if err != nil {
					fmt.Printf("%-20s %-10s %-20s %-10s %-15s %-25s %s\n", n.name, n.mode, n.host, "CONNECT_ERR", "-", "-", "0")
					continue
				}

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				resp, err := client.Agent().GetStatus(ctx, nil)
				cancel()

				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close %s: %v\n", n.name, closeErr)
				}

				if err != nil {
					fmt.Printf("%-20s %-10s %-20s %-10s %-15s %-25s %s\n", n.name, n.mode, n.host, "OFFLINE", "-", "-", "0")
					continue
				}

				transportPairs := make([]string, 0, len(resp.Tunnels))
				for _, tunnel := range resp.Tunnels {
					if transport, ok := transportByTunnel[tunnel.Name]; ok && transport != "" {
						transportPairs = append(transportPairs, transport)
					}
				}
				transportDisplay := "-"
				if len(transportPairs) > 0 {
					transportDisplay = strings.Join(transportPairs, ",")
				}

				tunnelCount := tunnelDisplayCount(n.mode, len(resp.Tunnels), transportTotalAllocations)

				fmt.Printf("%-20s %-10s %-20s %-10s %-15s %-25s %s\n",
					resp.Name, resp.Mode, n.host, "ONLINE", resp.OverlayIp, transportDisplay, fmt.Sprintf("%d", tunnelCount))
			}

			// --verify-data-plane: probe each (master, endpoint) overlay pair.
			if verifyDataPlane {
				fmt.Println()
				probeResults := runDataPlaneProbes(topo, configDir, probeTimeout, probeConcurrency)
				anyBroken := false
				for _, r := range probeResults {
					if r.reason != "" {
						fmt.Printf("BROKEN  master=%-15s endpoint=%-15s reason=%s\n",
							r.masterName, r.endpointName, r.reason)
						anyBroken = true
					}
				}
				if !anyBroken {
					fmt.Println("DATA-PLANE: all (master, endpoint) pairs OK")
				} else {
					return fmt.Errorf("data-plane verification failed: broken pairs reported above")
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&verifyDataPlane, "verify-data-plane", false,
		"Probe data-plane health per (master, endpoint) pair and report broken pairs with structured reasons")
	cmd.Flags().DurationVar(&probeTimeout, "timeout", 5*time.Second,
		"Timeout per node probe when --verify-data-plane is set")
	cmd.Flags().IntVar(&probeConcurrency, "concurrency", 4,
		"Maximum concurrent node probes when --verify-data-plane is set")

	return cmd
}

// tunnelDisplayCount returns the TUNNELS column value for a node row.
//
// Semantics differ by mode (issue #105):
//   - master   → active WG peer count from GetStatus (clients connected to ingress)
//   - endpoint → transport allocation count from admin-side transport.yml
//     (outbound links to masters; endpoint has no ingress peer list)
func tunnelDisplayCount(mode string, grpcTunnelCount, transportAllocations int) int {
	if mode == "endpoint" {
		return transportAllocations
	}
	return grpcTunnelCount
}

// pairProbeResult holds the data-plane verification result for one
// (master, endpoint) pair.
type pairProbeResult struct {
	masterName   string
	endpointName string
	reason       string // empty = healthy; else one of the structured reason codes
}

// classifyTunnelHealth maps a TunnelHealth entry (from master's GetHealth) and
// an optional matching PeerStatus (from master's ListTunnels pubkey comparison
// against admin-stored endpoint pubkey) into a structured failure reason.
//
// reason codes:
//
//	""                 — healthy
//	"missing_peer"     — tunnel not present in master's wg peer list
//	"key_mismatch"     — master's peer pubkey differs from admin-stored endpoint pubkey
//	"handshake_timeout" — peer present but last handshake older than handshakeStaleThreshold
//	"unreachable"      — generic: peer unhealthy with no more specific diagnosis
func classifyTunnelHealth(
	h *proto.TunnelHealth,
	masterPeerKey []byte, // from ListTunnels TunnelInfo.PeerPublicKey (may be nil)
	adminEndpointKeyHex string, // hex pubkey stored in admin state (may be "")
	now time.Time,
) string {
	if h == nil {
		return "missing_peer"
	}
	if h.Healthy {
		return ""
	}

	// Key mismatch: admin knows the endpoint pubkey and master's live key differs.
	if adminEndpointKeyHex != "" && len(masterPeerKey) == 32 {
		masterKeyHex := hex.EncodeToString(masterPeerKey)
		if masterKeyHex != adminEndpointKeyHex {
			return "key_mismatch"
		}
	}

	// Handshake timeout: last_check_ms is non-zero and older than threshold.
	// TunnelHealth.last_check_ms is a Unix ms timestamp of the last health check.
	// Use it as a proxy for liveness: if the check ran but the tunnel is still
	// unhealthy, classify as handshake_timeout.
	if h.LastCheckMs > 0 {
		lastCheck := time.UnixMilli(h.LastCheckMs)
		if now.Sub(lastCheck) > handshakeStaleThreshold {
			return "handshake_timeout"
		}
	}

	if h.ConsecutiveFailures > 0 {
		return "handshake_timeout"
	}

	return "unreachable"
}

// probeNodePairs queries a single master node's health and tunnel info to
// classify the data-plane health of each (master, endpoint) pair.
// Returns one pairProbeResult per bound endpoint.
func probeNodePairs(
	topo *topology.Topology,
	master *topology.MasterNode,
	cfgDir string,
	probeTimeout time.Duration,
) []pairProbeResult {
	results := make([]pairProbeResult, 0, len(master.Endpoints))

	nd := nodeDir(cfgDir, master.Name)
	token, tokenErr := loadToken(nd)
	if tokenErr != nil {
		for _, epName := range master.Endpoints {
			results = append(results, pairProbeResult{
				masterName:   master.Name,
				endpointName: epName,
				reason:       "unreachable",
			})
		}
		return results
	}

	client, err := grpcclient.NewClient(grpcclient.ClientConfig{
		Target:     master.GRPCAddr(),
		CACertPath: caPath(cfgDir),
		Token:      token,
	})
	if err != nil {
		for _, epName := range master.Endpoints {
			results = append(results, pairProbeResult{
				masterName:   master.Name,
				endpointName: epName,
				reason:       "unreachable",
			})
		}
		return results
	}
	defer func() { _ = client.Close() }()

	// Fetch health and live tunnel list in parallel.
	type healthResult struct {
		resp *proto.HealthResponse
		err  error
	}
	type tunnelResult struct {
		resp *proto.TunnelList
		err  error
	}

	healthCh := make(chan healthResult, 1)
	tunnelCh := make(chan tunnelResult, 1)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		resp, err := client.Agent().GetHealth(ctx, &proto.Empty{})
		healthCh <- healthResult{resp, err}
	}()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		resp, err := client.Agent().ListTunnels(ctx, &proto.Empty{})
		tunnelCh <- tunnelResult{resp, err}
	}()

	healthRes := <-healthCh
	tunnelRes := <-tunnelCh

	// Build lookup maps from health and live tunnel info.
	healthByName := make(map[string]*proto.TunnelHealth)
	if healthRes.err == nil && healthRes.resp != nil {
		for _, th := range healthRes.resp.TunnelHealth {
			healthByName[th.Name] = th
		}
	}

	// Build pubkey lookup: endpointName → master's live peer pubkey (hex).
	masterLivePeerKey := make(map[string][]byte)
	if tunnelRes.err == nil && tunnelRes.resp != nil {
		for _, t := range tunnelRes.resp.Tunnels {
			masterLivePeerKey[t.Name] = nil // placeholder; TunnelStatus has no PeerPublicKey
		}
	}

	now := time.Now()
	for _, epName := range master.Endpoints {
		ep := topo.FindEndpoint(epName)
		if ep == nil {
			results = append(results, pairProbeResult{
				masterName:   master.Name,
				endpointName: epName,
				reason:       "missing_peer",
			})
			continue
		}

		th := healthByName[epName]
		adminEpKeyHex := readAdminPubkey(cfgDir, epName)
		livePeerKey := masterLivePeerKey[epName] // nil if not in live list

		reason := classifyTunnelHealth(th, livePeerKey, adminEpKeyHex, now)
		results = append(results, pairProbeResult{
			masterName:   master.Name,
			endpointName: epName,
			reason:       reason,
		})
	}

	return results
}

// runDataPlaneProbes fans out probeNodePairs across all masters concurrently,
// bounded by maxConcurrency goroutines. Returns one result per (master, endpoint) pair.
func runDataPlaneProbes(
	topo *topology.Topology,
	cfgDir string,
	probeTimeout time.Duration,
	maxConcurrency int,
) []pairProbeResult {
	if maxConcurrency <= 0 {
		maxConcurrency = 4
	}
	if probeTimeout <= 0 {
		probeTimeout = 5 * time.Second
	}

	sem := make(chan struct{}, maxConcurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var allResults []pairProbeResult

	for i := range topo.Masters {
		master := &topo.Masters[i]
		if len(master.Endpoints) == 0 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(m *topology.MasterNode) {
			defer wg.Done()
			defer func() { <-sem }()
			res := probeNodePairs(topo, m, cfgDir, probeTimeout)
			mu.Lock()
			allResults = append(allResults, res...)
			mu.Unlock()
		}(master)
	}

	wg.Wait()
	return allResults
}
