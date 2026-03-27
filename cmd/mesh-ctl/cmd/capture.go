package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	grpcclient "github.com/thebtf/awg-mesh/pkg/grpc"
	"github.com/thebtf/awg-mesh/pkg/topology"
	proto "github.com/thebtf/awg-mesh/proto"
	"gopkg.in/yaml.v3"
)

func newCaptureCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "capture",
		Short: "Manage TLS/QUIC packet capture for AWG param generation",
	}

	cmd.AddCommand(newCaptureRefreshCommand())
	cmd.AddCommand(newCaptureDomainsCommand())
	cmd.AddCommand(newCaptureScheduleCommand())

	return cmd
}

func newCaptureRefreshCommand() *cobra.Command {
	var masters []string
	var countPerDomain int

	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Trigger packet capture on master nodes",
		RunE: func(cmd *cobra.Command, args []string) error {
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology: %w", err)
			}

			targets := topo.Masters
			if len(masters) > 0 {
				targets = nil
				for _, name := range masters {
					m := topo.FindMaster(name)
					if m == nil {
						return fmt.Errorf("master %q not found", name)
					}
					targets = append(targets, *m)
				}
			}

			for _, m := range targets {
				nd := nodeDir(configDir, m.Name)
				token, err := loadToken(nd)
				if err != nil {
					fmt.Fprintf(os.Stderr, "skip %s: no token: %v\n", m.Name, err)
					continue
				}

				client, err := grpcclient.NewClient(grpcclient.ClientConfig{
					Target:     m.Host + ":9090",
					CACertPath: caPath(configDir),
					Token:      token,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "skip %s: connect error: %v\n", m.Name, err)
					continue
				}

				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				resp, rpcErr := client.Agent().CaptureRefresh(ctx, &proto.CaptureRequest{
					CountPerDomain: int32(countPerDomain),
				})
				cancel()

				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close %s: %v\n", m.Name, closeErr)
				}

				if rpcErr != nil {
					fmt.Fprintf(os.Stderr, "%s: capture failed: %v\n", m.Name, rpcErr)
					continue
				}

				fmt.Printf("%s: captured %d packets\n", m.Name, resp.CapturedCount)
			}

			return nil
		},
	}

	cmd.Flags().StringSliceVar(&masters, "master", nil, "Specific masters to capture on (default: all)")
	cmd.Flags().IntVar(&countPerDomain, "count", 5, "Packets per domain")

	return cmd
}

func newCaptureDomainsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "domains",
		Short: "Manage capture domain list",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List configured capture domains",
		RunE: func(cmd *cobra.Command, args []string) error {
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology: %w", err)
			}

			domainsFile := topo.Capture.DomainsFile
			if domainsFile == "" {
				fmt.Println("No domains file configured in topology")
				return nil
			}

			data, err := os.ReadFile(domainsFile)
			if err != nil {
				return fmt.Errorf("read domains file %q: %w", domainsFile, err)
			}

			domains := strings.Split(strings.TrimSpace(string(data)), "\n")
			for _, d := range domains {
				d = strings.TrimSpace(d)
				if d != "" && !strings.HasPrefix(d, "#") {
					fmt.Println(d)
				}
			}

			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "add [domain...]",
		Short: "Add domains to capture list",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology: %w", err)
			}

			domainsFile := topo.Capture.DomainsFile
			if domainsFile == "" {
				return fmt.Errorf("no domains file configured in topology")
			}

			f, err := os.OpenFile(domainsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				return fmt.Errorf("open domains file: %w", err)
			}
			defer f.Close()

			for _, domain := range args {
				if _, err := fmt.Fprintln(f, domain); err != nil {
					return fmt.Errorf("write domain %q: %w", domain, err)
				}
			}

			fmt.Printf("Added %d domains to %s\n", len(args), domainsFile)
			return nil
		},
	})

	return cmd
}

func newCaptureScheduleCommand() *cobra.Command {
	var (
		interval string
		show     bool
	)

	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Show or update capture schedule",
		RunE: func(cmd *cobra.Command, args []string) error {
			trimmedInterval := strings.TrimSpace(interval)
			if show && trimmedInterval != "" {
				return fmt.Errorf("specify only one of --interval or --show")
			}

			if show {
				topo, err := topology.LoadTopology(topologyPath)
				if err != nil {
					return fmt.Errorf("load topology: %w", err)
				}

				schedule := strings.TrimSpace(topo.Capture.Schedule)
				if schedule == "" {
					fmt.Println("(not set)")
				} else {
					fmt.Println(schedule)
				}
				return nil
			}

			if trimmedInterval == "" {
				return fmt.Errorf("specify --interval or --show")
			}

			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology: %w", err)
			}

			topo.Capture.Schedule = trimmedInterval

			data, err := yaml.Marshal(topo)
			if err != nil {
				return fmt.Errorf("marshal topology yaml: %w", err)
			}

			if err := os.WriteFile(topologyPath, data, 0644); err != nil {
				return fmt.Errorf("write topology file: %w", err)
			}

			fmt.Printf("Capture schedule set to %s\n", trimmedInterval)
			return nil
		},
	}

	cmd.Flags().StringVar(&interval, "interval", "", "Capture interval duration (for example 24h)")
	cmd.Flags().BoolVar(&show, "show", false, "Show current capture schedule")

	return cmd
}
