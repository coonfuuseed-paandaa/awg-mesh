package cmd

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	grpcclient "github.com/thebtf/awg-mesh/pkg/grpc"
	pkgtls "github.com/thebtf/awg-mesh/pkg/tls"
	"github.com/thebtf/awg-mesh/pkg/topology"
	proto "github.com/thebtf/awg-mesh/proto"
)

func newMasterCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "master",
		Short: "Manage master nodes",
	}

	cmd.AddCommand(newMasterPrepareCommand())
	cmd.AddCommand(newMasterInitCommand())
	cmd.AddCommand(newMasterRemoveCommand())

	return cmd
}

func newMasterPrepareCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "prepare [name]",
		Short: "Generate docker-compose and token for a master",
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
				Name:       master.Name,
				Host:       master.Host,
				OverlayIP:  master.OverlayIP,
				Image:      "ghcr.io/thebtf/awg-mesh-node:latest",
				ListenPort: master.ListenPort,
				Token:      token,
			}

			masterTemplate, err := loadTemplate("docker-compose.master.yml.tmpl")
			if err != nil {
				return fmt.Errorf("load master compose template: %w", err)
			}

			outputPath := master.Name + "-docker-compose.yml"
			if err := renderDockerCompose(masterTemplate, data, outputPath); err != nil {
				return fmt.Errorf("render docker-compose: %w", err)
			}

			fmt.Printf("Master %q prepared.\n\nToken: %s\n\nDocker Compose written to: %s\n\nNext steps:\n  1. Copy %s to the target host\n  2. docker compose -f %s up -d\n  3. mesh-ctl master init %s\n",
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
				Target:     master.Host + ":9090",
				CACertPath: caPath(configDir),
				Token:      token,
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

			pubkeyPath := filepath.Join(nd, "pubkey")
			if err := os.WriteFile(pubkeyPath, resp.NodePublicKey, 0644); err != nil {
				return fmt.Errorf("write pubkey file %q: %w", pubkeyPath, err)
			}

			for _, epName := range master.Endpoints {
				ep := topo.FindEndpoint(epName)
				if ep == nil {
					fmt.Fprintf(os.Stderr, "warning: endpoint %q not found in topology for master %q\n", epName, master.Name)
					continue
				}

				epPubkeyPath := filepath.Join(nodeDir(configDir, ep.Name), "pubkey")
				peerPublicKey, err := os.ReadFile(epPubkeyPath)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: endpoint %q pubkey missing for master %q: %v\n", ep.Name, master.Name, err)
					continue
				}

				addCtx, addCancel := context.WithTimeout(context.Background(), 30*time.Second)
				addResp, addErr := client.Agent().AddTunnel(addCtx, &proto.AddTunnelRequest{
					Name:          ep.Name,
					EndpointHost:  ep.Host + ":" + strconv.Itoa(ep.ListenPort),
					OverlayIp:     ep.OverlayIP,
					BalancerIp:    "",
					PeerPublicKey: peerPublicKey,
					Weight:        1,
				})
				addCancel()
				if addErr != nil {
					fmt.Fprintf(os.Stderr, "warning: add tunnel to endpoint %q failed: %v\n", ep.Name, addErr)
					continue
				}

				if !addResp.Success {
					fmt.Fprintf(os.Stderr, "warning: add tunnel to endpoint %q failed: %s\n", ep.Name, "[RPC failure]")
					continue
				}

				fmt.Printf("Added tunnel for endpoint %q on master %q.\n", ep.Name, master.Name)
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
				Target:     master.Host + ":9090",
				CACertPath: caPath(configDir),
				Token:      token,
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
