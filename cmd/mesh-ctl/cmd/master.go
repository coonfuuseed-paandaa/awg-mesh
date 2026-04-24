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

	"github.com/coonfuuseed-paandaa/awg-mesh/cmd/mesh-ctl/internal/adminstate"
	grpcclient "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

func newMasterCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "master",
		Short: "Manage master nodes",
	}

	cmd.AddCommand(newMasterPrepareCommand())
	cmd.AddCommand(newMasterInitCommand())
	cmd.AddCommand(newMasterRemoveCommand())
	cmd.AddCommand(newMasterReloadCommand())

	return cmd
}

const endpointPublicKeyLen = 32

func newMasterPrepareCommand() *cobra.Command {
	var useTraefik bool
	var showToken bool
	var imageFlag string

	cmd := &cobra.Command{
		Use:   "prepare [name]",
		Short: "Generate docker-compose and token for a master",
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

			master := topo.FindMaster(name)
			if master == nil {
				return fmt.Errorf("master %q not found in topology", name)
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
				Name:       master.Name,
				Host:       master.Host,
				OverlayIP:  master.OverlayIP,
				Image:      resolveImage(imageFlag, topo.Defaults.Image.Node, "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:latest", "defaults.image.node"),
				ListenPort: master.ListenPort,
				// Escape $ → $$ to survive Docker Compose variable
				// interpolation. Bcrypt hashes contain literal `$`.
				TokenHash: composeEscapeDollar(hash),
			}

			templateName := "docker-compose.master.yml.tmpl"
			if useTraefik {
				templateName = "docker-compose.master.traefik.yml.tmpl"
			}

			masterTemplate, err := loadTemplate(templateName)
			if err != nil {
				return fmt.Errorf("load master compose template: %w", err)
			}

			// B3 fix: write compose to configDir/nodes/<name>/ (co-located with
			// token and pubkey) instead of CWD.
			outputPath := filepath.Join(nd, master.Name+"-docker-compose.yml")
			if err := renderDockerCompose(masterTemplate, data, outputPath); err != nil {
				return fmt.Errorf("render docker-compose: %w", err)
			}

			tokenPath := filepath.Join(nd, "token")
			printNextSteps("master", name, token, tokenPath, outputPath, useTraefik, showToken)
			return nil
		},
	}

	cmd.Flags().BoolVar(&useTraefik, "traefik", false, "Generate Traefik-compatible compose with labels (no host networking)")
	cmd.Flags().BoolVar(&showToken, "show-token", false, "print raw token to stdout (default: save to disk only)")
	cmd.Flags().StringVar(&imageFlag, "image", "", "Docker image reference (default: topology defaults.image.node, else ghcr.io/coonfuuseed-paandaa/awg-mesh-node:latest)")
	return cmd
}

func newMasterInitCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "init [name]",
		Short: "Initialize master via gRPC",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			master := topo.FindMaster(name)
			if master == nil {
				return fmt.Errorf("master %q not found in topology", name)
			}

			caCert, caKey, err := pkgtls.LoadCA(configDir)
			if err != nil {
				return fmt.Errorf("load CA: %w", err)
			}

			certPEM, keyPEM, err := pkgtls.IssueCert(caCert, caKey, master.Name, []string{master.Host})
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
				Target:   master.GRPCAddr(),
				Token:    token,
				Insecure: true, // pre-Init bootstrap
			})
			if err != nil {
				return fmt.Errorf("create gRPC client: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client: %v\n", closeErr)
				}
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := client.Agent().Init(ctx, &proto.InitRequest{
				CaCert:   caCertPEM,
				NodeCert: certPEM,
				NodeKey:  keyPEM,
				Config: &proto.NodeConfig{
					Name:       master.Name,
					Mode:       "master",
					OverlayIp:  master.OverlayIP,
					ListenPort: int32(master.ListenPort),
				},
			})
			if err != nil {
				return fmt.Errorf("init RPC: %w", err)
			}
			if !resp.Success {
				return fmt.Errorf("init failed: %s", resp.Message)
			}

			// FR-3: use adminstate.SetPubkey for the atomic write.
			// Master init has no UpdateTunnelPeer RPC to issue (Init is the
			// one-time bootstrap), so the callback simply returns the new key.
			// The file is still written atomically and under the per-node lock.
			masterPubKeyHex := hex.EncodeToString(resp.NodePublicKey)
			store := adminstate.NewStore(configDir)
			if _, storeErr := store.SetPubkey(name, func(_ string) (string, error) {
				return masterPubKeyHex, nil
			}); storeErr != nil {
				return fmt.Errorf("write master pubkey: %w", storeErr)
			}

			alloc, err := loadOrCreateAllocator(configDir, topo)
			if err != nil {
				return fmt.Errorf("load transport allocator: %w", err)
			}

			parsedRanges := make([]topology.Range, 0, len(topo.Overlay.Ranges))
			for _, nr := range topo.Overlay.Ranges {
				if r, rErr := topology.ParseRange(nr); rErr == nil {
					parsedRanges = append(parsedRanges, r)
				}
			}

			// cliLogger writes WARN-level messages to stderr for port-assignment fallback.
			cliLogger := zerolog.New(os.Stderr).With().Timestamp().Logger().Level(zerolog.WarnLevel)

			for _, epName := range master.Endpoints {
				ep := topo.FindEndpoint(epName)
				if ep == nil {
					fmt.Fprintf(os.Stderr, "warning: endpoint %q not found in topology for master %q\n", epName, master.Name)
					continue
				}

				var balancerIP string
				if epAddr, parseErr := netip.ParseAddr(ep.OverlayIP); parseErr == nil {
					if bip := topology.BalancerIPForAddr(parsedRanges, epAddr); bip.IsValid() {
						balancerIP = bip.String()
					}
				}

				allocation, err := alloc.Allocate(master.Name, ep.Name)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: allocate transport for master %q and endpoint %q failed: %v\n", master.Name, ep.Name, err)
					continue
				}

				epPubkeyPath := filepath.Join(nodeDir(configDir, ep.Name), "pubkey")
				peerPublicKey, err := readEndpointPublicKey(epPubkeyPath)
				if err != nil {
					if os.IsNotExist(err) {
						// Endpoint is not yet prepared — skip quietly. This is the
						// partial-rollout case and is expected, not an error.
						fmt.Fprintf(os.Stderr, "note: endpoint %q not yet prepared, skipping\n", ep.Name)
					} else {
						fmt.Fprintf(os.Stderr, "warning: endpoint %q pubkey read for master %q: %v\n", ep.Name, master.Name, err)
					}
					continue
				}

				epToken, err := loadToken(nodeDir(configDir, ep.Name))
				if err != nil {
					if os.IsNotExist(err) {
						// Endpoint token not generated yet — the matching
						// 'mesh-ctl endpoint prepare' has not run.
						fmt.Fprintf(os.Stderr, "note: endpoint %q has no local token, skipping peer setup\n", ep.Name)
					} else {
						fmt.Fprintf(os.Stderr, "warning: endpoint %q token read for master %q: %v\n", ep.Name, master.Name, err)
					}
					continue
				}

				peerClient, err := grpcclient.NewClient(grpcclient.ClientConfig{
					Target:   ep.GRPCAddr(),
					Insecure: true,
					Token:    epToken,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: cannot connect to endpoint %q: %v\n", ep.Name, err)
					continue
				}

				allowedIPs, aipErr := topology.BuildAllowedIPsForEndpoint(topo, master.OverlayIP, allocation.Subnet.String())
				if aipErr != nil {
					fmt.Fprintf(os.Stderr, "warning: build allowed_ips for endpoint %q / master %q: %v\n", ep.Name, master.Name, aipErr)
					// Safe fallback: at minimum transport subnet + overlay ranges.
					allowedIPs = []string{allocation.Subnet.String()}
					for _, nr := range topo.Overlay.Ranges {
						if nr.CIDR != "" {
							allowedIPs = append(allowedIPs, nr.CIDR)
						}
					}
				}
				fmt.Printf("master init: AddPeer to endpoint %q with allowed_ips=%v\n", ep.Name, allowedIPs)

				// Step 1: AddPeer to endpoint FIRST so we can read the per-master
				// listen port from the response (FR-1 / T003). The port is then used
				// as EndpointHost in the subsequent AddTunnel call to the master.
				peerCtx, peerCancel := context.WithTimeout(context.Background(), 30*time.Second)
				peerResp, peerErr := peerClient.Agent().AddPeer(peerCtx, &proto.AddPeerRequest{
					PublicKey:           resp.NodePublicKey,
					AllowedIps:          allowedIPs,
					EndpointHost:        master.PeerAddr(),
					PersistentKeepalive: 25,
					TransportSubnet:     allocation.Subnet.String(),
					LocalTransportIp:    allocation.EndpointIP.String(),
					PeerTransportIp:     allocation.MasterIP.String(),
					PeerName:            master.Name,
				})
				peerCancel()
				if closeErr := peerClient.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client for endpoint %q: %v\n", ep.Name, closeErr)
				}
				if peerErr != nil {
					fmt.Fprintf(os.Stderr, "warning: add peer to endpoint %q failed: %v\n", ep.Name, peerErr)
					continue
				}
				if peerResp == nil || !peerResp.Success {
					fmt.Fprintf(os.Stderr, "warning: add peer to endpoint %q failed: %s\n", ep.Name, "[RPC failure]")
					continue
				}

				// Step 2: compute the correct per-master endpoint host using the port
				// returned by AddPeer (or fallback to topology port + offset).
				masterNodes := topo.MastersForEndpoint(ep.Name)
				allMasterNames := make([]string, len(masterNodes))
				for i, mn := range masterNodes {
					allMasterNames[i] = mn.Name
				}
				epHost := ep.PeerHost
				if epHost == "" {
					epHost = ep.Host
				}
				endpointHost := computePeerEndpoint(epHost, ep.ListenPort, peerResp, allMasterNames, master.Name, cliLogger)

				// Step 3: AddTunnel to master with the per-master endpoint host:port.
				// Compute master-side AllowedIPs from topology so the master persists
				// the full list (including /27) even when it starts without --topology.
				masterAllowedIPs, maipErr := topology.BuildAllowedIPsForMasterPeer(topo, ep.Name, ep.OverlayIP, allocation.Subnet.String())
				if maipErr != nil {
					fmt.Fprintf(os.Stderr, "warning: build master allowed_ips for endpoint %q / master %q: %v\n", ep.Name, master.Name, maipErr)
					masterAllowedIPs = []string{allocation.Subnet.String(), ep.OverlayIP + "/32"}
				}
				addTunnelReq := &proto.AddTunnelRequest{
					Name:                ep.Name,
					EndpointHost:        endpointHost,
					OverlayIp:           ep.OverlayIP,
					BalancerIp:          balancerIP,
					PeerPublicKey:       peerPublicKey,
					Weight:              1,
					TransportSubnet:     allocation.Subnet.String(),
					MasterTransportIp:   allocation.MasterIP.String(),
					EndpointTransportIp: allocation.EndpointIP.String(),
					AllowedIps:          masterAllowedIPs,
				}
				addCtx, addCancel := context.WithTimeout(context.Background(), 30*time.Second)
				addResp, addErr := client.Agent().AddTunnel(addCtx, addTunnelReq)
				addCancel()

				// Idempotent re-init: if the tunnel already exists (e.g. from a prior
				// endpoint init), remove it and re-add with the correct endpoint host
				// and per-master WG listen port. Without this, the master keeps the
				// stale endpoint_host from the original AddTunnel and the WG handshake
				// never completes.
				if addErr != nil && strings.Contains(addErr.Error(), "already exists") {
					fmt.Fprintf(os.Stderr, "note: tunnel %q on master %q already exists, re-creating with updated endpoint %s\n", ep.Name, master.Name, endpointHost)
					rmCtx, rmCancel := context.WithTimeout(context.Background(), 15*time.Second)
					_, _ = client.Agent().RemoveTunnel(rmCtx, &proto.RemoveTunnelRequest{Name: ep.Name})
					rmCancel()

					addCtx2, addCancel2 := context.WithTimeout(context.Background(), 30*time.Second)
					addResp, addErr = client.Agent().AddTunnel(addCtx2, addTunnelReq)
					addCancel2()
				}

				if addErr != nil {
					fmt.Fprintf(os.Stderr, "warning: add tunnel to master %q for endpoint %q failed: %v\n", master.Name, ep.Name, addErr)
					continue
				}

				if addResp == nil || !addResp.Success {
					fmt.Fprintf(os.Stderr, "warning: add tunnel to master %q for endpoint %q failed: %s\n", master.Name, ep.Name, "[RPC failure]")
					continue
				}

				fmt.Printf("Added peer on endpoint %q for master %q.\n", ep.Name, master.Name)
				fmt.Printf("Tunnel %q on master %q: created (endpoint: %s).\n", ep.Name, master.Name, endpointHost)
			}

			if err := saveTransportState(alloc, configDir); err != nil {
				return fmt.Errorf("save transport state: %w", err)
			}

			fmt.Printf("Master %q initialized successfully.\nPublic key: %s\n", name, hex.EncodeToString(resp.NodePublicKey))
			return nil
		},
	}
}

func newMasterRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove [name]",
		Short: "Remove all tunnels from a master",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			master := topo.FindMaster(name)
			if master == nil {
				return fmt.Errorf("master %q not found in topology", name)
			}

			_, _, err = pkgtls.LoadCA(configDir)
			if err != nil {
				return fmt.Errorf("load CA: %w", err)
			}

			nd := nodeDir(configDir, name)
			token, err := loadToken(nd)
			if err != nil {
				return fmt.Errorf("load token: %w", err)
			}

			client, err := grpcclient.NewClient(grpcclient.ClientConfig{
				Target:   master.GRPCAddr(),
				Token:    token,
				Insecure: true, // pre-Init bootstrap
			})
			if err != nil {
				return fmt.Errorf("create gRPC client: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client: %v\n", closeErr)
				}
			}()

			for _, epName := range master.Endpoints {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				removeResp, err := client.Agent().RemoveTunnel(ctx, &proto.RemoveTunnelRequest{
					Name: epName,
				})
				cancel()

				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: remove tunnel for endpoint %q from master %q failed: %v\n", epName, master.Name, err)
					continue
				}

				if !removeResp.Success {
					fmt.Fprintf(os.Stderr, "warning: remove tunnel for endpoint %q from master %q failed: %s\n", epName, master.Name, "[RPC failure]")
					continue
				}

				fmt.Printf("Removed tunnel for endpoint %q from master %q.\n", epName, master.Name)
			}

			fmt.Printf("Master tunnels removed. Manual cleanup may be needed on host.\n")
			return nil
		},
	}
}

// newMasterReloadCommand implements T012+T013 (local tracker #92).
//
// Walk every endpoint bound to master <name>, read admin-state pubkey, and
// force-push via UpdateTunnelPeer RPC. Recovery primitive for admin-master
// state divergence (e.g. after master restart with corrupted transport.yml).
//
// The command is strictly read-only from the admin-state perspective: it never
// modifies ~/.mesh-ctl/nodes/<ep>/pubkey or topology.yml. The UAPI update on
// the master side is the only write that occurs.
func newMasterReloadCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "reload <name>",
		Short: "Force-push all endpoint keys to a master (recovery primitive)",
		Long: `Walk every endpoint bound to master <name>, read admin-state pubkey,
force-push via UpdateTunnelPeer RPC. Recovery primitive for admin-master
state divergence (e.g. after master restart with a corrupted transport.yml).

Read-only from admin-state: never modifies ~/.mesh-ctl/ files.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			master := topo.FindMaster(name)
			if master == nil {
				return fmt.Errorf("master %q not in topology — run 'mesh-ctl master reload <name>' with a valid master name", name)
			}

			nd := nodeDir(configDir, name)
			token, err := loadToken(nd)
			if err != nil {
				return fmt.Errorf("load token for master %q: %w", name, err)
			}

			client, err := grpcclient.NewClient(grpcclient.ClientConfig{
				Target:   master.GRPCAddr(),
				Token:    token,
				Insecure: true,
			})
			if err != nil {
				return fmt.Errorf("create gRPC client for master %q: %w", name, err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client: %v\n", closeErr)
				}
			}()

			// Pre-compute balancer IP lookup table from overlay ranges once.
			parsedRanges := make([]topology.Range, 0, len(topo.Overlay.Ranges))
			for _, nr := range topo.Overlay.Ranges {
				if r, rErr := topology.ParseRange(nr); rErr == nil {
					parsedRanges = append(parsedRanges, r)
				}
			}
			alloc, err := loadOrCreateAllocator(configDir, topo)
			if err != nil {
				return fmt.Errorf("load transport allocator for allowed_ips refresh: %w", err)
			}

			endpointsTotal := 0
			endpointsOk := 0

			for _, epName := range master.Endpoints {
				ep := topo.FindEndpoint(epName)
				if ep == nil {
					fmt.Fprintf(os.Stderr, "warning: endpoint %q referenced by master %q not found in topology — skipping\n", epName, name)
					endpointsTotal++
					continue
				}

				endpointsTotal++

				// Read admin-state pubkey (raw 32-byte WireGuard public key written
				// by 'mesh-ctl endpoint init' via resp.NodePublicKey from Init RPC).
				pubkeyPath := filepath.Join(nodeDir(configDir, ep.Name), "pubkey")
				pubkeyBytes, err := readEndpointPublicKey(pubkeyPath)
				if err != nil {
					fmt.Printf("Endpoint %s: FAILED: read pubkey: %v\n", ep.Name, err)
					continue
				}

				// Compute balancer IP from the endpoint overlay IP and overlay ranges.
				var balancerIP string
				if epAddr, parseErr := netip.ParseAddr(ep.OverlayIP); parseErr == nil {
					if bip := topology.BalancerIPForAddr(parsedRanges, epAddr); bip.IsValid() {
						balancerIP = bip.String()
					}
				}

				var allowedIPs []string
				if allocation, aErr := alloc.Allocate(master.Name, ep.Name); aErr == nil {
					if aips, aipErr := topology.BuildAllowedIPsForMasterPeer(topo, ep.Name, ep.OverlayIP, allocation.Subnet.String()); aipErr == nil {
						allowedIPs = aips
					} else {
						fmt.Fprintf(os.Stderr, "warning: master %s endpoint %s: build master-side allowed_ips: %v\n", name, ep.Name, aipErr)
					}
				} else {
					fmt.Fprintf(os.Stderr, "warning: master %s endpoint %s: allocate transport for allowed_ips: %v\n", name, ep.Name, aErr)
				}

				reloadCtx, reloadCancel := context.WithTimeout(context.Background(), 30*time.Second)
				updateResp, updateErr := client.Agent().UpdateTunnelPeer(reloadCtx, &proto.UpdateTunnelPeerRequest{
					Name:          ep.Name,
					PeerPublicKey: pubkeyBytes,
					BalancerIp:    balancerIP,
					AllowedIps:    allowedIPs,
				})
				reloadCancel()

				if updateErr != nil {
					statusLine, isPreV110 := updateTunnelPeerFailureStatus(updateErr)
					if isPreV110 {
						fmt.Fprintf(os.Stderr, "master %s running pre-v1.10.0 — upgrade master before using 'master reload'\n", name)
					}
					fmt.Printf("Endpoint %s: %s\n", ep.Name, statusLine)
					continue
				}

				if updateResp == nil || !updateResp.Success {
					fmt.Printf("Endpoint %s: FAILED: update tunnel peer RPC returned unsuccessful response\n", ep.Name)
					continue
				}

				endpointsOk++
				if updateResp.Unchanged {
					fmt.Printf("Endpoint %s: already up to date\n", ep.Name)
				} else {
					fmt.Printf("Endpoint %s: updated (new key applied)\n", ep.Name)
				}
			}

			if endpointsOk < endpointsTotal {
				return fmt.Errorf("master reload %q: %d of %d endpoint(s) failed — see above for details",
					name, endpointsTotal-endpointsOk, endpointsTotal)
			}

			return nil
		},
	}
}

// readEndpointPublicKey reads a 32-byte WireGuard public key from path.
// Supports two storage formats:
//   - 32 raw bytes  (legacy — written by pre-v1.11.2 init commands)
//   - 64 hex chars + optional newline (current — written by adminstate.SetPubkey)
func readEndpointPublicKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file %q: %w", path, err)
	}
	// Legacy: raw 32-byte binary.
	if len(data) == endpointPublicKeyLen {
		return data, nil
	}
	// Current: 64 hex chars (+ optional newline).
	trimmed := strings.TrimSpace(string(data))
	if len(trimmed) == 64 {
		b, hexErr := hex.DecodeString(trimmed)
		if hexErr != nil {
			return nil, fmt.Errorf("decode hex pubkey from %q: %w", path, hexErr)
		}
		return b, nil
	}
	return nil, fmt.Errorf("pubkey at %q has unexpected length %d (want %d bytes or 64 hex chars)", path, len(data), endpointPublicKeyLen)
}
