package cmd

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	grpcclient "github.com/thebtf/awg-mesh/pkg/grpc"
	pkgtls "github.com/thebtf/awg-mesh/pkg/tls"
	"github.com/thebtf/awg-mesh/pkg/topology"
	proto "github.com/thebtf/awg-mesh/proto"
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
	return &cobra.Command{
		Use:   "prepare [name]",
		Short: "Generate docker-compose and token for an endpoint",
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

			caCert, caKey, err := ensureCA(configDir)
			_ = caCert
			_ = caKey
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
				Token      string
			}{
				Name:       ep.Name,
				Host:       ep.Host,
				OverlayIP:  ep.OverlayIP,
				Image:      "ghcr.io/thebtf/awg-mesh:latest",
				ListenPort: ep.ListenPort,
				Token:      token,
			}
			endpointTemplate, err := loadTemplate("docker-compose.endpoint.yml.tmpl")
			if err != nil {
				return fmt.Errorf("load endpoint compose template: %w", err)
			}
			outputPath := ep.Name + "-docker-compose.yml"
			if err := renderDockerCompose(endpointTemplate, data, outputPath); err != nil {
				return fmt.Errorf("render docker-compose: %w", err)
			}

			fmt.Printf("Endpoint %q prepared.\n\nToken: %s\n\nDocker Compose written to: %s\n\nNext steps:\n  1. Copy %s to the target host\n  2. docker compose -f %s up -d\n  3. mesh-ctl endpoint init %s\n",
				name,
				token,
				outputPath,
				outputPath,
				outputPath,
				name,
			)
			return nil
		},
	}
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
				Target:     ep.GRPCAddr(),
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

			for _, master := range topo.Masters {
				if !containsName(master.Endpoints, ep.Name) {
					continue
				}

				allocation, err := alloc.Allocate(master.Name, ep.Name)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: allocate transport for master %q and endpoint %q failed: %v\n", master.Name, ep.Name, err)
					continue
				}

				masterToken, err := loadToken(nodeDir(configDir, master.Name))
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: cannot load token for master %q: %v\n", master.Name, err)
					continue
				}

				masterClient, err := grpcclient.NewClient(grpcclient.ClientConfig{
					Target:     master.GRPCAddr(),
					Token:      masterToken,
					Insecure:   true,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: cannot connect to master %q: %v\n", master.Name, err)
					continue
				}

				masterCtx, masterCancel := context.WithTimeout(context.Background(), 30*time.Second)
				addResp, addErr := masterClient.Agent().AddTunnel(masterCtx, &proto.AddTunnelRequest{
					Name:          ep.Name,
					EndpointHost:  ep.PeerAddr(),
					OverlayIp:     ep.OverlayIP,
					BalancerIp:    balancerIP,
					PeerPublicKey: resp.NodePublicKey,
					Weight:        1,
					TransportSubnet:     allocation.Subnet.String(),
					MasterTransportIp:   allocation.MasterIP.String(),
					EndpointTransportIp: allocation.EndpointIP.String(),
				})
				masterCancel()
				if closeErr := masterClient.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client for master %q: %v\n", master.Name, closeErr)
				}

				if addErr != nil {
					fmt.Fprintf(os.Stderr, "warning: add tunnel to master %q failed: %v\n", master.Name, addErr)
					continue
				}

				if !addResp.Success {
					fmt.Fprintf(os.Stderr, "warning: add tunnel to master %q failed: %s\n", master.Name, "[RPC failure]")
					continue
				}

				fmt.Printf("Added tunnel for endpoint %q on master %q.\n", ep.Name, master.Name)

				peerCtx, peerCancel := context.WithTimeout(context.Background(), 30*time.Second)
				peerResp, peerErr := selfClient.Agent().AddPeer(peerCtx, &proto.AddPeerRequest{
					PublicKey:           addResp.MasterPublicKey,
					AllowedIps:          []string{allocation.Subnet.String()},
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
				if !peerResp.Success {
					fmt.Fprintf(os.Stderr, "warning: add peer on endpoint for master %q failed: %s\n", master.Name, "[RPC failure]")
					continue
				}

				fmt.Printf("Added peer on endpoint %q for master %q.\n", ep.Name, master.Name)
			}

			if err := saveTransportState(alloc, configDir); err != nil {
				return fmt.Errorf("save transport state: %w", err)
			}

			fmt.Printf("Endpoint %q initialized successfully.\nPublic key: %s\n", name, hex.EncodeToString(resp.NodePublicKey))
			return nil
		},
	}
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
					Target:     master.GRPCAddr(),
					Token:      masterToken,
					Insecure:   true,
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

			fmt.Printf("Endpoint removed from all masters. Manual cleanup may be needed on endpoint host.\n")
			return nil
		},
	}
}
