package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	grpcclient "github.com/thebtf/awg-mesh/pkg/grpc"
	"github.com/thebtf/awg-mesh/pkg/topology"
	"gopkg.in/yaml.v3"
)

type localTransportState struct {
	Allocations []struct {
		Tunnel     string `yaml:"tunnel"`
		MasterIP   string `yaml:"master_ip"`
		EndpointIP string `yaml:"endpoint_ip"`
	} `yaml:"allocations"`
}

func newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Query status of all mesh nodes via gRPC",
		RunE: func(cmd *cobra.Command, args []string) error {
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology: %w", err)
			}

			type nodeInfo struct {
				name string
				host string
				mode string
			}

			var nodes []nodeInfo
			for _, m := range topo.Masters {
				nodes = append(nodes, nodeInfo{name: m.Name, host: m.Host, mode: "master"})
			}
			for _, e := range topo.Endpoints {
				nodes = append(nodes, nodeInfo{name: e.Name, host: e.Host, mode: "endpoint"})
			}

			transportStatePath := filepath.Join(configDir, "transport.yml")
			transportByTunnel := make(map[string]string)
			if transportData, err := os.ReadFile(transportStatePath); err == nil {
				var transportState localTransportState
				if yaml.Unmarshal(transportData, &transportState) == nil {
					for _, allocation := range transportState.Allocations {
						transportByTunnel[allocation.Tunnel] = allocation.MasterIP + "->" + allocation.EndpointIP
					}
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
					Target:     n.host + ":9090",
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

				fmt.Printf("%-20s %-10s %-20s %-10s %-15s %-25s %s\n",
					resp.Name, resp.Mode, n.host, "ONLINE", resp.OverlayIp, transportDisplay, fmt.Sprintf("%d", len(resp.Tunnels)))
			}

			return nil
		},
	}
}
