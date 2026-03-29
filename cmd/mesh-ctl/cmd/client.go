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

	"github.com/spf13/cobra"
	grpcclient "github.com/thebtf/awg-mesh/pkg/grpc"
	"github.com/thebtf/awg-mesh/pkg/mikrotik"
	pkgtls "github.com/thebtf/awg-mesh/pkg/tls"
	"github.com/thebtf/awg-mesh/pkg/topology"
	proto "github.com/thebtf/awg-mesh/proto"
)

func newClientCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Manage client nodes",
	}

	cmd.AddCommand(newClientPrepareCommand())
	cmd.AddCommand(newClientInitCommand())
	cmd.AddCommand(newClientRemoveCommand())

	return cmd
}

func newClientPrepareCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "prepare [name]",
		Short: "Generate config for a client (linux or mikrotik)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			client := topo.FindClient(name)
			if client == nil {
				return fmt.Errorf("client %q not found in topology", name)
			}

			switch client.Type {
			case "linux":
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
					Masters    string
				}{
					Name:       client.Name,
					Host:       "",
					OverlayIP:  client.OverlayIP,
					Image:      "ghcr.io/thebtf/awg-mesh:latest",
					ListenPort: 51820,
					Token:      token,
					Masters:    strings.Join(client.Masters, ","),
				}

				outputPath := client.Name + "-docker-compose.yml"
				clientTemplate, err := loadTemplate("docker-compose.client.yml.tmpl")
				if err != nil {
					return fmt.Errorf("load client compose template: %w", err)
				}
				if err := renderDockerCompose(clientTemplate, data, outputPath); err != nil {
					return fmt.Errorf("render docker-compose: %w", err)
				}

				fmt.Printf("Client %q prepared.\n\nToken: %s\n\nDocker Compose written to: %s\n\nNext steps:\n  1. Copy %s to the target host\n  2. docker compose -f %s up -d\n  3. mesh-ctl client init %s\n",
					name,
					token,
					outputPath,
					outputPath,
					outputPath,
					name,
				)

			case "mikrotik":
				nd := nodeDir(configDir, name)
				if err := os.MkdirAll(nd, 0755); err != nil {
					return fmt.Errorf("create node directory: %w", err)
				}

				token, err := pkgtls.GenerateToken()
				if err != nil {
					return fmt.Errorf("generate token: %w", err)
				}

				hash, err := pkgtls.HashToken(token)
				if err != nil {
					return fmt.Errorf("hash token: %w", err)
				}

				if err := pkgtls.SaveTokenHash(nd, hash); err != nil {
					return fmt.Errorf("save token hash: %w", err)
				}

				if err := saveToken(nd, token); err != nil {
					return fmt.Errorf("save token: %w", err)
				}

				// Collect master hosts for the deploy script.
				masterHosts := make([]string, 0, len(client.Masters))
				for _, masterName := range client.Masters {
					m := topo.FindMaster(masterName)
					if m == nil {
						return fmt.Errorf("master %q referenced by client %q not found in topology", masterName, name)
					}
					masterHosts = append(masterHosts, m.Host)
				}

				ds := mikrotik.DeployScript{
					ContainerName: name,
					Image:         "ghcr.io/thebtf/awg-mesh:latest",
					Veth:          "veth-" + name,
					VethGateway:   "192.168.100.1/24",
					OverlayIP:     client.OverlayIP,
					OverlayNet:    topo.Overlay.Space,
					ListenPort:    51820,
					Masters:       masterHosts,
					AWGConfig:     strings.Join(masterHosts, ","),
					Token:         token,
				}

				rsc, err := mikrotik.GenerateDeployRSC(ds)
				if err != nil {
					return fmt.Errorf("generate RouterOS script: %w", err)
				}

				rscPath := name + "-mikrotik.rsc"
				if err := os.WriteFile(rscPath, []byte(rsc), 0644); err != nil {
					return fmt.Errorf("write RouterOS script: %w", err)
				}

				fmt.Printf("MikroTik client %q prepared.\n\nToken: %s\n\nRouterOS script written to: %s\n\nNext steps:\n  1. Copy %s to the MikroTik router\n  2. /import file-name=%s\n  3. mesh-ctl client init %s\n",
					name,
					token,
					rscPath,
					rscPath,
					rscPath,
					name,
				)

			default:
				return fmt.Errorf("unknown client type %q (must be linux or mikrotik)", client.Type)
			}

			return nil
		},
	}
}

func resolveClientTarget(topo *topology.Topology, client *topology.ClientNode) (string, error) {
	if len(client.Masters) == 0 {
		return "", fmt.Errorf("client %q has no masters", client.Name)
	}

	master := topo.FindMaster(client.Masters[0])
	if master == nil {
		return "", fmt.Errorf("master %q not found in topology", client.Masters[0])
	}

	return master.Host, nil
}

func resolveClientGRPCAddr(_ *topology.Topology, client *topology.ClientNode) string {
	return client.GRPCAddr()
}

func newClientInitCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "init [name]",
		Short: "Initialize client via gRPC",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			client := topo.FindClient(name)
			if client == nil {
				return fmt.Errorf("client %q not found in topology", name)
			}

			targetHost, err := resolveClientTarget(topo, client)
			if err != nil {
				return err
			}

			caCert, caKey, err := pkgtls.LoadCA(configDir)
			if err != nil {
				return fmt.Errorf("load CA: %w", err)
			}

			certPEM, keyPEM, err := pkgtls.IssueCert(caCert, caKey, client.Name, []string{targetHost})
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

			clientGRPC, err := grpcclient.NewClient(grpcclient.ClientConfig{
				Target:     resolveClientGRPCAddr(topo, client),
				Token:    token,
				Insecure: true,
			})
			if err != nil {
				return fmt.Errorf("create gRPC client: %w", err)
			}
			defer func() {
				if closeErr := clientGRPC.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client: %v\n", closeErr)
				}
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := clientGRPC.Agent().Init(ctx, &proto.InitRequest{
				CaCert:   caCertPEM,
				NodeCert: certPEM,
				NodeKey:  keyPEM,
				Config: &proto.NodeConfig{
					Name:       client.Name,
					Mode:       "client",
					OverlayIp:  client.OverlayIP,
					ListenPort: 0,
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
				return fmt.Errorf("write client pubkey file %q: %w", pubkeyPath, err)
			}

			alloc, err := loadOrCreateAllocator(configDir, topo)
			if err != nil {
				return fmt.Errorf("load transport allocator: %w", err)
			}

			mastersConnected := 0
			parsedRanges := make([]topology.Range, 0, len(topo.Overlay.Ranges))
			for _, nr := range topo.Overlay.Ranges {
				if r, rErr := topology.ParseRange(nr); rErr == nil {
					parsedRanges = append(parsedRanges, r)
				} else {
					fmt.Fprintf(os.Stderr, "warning: failed to parse topology range %q: %v\n", nr, rErr)
				}
			}

			for _, masterName := range client.Masters {
				master := topo.FindMaster(masterName)
				if master == nil {
					fmt.Fprintf(os.Stderr, "warning: master %q not found in topology, skipping\n", masterName)
					continue
				}

				var masterBalancerIP string
				if masterAddr, parseErr := netip.ParseAddr(master.OverlayIP); parseErr == nil {
					if bip := topology.BalancerIPForAddr(parsedRanges, masterAddr); bip.IsValid() {
						masterBalancerIP = bip.String()
					}
				} else {
					fmt.Fprintf(os.Stderr, "warning: failed to parse master overlay IP %q for master %q: %v\n", master.OverlayIP, master.Name, parseErr)
				}

				allocation, err := alloc.Allocate(master.Name, client.Name)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: allocate transport for master %q and client %q failed: %v\n", master.Name, name, err)
					continue
				}

				masterPubkeyPath := filepath.Join(nodeDir(configDir, master.Name), "pubkey")
				masterPubkey, err := os.ReadFile(masterPubkeyPath)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: read master %q pubkey: %v\n", master.Name, err)
					continue
				}
				if len(masterPubkey) == 0 {
					fmt.Fprintf(os.Stderr, "warning: master %q pubkey is empty\n", master.Name)
					continue
				}

				masterToken, err := loadToken(nodeDir(configDir, master.Name))
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: load token for master %q: %v\n", master.Name, err)
					continue
				}

				masterClient, err := grpcclient.NewClient(grpcclient.ClientConfig{
					Target:     master.GRPCAddr(),
					Token:      masterToken,
					Insecure:   true,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: connect to master %q: %v\n", master.Name, err)
					continue
				}

				masterCtx, masterCancel := context.WithTimeout(context.Background(), 30*time.Second)
				addResp, addErr := masterClient.Agent().AddTunnel(masterCtx, &proto.AddTunnelRequest{
					Name:                client.Name,
					EndpointHost:        "",
					OverlayIp:           client.OverlayIP,
					BalancerIp:          "",
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

				if addErr != nil {
					fmt.Fprintf(os.Stderr, "warning: add tunnel on master %q failed: %v\n", master.Name, addErr)
					continue
				}
				if addResp == nil || !addResp.Success {
					fmt.Fprintf(os.Stderr, "warning: add tunnel on master %q failed: [RPC failure]\n", master.Name)
					continue
				}

				clientPeerClient, err := grpcclient.NewClient(grpcclient.ClientConfig{
					Target:     resolveClientGRPCAddr(topo, client),
					Insecure: true,
					Token:      token,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: connect to client %q for add peer: %v\n", name, err)
					continue
				}

				// Use the actual listen port from the master's per-tunnel WG interface
				// (not master.ListenPort which is the config default, not the ephemeral port).
				peerHost := master.PeerHost
				if peerHost == "" {
					peerHost = master.Host
				}
				tunnelEndpoint := fmt.Sprintf("%s:%d", peerHost, addResp.ListenPort)

				peerCtx, peerCancel := context.WithTimeout(context.Background(), 30*time.Second)
				peerResp, peerErr := clientPeerClient.Agent().AddPeer(peerCtx, &proto.AddPeerRequest{
					PublicKey:           masterPubkey,
					AllowedIps:          []string{"0.0.0.0/0"},
					EndpointHost:        master.Name + "|" + tunnelEndpoint,
					PersistentKeepalive: 25,
					TransportSubnet:     allocation.Subnet.String(),
					LocalTransportIp:    allocation.EndpointIP.String(),
					PeerTransportIp:     allocation.MasterIP.String(),
					BalancerIp:          masterBalancerIP,
				})
				peerCancel()
				if closeErr := clientPeerClient.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client for client %q: %v\n", name, closeErr)
				}

				if peerErr != nil {
					fmt.Fprintf(os.Stderr, "warning: add peer on client %q for master %q failed: %v\n", name, master.Name, peerErr)
					continue
				}
				if peerResp == nil || !peerResp.Success {
					fmt.Fprintf(os.Stderr, "warning: add peer on client %q for master %q failed: [RPC failure]\n", name, master.Name)
					continue
				}

				fmt.Printf("Added peer on client %q for master %q.\n", name, master.Name)
				mastersConnected++
			}

			if mastersConnected == 0 {
				return fmt.Errorf("client %q: no masters connected — initialization incomplete (check master availability)", name)
			}

			if err := saveTransportState(alloc, configDir); err != nil {
				return fmt.Errorf("save transport state: %w", err)
			}

			fmt.Printf("Client %q initialized: %d/%d masters connected.\nPublic key: %s\n",
				name, mastersConnected, len(client.Masters), hex.EncodeToString(resp.NodePublicKey))
			return nil
		},
	}
}

func newClientRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove [name]",
		Short: "Remove a client from the mesh",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			client := topo.FindClient(name)
			if client == nil {
				return fmt.Errorf("client %q not found in topology", name)
			}

			for _, masterName := range client.Masters {
				master := topo.FindMaster(masterName)
				if master == nil {
					fmt.Fprintf(os.Stderr, "warning: master %q not found in topology, skipping\n", masterName)
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
					Name: client.Name,
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

				fmt.Printf("Removed tunnel for client %q from master %q.\n", client.Name, master.Name)
			}

			fmt.Printf("Client removed from all masters. Manual cleanup may be needed on client host.\n")
			return nil
		},
	}
}
