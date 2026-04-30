package cmd

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/cmd/mesh-ctl/internal/adminstate"
	grpcclient "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newEndpointCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "endpoint",
		Short: "Manage endpoint nodes",
	}

	cmd.AddCommand(newEndpointPrepareCommand())
	cmd.AddCommand(newEndpointInitCommand())
	cmd.AddCommand(newEndpointRemoveCommand())

	return cmd
}

func newEndpointPrepareCommand() *cobra.Command {
	var useTraefik bool
	var showToken bool
	var imageFlag string

	cmd := &cobra.Command{
		Use:   "prepare [name]",
		Short: "Generate docker-compose and token for an endpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			// Validate --image early so the user gets a clear error before any
			// expensive operations (topology load, CA, token generation).
			if imageFlag != "" {
				if err := validateImageRef(imageFlag); err != nil {
					return fmt.Errorf("invalid --image: %w", err)
				}
			}

			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			ep := topo.FindEndpoint(name)
			if ep == nil {
				return fmt.Errorf("endpoint %q not found in topology", name)
			}

			_, _, err = ensureCA(configDir)
			if err != nil {
				return fmt.Errorf("ensure CA: %w", err)
			}

			token, err := pkgtls.GenerateToken()
			if err != nil {
				return fmt.Errorf("generate token: %w", err)
			}

			hash, err := pkgtls.HashToken(token)
			if err != nil {
				return fmt.Errorf("hash token: %w", err)
			}

			nd := nodeDir(configDir, name)
			if err := pkgtls.SaveTokenHash(nd, hash); err != nil {
				return fmt.Errorf("save token hash: %w", err)
			}

			if err := saveToken(nd, token); err != nil {
				return fmt.Errorf("save token: %w", err)
			}

			data := struct {
				Name       string
				Host       string
				OverlayIP  string
				Image      string
				ListenPort int
				TokenHash  string
			}{
				Name:       ep.Name,
				Host:       ep.Host,
				OverlayIP:  ep.OverlayIP,
				Image:      resolveImage(imageFlag, topo.Defaults.Image.Node, "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:latest", "defaults.image.node"),
				ListenPort: ep.ListenPort,
				TokenHash:  hash,
			}

			templateName := "docker-compose.endpoint.yml.tmpl"
			if useTraefik {
				templateName = "docker-compose.endpoint.traefik.yml.tmpl"
			}

			endpointTemplate, err := loadTemplate(templateName)
			if err != nil {
				return fmt.Errorf("load endpoint compose template: %w", err)
			}
			// B3 fix: write compose to configDir/nodes/<name>/ (co-located with
			// token and pubkey) instead of CWD, which scattered files across
			// whichever directory the operator happened to be in.
			outputPath := filepath.Join(nd, ep.Name+"-docker-compose.yml")
			if err := renderDockerCompose(endpointTemplate, data, outputPath); err != nil {
				return fmt.Errorf("render docker-compose: %w", err)
			}

			tokenPath := filepath.Join(nd, "token")
			printNextSteps("endpoint", name, token, tokenPath, outputPath, useTraefik, showToken)
			return nil
		},
	}

	cmd.Flags().BoolVar(&useTraefik, "traefik", false, "Generate Traefik-compatible compose with labels (no host networking)")
	cmd.Flags().BoolVar(&showToken, "show-token", false, "print raw token to stdout (default: save to disk only)")
	cmd.Flags().StringVar(&imageFlag, "image", "", "Docker image reference (default: topology defaults.image.node, else ghcr.io/coonfuuseed-paandaa/awg-mesh-node:latest)")
	return cmd
}

func newEndpointInitCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "init [name]",
		Short: "Initialize endpoint via gRPC — exchange certs, configure node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			ep := topo.FindEndpoint(name)
			if ep == nil {
				return fmt.Errorf("endpoint %q not found in topology", name)
			}

			caCert, caKey, err := pkgtls.LoadCA(configDir)
			if err != nil {
				return fmt.Errorf("load CA: %w", err)
			}

			certPEM, keyPEM, err := pkgtls.IssueCert(caCert, caKey, ep.Name, []string{ep.Host})
			if err != nil {
				return fmt.Errorf("issue cert: %w", err)
			}

			caCertPEM, err := os.ReadFile(caPath(configDir))
			if err != nil {
				return fmt.Errorf("read CA cert: %w", err)
			}

			nd := nodeDir(configDir, name)
			token, err := loadToken(nd)
			if err != nil {
				return fmt.Errorf("load token: %w", err)
			}

			client, err := grpcclient.NewClient(grpcclient.ClientConfig{
				Target:   ep.GRPCAddr(),
				Token:    token,
				Insecure: true, // pre-Init: server has no CA-signed cert yet
			})
			if err != nil {
				return fmt.Errorf("create gRPC client: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client: %v\n", closeErr)
				}
			}()

			initCtx, initCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer initCancel()

			resp, err := client.Agent().Init(initCtx, &proto.InitRequest{
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
			if err != nil {
				return fmt.Errorf("init RPC: %w", err)
			}
			if !resp.Success {
				return fmt.Errorf("init failed: %s", resp.Message)
			}

			// newPubKeyHex is the hex-encoded 32-byte WireGuard public key returned by Init.
			// We hold it in the outer scope so it is available after the adminstate transaction.
			newPubKeyHex := hex.EncodeToString(resp.NodePublicKey)

			alloc, err := loadOrCreateAllocator(configDir, topo)
			if err != nil {
				return fmt.Errorf("load transport allocator: %w", err)
			}

			selfClient, err := grpcclient.NewClient(grpcclient.ClientConfig{
				Target:   ep.GRPCAddr(),
				Token:    token,
				Insecure: true,
			})
			if err != nil {
				return fmt.Errorf("create self gRPC client: %w", err)
			}
			defer func() {
				if closeErr := selfClient.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close self gRPC client: %v\n", closeErr)
				}
			}()

			var balancerIP string
			if epAddr, parseErr := netip.ParseAddr(ep.OverlayIP); parseErr == nil {
				parsedRanges := make([]topology.Range, 0, len(topo.Overlay.Ranges))
				for _, nr := range topo.Overlay.Ranges {
					if r, rErr := topology.ParseRange(nr); rErr == nil {
						parsedRanges = append(parsedRanges, r)
					}
				}
				if bip := topology.BalancerIPForAddr(parsedRanges, epAddr); bip.IsValid() {
					balancerIP = bip.String()
				}
			}

			// needAddPeerForMaster tracks which masters require AddPeer on the endpoint
			// side after the adminstate transaction completes.
			type addPeerWork struct {
				masterName  string
				masterToken string
				masterAddr  string
				masterKey   []byte
				allowedIPs  []string
				subnet      string
				epTransIP   string
				masterIP    string
			}
			var pendingAddPeers []addPeerWork

			// F-005 fix: pre-compute the alphabetical list of masters bound to this
			// endpoint. The endpoint creates per-master ifaces lazily via AddPeer
			// in deterministic order, each on listen_port = config.ListenPort +
			// count_of_existing_ifaces. So iface for master M listens on
			// ep.ListenPort + portOffset(boundMasters, M.Name). The master's tunnel
			// peer_endpoint MUST point at THAT port — using ep.PeerAddr() (topology
			// default port only) sends every master's WireGuard handshakes to the
			// FIRST iface (alphabetical[0]), which has a different keypair → drop.
			// Latent because data-plane reachability tests only exercise paths via
			// alphabetical-first master; F-005 fixture surfaces it via ECMP that
			// hashes through master-02.
			boundMasterNames := make([]string, 0, len(topo.Masters))
			for _, m := range topo.Masters {
				if containsName(m.Endpoints, ep.Name) {
					boundMasterNames = append(boundMasterNames, m.Name)
				}
			}

			// FR-2: adminstate.SetPubkey issues all master RPCs inside the callback.
			// The pubkey file is written ONLY after every master acknowledges the new key.
			// If any master fails, the callback returns an error and the file is untouched.
			store := adminstate.NewStore(configDir)
			_, txnErr := store.SetPubkey(ep.Name, func(oldKeyHex string) (string, error) {
				// T010: track per-master outcome counters inside the transaction.
				mastersTotal := 0
				mastersOk := 0
				var failedMasters []string

				for _, master := range topo.Masters {
					if !containsName(master.Endpoints, ep.Name) {
						continue
					}

					mastersTotal++

					allocation, allocErr := alloc.Allocate(master.Name, ep.Name)
					if allocErr != nil {
						fmt.Fprintf(os.Stderr, "warning: allocate transport for master %q and endpoint %q failed: %v\n", master.Name, ep.Name, allocErr)
						failedMasters = append(failedMasters, master.Name)
						continue
					}

					masterToken, tokenErr := loadToken(nodeDir(configDir, master.Name))
					if tokenErr != nil {
						fmt.Fprintf(os.Stderr, "warning: cannot load token for master %q: %v\n", master.Name, tokenErr)
						failedMasters = append(failedMasters, master.Name)
						continue
					}

					masterClient, connErr := grpcclient.NewClient(grpcclient.ClientConfig{
						Target:   master.GRPCAddr(),
						Token:    masterToken,
						Insecure: true,
					})
					if connErr != nil {
						fmt.Fprintf(os.Stderr, "warning: cannot connect to master %q: %v\n", master.Name, connErr)
						failedMasters = append(failedMasters, master.Name)
						continue
					}

					masterAllowedIPs, maipErr := topology.BuildAllowedIPsForMasterPeer(topo, ep.Name, ep.OverlayIP, allocation.Subnet.String())
					if maipErr != nil {
						fmt.Fprintf(os.Stderr, "warning: build master allowed_ips for endpoint %q / master %q: %v\n", ep.Name, master.Name, maipErr)
						masterAllowedIPs = []string{allocation.Subnet.String(), ep.OverlayIP + "/32"}
					}
					// F-005 fix: per-master endpoint host. ep.PeerAddr() returns
					// epHost:topology_listen_port — correct only for the alphabetical-
					// first master. For every other master M, the endpoint's
					// wg-{M.Name} iface listens on ep.ListenPort + portOffset(boundMasters, M.Name).
					epHost := ep.PeerHost
					if epHost == "" {
						epHost = ep.Host
					}
					perMasterEndpointHost := net.JoinHostPort(
						epHost,
						strconv.Itoa(ep.ListenPort+portOffset(boundMasterNames, master.Name)),
					)

					masterCtx, masterCancel := context.WithTimeout(context.Background(), 30*time.Second)
					addResp, addErr := masterClient.Agent().AddTunnel(masterCtx, &proto.AddTunnelRequest{
						Name:                ep.Name,
						EndpointHost:        perMasterEndpointHost,
						OverlayIp:           ep.OverlayIP,
						BalancerIp:          balancerIP,
						PeerPublicKey:       resp.NodePublicKey,
						Weight:              1,
						TransportSubnet:     allocation.Subnet.String(),
						MasterTransportIp:   allocation.MasterIP.String(),
						EndpointTransportIp: allocation.EndpointIP.String(),
						AllowedIps:          masterAllowedIPs,
					})
					masterCancel()
					if closeErr := masterClient.Close(); closeErr != nil {
						fmt.Fprintf(os.Stderr, "warning: close grpc client for master %q: %v\n", master.Name, closeErr)
					}

					statusLine := ""
					needAddPeer := false
					// updateResp is declared in outer scope so the post-branch
					// masterPubKey fallback can reference it. Nil when we took
					// the AddTunnel path (tunnel was created, not updated).
					var updateResp *proto.UpdateTunnelPeerResponse

					// v1.12.2 Pattern X (per-master iface): each endpoint-side iface has
					// exactly ONE peer, so AllowedIPs on this peer can safely include the
					// overlay /32 of other endpoints reachable via this master — there's
					// no dedup risk with a single peer. AllowedIPs = [transport_subnet,
					// master_overlay/32, other_endpoint_overlay/32 ...]. The /32 entries
					// also install kernel routes on the endpoint side via ConfigureTransport.
					allowedIPs, aipErr := topology.BuildMinimalAllowedIPsForEndpointPeer(topo, master.OverlayIP, allocation.Subnet.String())
					if aipErr != nil {
						fmt.Fprintf(os.Stderr, "warning: build allowed_ips for master %q / endpoint %q: %v\n", master.Name, ep.Name, aipErr)
						allowedIPs = []string{allocation.Subnet.String(), master.OverlayIP + "/32"}
					}
					allowedIPs = append(allowedIPs, computeExtraOverlayRoutes(topo, ep.Name, master.Name)...)
					fmt.Printf("endpoint init: AddPeer to endpoint %q (master %q) with allowed_ips=%v\n", ep.Name, master.Name, allowedIPs)

					if addErr != nil {
						errLower := strings.ToLower(addErr.Error())
						isAlreadyExists := status.Code(addErr) == codes.AlreadyExists || strings.Contains(errLower, "already exists")
						if !isAlreadyExists {
							statusLine = fmt.Sprintf("FAILED: %v", addErr)
							fmt.Printf("Tunnel %q on master %q: %s\n", ep.Name, master.Name, statusLine)
							failedMasters = append(failedMasters, master.Name)
							continue
						}

						// Tunnel already exists on master — update the peer key.
						masterClient2, conn2Err := grpcclient.NewClient(grpcclient.ClientConfig{
							Target:   master.GRPCAddr(),
							Token:    masterToken,
							Insecure: true,
						})
						if conn2Err != nil {
							statusLine = fmt.Sprintf("FAILED: cannot connect to master %q for tunnel update: %v", master.Name, conn2Err)
							fmt.Printf("Tunnel %q on master %q: %s\n", ep.Name, master.Name, statusLine)
							failedMasters = append(failedMasters, master.Name)
							continue
						}

						masterCtx2, masterCancel2 := context.WithTimeout(context.Background(), 30*time.Second)
						var updateErr error
						// Use masterAllowedIPs (master-side view: transport/30 + endpoint_overlay/32 +
						// endpoints/27) — NOT allowedIPs (endpoint-side view: transport/30 +
						// master_overlay/32). This is the admin source of truth for the master tunnel;
						// passing the wrong set would defeat the v1.12.8 fix for issue #147 layer 3.
						updateResp, updateErr = masterClient2.Agent().UpdateTunnelPeer(masterCtx2, &proto.UpdateTunnelPeerRequest{
							Name:          ep.Name,
							PeerPublicKey: resp.NodePublicKey,
							BalancerIp:    balancerIP,
							AllowedIps:    masterAllowedIPs,
						})
						masterCancel2()
						if closeErr := masterClient2.Close(); closeErr != nil {
							fmt.Fprintf(os.Stderr, "warning: close grpc client for master %q: %v\n", master.Name, closeErr)
						}

						if updateErr != nil {
							// T011: detect pre-v1.10.0 masters that lack UpdateTunnelPeer RPC.
							var isPreV110 bool
							statusLine, isPreV110 = updateTunnelPeerFailureStatus(updateErr)
							if isPreV110 {
								fmt.Fprintf(os.Stderr, "master %s running pre-v1.10.0 — upgrade master before rotating endpoint keys\n", master.Name)
							}
							fmt.Printf("Tunnel %q on master %q: %s\n", ep.Name, master.Name, statusLine)
							failedMasters = append(failedMasters, master.Name)
							continue
						}

						if updateResp == nil || !updateResp.Success {
							statusLine = "FAILED: update tunnel peer RPC returned unsuccessful response"
							fmt.Printf("Tunnel %q on master %q: %s\n", ep.Name, master.Name, statusLine)
							failedMasters = append(failedMasters, master.Name)
							continue
						}

						needAddPeer = !updateResp.Unchanged
						if updateResp.Unchanged {
							statusLine = "unchanged (key matches)"
						} else {
							statusLine = fmt.Sprintf("updated (new key: %s)", newPubKeyHex[:min(8, len(newPubKeyHex))])
						}
					} else {
						if addResp == nil || !addResp.Success {
							statusLine = "FAILED: add tunnel RPC returned unsuccessful response"
							fmt.Printf("Tunnel %q on master %q: %s\n", ep.Name, master.Name, statusLine)
							failedMasters = append(failedMasters, master.Name)
							continue
						}
						needAddPeer = true
						statusLine = "created"
					}

					mastersOk++
					fmt.Printf("Tunnel %q on master %q: %s\n", ep.Name, master.Name, statusLine)

					if needAddPeer {
						// Collect master public key for the post-transaction AddPeer call.
						// UpdateTunnelPeer path: use updateResp.MasterPublicKey when the key
						// actually changed (Unchanged==false). AddTunnel path: use addResp.
						var masterPubKey []byte
						if addResp != nil {
							masterPubKey = addResp.MasterPublicKey
						} else if updateResp != nil {
							masterPubKey = updateResp.MasterPublicKey
						}
						if len(masterPubKey) == 0 {
							// Fallback: read from admin-state disk (raw bytes or hex).
							masterPubKey, _ = readAdminPubkeyRaw(configDir, master.Name)
						}
						if len(masterPubKey) > 0 {
							// v1.12.2: compute extra kernel routes for other endpoints
							// reachable via this master (first master alphabetically that
							// v1.12.2: allowedIPs already includes other-endpoint /32s
							// appended via computeExtraOverlayRoutes at AllowedIPs build time.
							// The endpoint's ConfigureTransport handler installs kernel routes
							// for each allowedIPs entry; single-peer per-master iface makes
							// WG AllowedIPs dedup a non-issue.
							pendingAddPeers = append(pendingAddPeers, addPeerWork{
								masterName:  master.Name,
								masterToken: masterToken,
								masterAddr:  master.PeerAddr(),
								masterKey:   masterPubKey,
								allowedIPs:  allowedIPs,
								subnet:      allocation.Subnet.String(),
								epTransIP:   allocation.EndpointIP.String(),
								masterIP:    allocation.MasterIP.String(),
							})
						} else {
							fmt.Fprintf(os.Stderr, "warning: master %q public key not available, skipping peer setup\n", master.Name)
						}
					}
				}

				// FR-2: only return success if ALL masters acknowledged.
				if mastersOk < mastersTotal {
					return "", fmt.Errorf(
						"partial update: %d of %d master(s) failed %v — admin state unchanged, run 'mesh-ctl reconcile' once all masters are reachable",
						mastersTotal-mastersOk, mastersTotal, failedMasters,
					)
				}

				return newPubKeyHex, nil
			})

			if txnErr != nil {
				return fmt.Errorf("endpoint init: %w", txnErr)
			}

			// FR-2 complete: all masters acknowledged the new key and admin state is written.
			// Now set up AddPeer on the endpoint side for each master that created a new tunnel.
			for _, work := range pendingAddPeers {
				peerCtx, peerCancel := context.WithTimeout(context.Background(), 30*time.Second)
				peerResp, peerErr := selfClient.Agent().AddPeer(peerCtx, &proto.AddPeerRequest{
					PublicKey:           work.masterKey,
					AllowedIps:          work.allowedIPs,
					EndpointHost:        work.masterAddr,
					PersistentKeepalive: 25,
					TransportSubnet:     work.subnet,
					LocalTransportIp:    work.epTransIP,
					PeerTransportIp:     work.masterIP,
					PeerName:            work.masterName,
				})
				peerCancel()
				if peerErr != nil {
					fmt.Fprintf(os.Stderr, "warning: add peer on endpoint for master %q failed: %v\n", work.masterName, peerErr)
					continue
				}
				if peerResp == nil || !peerResp.Success {
					fmt.Fprintf(os.Stderr, "warning: add peer on endpoint for master %q failed: %s\n", work.masterName, "[RPC failure]")
				}
			}

			if err := saveTransportState(alloc, configDir); err != nil {
				return fmt.Errorf("save transport state: %w", err)
			}

			fmt.Printf("Endpoint %q initialized successfully.\nPublic key: %s\n", name, newPubKeyHex)
			return nil
		},
	}
}

// computeExtraOverlayRoutes returns the list of other endpoints' overlay IPs
// (as /32 CIDR strings) reachable from selfEndpointName via masterName.
// Only endpoints for which masterName is the first-alphabetically shared master
// are included, to avoid installing duplicate routes on multiple ifaces (v1.12.2
// endpoint-side Pattern X uses one master per peer, no ECMP). These routes are
// installed as kernel `ip route` on the endpoint without bloating WG AllowedIPs.
func computeExtraOverlayRoutes(topo *topology.Topology, selfEndpointName, masterName string) []string {
	if topo == nil {
		return nil
	}
	// Build map: endpoint name → sorted list of master names that bind it.
	epMasters := make(map[string][]string, len(topo.Endpoints))
	for _, m := range topo.Masters {
		for _, epName := range m.Endpoints {
			epMasters[epName] = append(epMasters[epName], m.Name)
		}
	}
	for k := range epMasters {
		sort.Strings(epMasters[k])
	}
	// Masters bound to self.
	selfMastersSet := make(map[string]struct{}, len(epMasters[selfEndpointName]))
	for _, m := range epMasters[selfEndpointName] {
		selfMastersSet[m] = struct{}{}
	}
	if _, ok := selfMastersSet[masterName]; !ok {
		return nil
	}
	var routes []string
	for _, peer := range topo.Endpoints {
		if peer.Name == selfEndpointName {
			continue
		}
		// Find the first master alphabetically that binds BOTH self and peer.
		var chosen string
		for _, m := range epMasters[peer.Name] {
			if _, shared := selfMastersSet[m]; shared {
				chosen = m
				break
			}
		}
		if chosen != masterName {
			continue
		}
		overlay := strings.TrimSpace(peer.OverlayIP)
		if overlay == "" {
			continue
		}
		if strings.Contains(overlay, "/") {
			overlay = strings.SplitN(overlay, "/", 2)[0]
		}
		routes = append(routes, overlay+"/32")
	}
	sort.Strings(routes)
	return routes
}

// readAdminPubkeyRaw reads the admin-state pubkey file for <name> and returns
// the raw 32-byte WireGuard public key.  Supports both storage formats:
//   - 32 raw bytes (legacy — written by pre-v1.11.2 endpoint/master init)
//   - 64 hex chars + optional newline (current — written by adminstate.SetPubkey)
//
// Returns nil if the file is missing, unreadable, or contains an unrecognised format.
func readAdminPubkeyRaw(cfgDir, name string) ([]byte, error) {
	path := filepath.Join(nodeDir(cfgDir, name), "pubkey")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Raw 32-byte format (legacy).
	if len(data) == 32 {
		return data, nil
	}
	// Hex format: 64 chars (+ optional "\n").
	trimmed := strings.TrimSpace(string(data))
	if len(trimmed) == 64 {
		b, hexErr := hex.DecodeString(trimmed)
		if hexErr != nil {
			return nil, fmt.Errorf("decode hex pubkey for %q: %w", name, hexErr)
		}
		return b, nil
	}
	return nil, fmt.Errorf("pubkey for %q has unexpected length %d (want 32 bytes or 64 hex chars)", name, len(data))
}

// updateTunnelPeerFailureStatus returns the human-readable failure status line
// and whether the error is due to a pre-v1.10.0 master (codes.Unimplemented).
// Detection uses the typed gRPC status code — never string-matching on the
// error message. This is a pure function extracted for testability (T011).
func updateTunnelPeerFailureStatus(err error) (statusLine string, isPreV110 bool) {
	if status.Code(err) == codes.Unimplemented {
		return "FAILED: master running pre-v1.10.0", true
	}
	return fmt.Sprintf("FAILED: %v", err), false
}

func newEndpointRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove [name]",
		Short: "Remove an endpoint from the mesh",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			ep := topo.FindEndpoint(name)
			if ep == nil {
				return fmt.Errorf("endpoint %q not found in topology", name)
			}

			for _, master := range topo.Masters {
				if !containsName(master.Endpoints, ep.Name) {
					continue
				}

				masterToken, err := loadToken(nodeDir(configDir, master.Name))
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: cannot load token for master %q: %v\n", master.Name, err)
					continue
				}

				masterClient, err := grpcclient.NewClient(grpcclient.ClientConfig{
					Target:   master.GRPCAddr(),
					Token:    masterToken,
					Insecure: true,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: cannot connect to master %q: %v\n", master.Name, err)
					continue
				}

				masterCtx, masterCancel := context.WithTimeout(context.Background(), 30*time.Second)
				removeResp, removeErr := masterClient.Agent().RemoveTunnel(masterCtx, &proto.RemoveTunnelRequest{
					Name: ep.Name,
				})
				masterCancel()
				if closeErr := masterClient.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client for master %q: %v\n", master.Name, closeErr)
				}

				if removeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: remove tunnel from master %q failed: %v\n", master.Name, removeErr)
					continue
				}

				if !removeResp.Success {
					fmt.Fprintf(os.Stderr, "warning: remove tunnel from master %q failed: %s\n", master.Name, "[RPC failure]")
					continue
				}

				fmt.Printf("Removed tunnel for endpoint %q from master %q.\n", ep.Name, master.Name)
			}

			// Deallocate transport subnets for this endpoint.
			alloc, allocErr := loadOrCreateAllocator(configDir, topo)
			if allocErr == nil {
				for _, master := range topo.Masters {
					if containsName(master.Endpoints, ep.Name) {
						alloc.Deallocate(master.Name, ep.Name)
					}
				}
				if saveErr := saveTransportState(alloc, configDir); saveErr != nil {
					fmt.Fprintf(os.Stderr, "warning: save transport state: %v\n", saveErr)
				}
			}

			fmt.Printf("Endpoint removed from all masters. Transport subnets deallocated.\n")
			return nil
		},
	}
}
