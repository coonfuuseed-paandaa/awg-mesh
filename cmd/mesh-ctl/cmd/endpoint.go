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
				Image:      resolveImage(imageFlag, topo.Defaults.Image.Node, "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:latest"),
				ListenPort: ep.ListenPort,
				// Escape $ → $$ to survive Docker Compose variable
				// interpolation. Bcrypt hashes contain literal `$`.
				TokenHash: composeEscapeDollar(hash),
			}

			templateName := "docker-compose.endpoint.yml.tmpl"
			if useTraefik {
				templateName = "docker-compose.endpoint.traefik.yml.tmpl"
			}

			endpointTemplate, err := loadTemplate(templateName)
			if err != nil {
				return fmt.Errorf("load endpoint compose template: %w", err)
			}
			outputPath := ep.Name + "-docker-compose.yml"
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

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := client.Agent().Init(ctx, &proto.InitRequest{
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

			pubkeyPath := filepath.Join(nd, "pubkey")
			if err := os.WriteFile(pubkeyPath, resp.NodePublicKey, 0644); err != nil {
				return fmt.Errorf("write pubkey file %q: %w", pubkeyPath, err)
			}

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

			// T010: track per-master outcome counters for exit-code determination.
			mastersTotal := 0
			mastersOk := 0
			var failedMasters []string

			for _, master := range topo.Masters {
				if !containsName(master.Endpoints, ep.Name) {
					continue
				}

				mastersTotal++

				allocation, err := alloc.Allocate(master.Name, ep.Name)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: allocate transport for master %q and endpoint %q failed: %v\n", master.Name, ep.Name, err)
					failedMasters = append(failedMasters, master.Name)
					continue
				}

				masterToken, err := loadToken(nodeDir(configDir, master.Name))
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: cannot load token for master %q: %v\n", master.Name, err)
					failedMasters = append(failedMasters, master.Name)
					continue
				}

				masterClient, err := grpcclient.NewClient(grpcclient.ClientConfig{
					Target:   master.GRPCAddr(),
					Token:    masterToken,
					Insecure: true,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: cannot connect to master %q: %v\n", master.Name, err)
					failedMasters = append(failedMasters, master.Name)
					continue
				}

				masterCtx, masterCancel := context.WithTimeout(context.Background(), 30*time.Second)
				addResp, addErr := masterClient.Agent().AddTunnel(masterCtx, &proto.AddTunnelRequest{
					Name:                ep.Name,
					EndpointHost:        ep.PeerAddr(),
					OverlayIp:           ep.OverlayIP,
					BalancerIp:          balancerIP,
					PeerPublicKey:       resp.NodePublicKey,
					Weight:              1,
					TransportSubnet:     allocation.Subnet.String(),
					MasterTransportIp:   allocation.MasterIP.String(),
					EndpointTransportIp: allocation.EndpointIP.String(),
				})
				masterCancel()
				if closeErr := masterClient.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client for master %q: %v\n", master.Name, closeErr)
				}

				statusLine := ""
				needAddPeer := false
				allowedIPs := []string{allocation.Subnet.String()}
				for _, nr := range topo.Overlay.Ranges {
					if nr.CIDR != "" {
						allowedIPs = append(allowedIPs, nr.CIDR)
					}
				}

				if addErr != nil {
					errLower := strings.ToLower(addErr.Error())
					isAlreadyExists := status.Code(addErr) == codes.AlreadyExists || strings.Contains(errLower, "already exists")
					if !isAlreadyExists {
						statusLine = fmt.Sprintf("FAILED: %v", addErr)
						fmt.Printf("Tunnel %q on master %q: %s\n", ep.Name, master.Name, statusLine)
						failedMasters = append(failedMasters, master.Name)
						continue
					}

					masterClient, err = grpcclient.NewClient(grpcclient.ClientConfig{
						Target:   master.GRPCAddr(),
						Token:    masterToken,
						Insecure: true,
					})
					if err != nil {
						statusLine = fmt.Sprintf("FAILED: cannot connect to master %q for tunnel update: %v", master.Name, err)
						fmt.Printf("Tunnel %q on master %q: %s\n", ep.Name, master.Name, statusLine)
						failedMasters = append(failedMasters, master.Name)
						continue
					}

					masterCtx, masterCancel = context.WithTimeout(context.Background(), 30*time.Second)
					updateResp, updateErr := masterClient.Agent().UpdateTunnelPeer(masterCtx, &proto.UpdateTunnelPeerRequest{
						Name:          ep.Name,
						PeerPublicKey: resp.NodePublicKey,
						BalancerIp:    balancerIP,
						AllowedIps:    allowedIPs,
					})
					masterCancel()
					if closeErr := masterClient.Close(); closeErr != nil {
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

					needAddPeer = false
					if updateResp.Unchanged {
						statusLine = "unchanged (key matches)"
					} else {
						newPubKeyHex := hex.EncodeToString(resp.NodePublicKey)
						if len(newPubKeyHex) > 8 {
							newPubKeyHex = newPubKeyHex[:8]
						}
						statusLine = fmt.Sprintf("updated (new key: %s)", newPubKeyHex)
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

				if !needAddPeer {
					continue
				}

				// Get master's public key — from response or from disk.
				var masterPubKey []byte
				if addResp != nil {
					masterPubKey = addResp.MasterPublicKey
				}
				if len(masterPubKey) == 0 {
					pubkeyPath := filepath.Join(nodeDir(configDir, master.Name), "pubkey")
					masterPubKey, _ = os.ReadFile(pubkeyPath)
				}
				if len(masterPubKey) == 0 {
					fmt.Fprintf(os.Stderr, "warning: master %q public key not available, skipping peer setup\n", master.Name)
					continue
				}

				peerCtx, peerCancel := context.WithTimeout(context.Background(), 30*time.Second)
				peerResp, peerErr := selfClient.Agent().AddPeer(peerCtx, &proto.AddPeerRequest{
					PublicKey:           masterPubKey,
					AllowedIps:          allowedIPs,
					EndpointHost:        master.PeerAddr(),
					PersistentKeepalive: 25,
					TransportSubnet:     allocation.Subnet.String(),
					LocalTransportIp:    allocation.EndpointIP.String(),
					PeerTransportIp:     allocation.MasterIP.String(),
				})
				peerCancel()
				if peerErr != nil {
					fmt.Fprintf(os.Stderr, "warning: add peer on endpoint for master %q failed: %v\n", master.Name, peerErr)
					continue
				}
				if peerResp == nil || !peerResp.Success {
					fmt.Fprintf(os.Stderr, "warning: add peer on endpoint for master %q failed: %s\n", master.Name, "[RPC failure]")
					continue
				}
			}

			if err := saveTransportState(alloc, configDir); err != nil {
				return fmt.Errorf("save transport state: %w", err)
			}

			// T010: if any master failed, emit a failure summary to stderr and
			// return a non-zero exit code so operator CI scripts can detect the
			// partial failure.
			if mastersOk < mastersTotal {
				fmt.Fprintf(os.Stderr, "\nFailed to update %d of %d master(s):\n", mastersTotal-mastersOk, mastersTotal)
				for _, m := range failedMasters {
					fmt.Fprintf(os.Stderr, "  - %s\n", m)
					fmt.Fprintf(os.Stderr, "    To recover: mesh-ctl master reload %s\n", m)
				}
				return fmt.Errorf("endpoint init: %d master(s) failed — see above for details", mastersTotal-mastersOk)
			}

			fmt.Printf("Endpoint %q initialized successfully.\nPublic key: %s\n", name, hex.EncodeToString(resp.NodePublicKey))
			return nil
		},
	}
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
