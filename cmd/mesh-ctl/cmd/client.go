package cmd

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	grpcclient "github.com/thebtf/awg-mesh/pkg/grpc"
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
				}{
					Name:       client.Name,
					Host:       "",
					OverlayIP:  client.OverlayIP,
					Image:      "ghcr.io/thebtf/awg-mesh-node:latest",
					ListenPort: 51820,
					Token:      token,
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
				rscPath := name + "-mikrotik.rsc"
				content := "# RouterOS config stub for " + name + " — to be implemented in Phase 5\n"
				if err := os.WriteFile(rscPath, []byte(content), 0644); err != nil {
					return fmt.Errorf("write mikrotik stub: %w", err)
				}
				fmt.Printf("MikroTik client %q stub written to: %s\n", name, rscPath)
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
				Target:     targetHost + ":9090",
				CACertPath: caPath(configDir),
				Token:      token,
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

			fmt.Printf("Client %q initialized successfully.\nPublic key: %s\n", name, hex.EncodeToString(resp.NodePublicKey))
			return nil
		},
	}
}
