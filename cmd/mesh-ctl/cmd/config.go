package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show mesh-ctl configuration",
	}

	cmd.AddCommand(newConfigShowCommand())

	return cmd
}

func newConfigShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Display current mesh-ctl configuration paths and state",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("Config directory:  %s\n", configDir)
			fmt.Printf("Topology file:    %s\n", topologyPath)

			// Check CA
			caFile := filepath.Join(configDir, "ca.crt")
			if _, err := os.Stat(caFile); err == nil {
				fmt.Printf("CA certificate:   %s (exists)\n", caFile)
			} else {
				fmt.Printf("CA certificate:   not initialized (run 'mesh-ctl <role> prepare' first)\n")
			}

			caKeyFile := filepath.Join(configDir, "ca.key")
			if _, err := os.Stat(caKeyFile); err == nil {
				fmt.Printf("CA private key:   %s (exists)\n", caKeyFile)
			} else {
				fmt.Printf("CA private key:   not initialized\n")
			}

			// Check nodes
			nodesDir := filepath.Join(configDir, "nodes")
			entries, err := os.ReadDir(nodesDir)
			if err != nil {
				fmt.Printf("Nodes:            none (no nodes directory)\n")
			} else {
				fmt.Printf("Nodes:            %d configured\n", len(entries))
				for _, e := range entries {
					if !e.IsDir() {
						continue
					}
					name := e.Name()
					tokenFile := filepath.Join(nodesDir, name, "token")
					pubkeyFile := filepath.Join(nodesDir, name, "pubkey")
					hasToken := fileExists(tokenFile)
					hasPubkey := fileExists(pubkeyFile)

					status := "prepared"
					if hasPubkey {
						status = "initialized"
					}
					if !hasToken {
						status = "incomplete"
					}

					fmt.Printf("  %-20s %s\n", name, status)
				}
			}

			// Check topology
			if _, err := os.Stat(topologyPath); err == nil {
				fmt.Printf("Topology:         %s (exists)\n", topologyPath)
			} else {
				fmt.Printf("Topology:         %s (not found)\n", topologyPath)
			}

			return nil
		},
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
