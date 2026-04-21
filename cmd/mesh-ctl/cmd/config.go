package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
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
			// B30 fix: show the resolved absolute topology path so the operator
			// always sees where the file will actually be read from, not the
			// bare filename that was passed on the command line.
			absTopology, err := filepath.Abs(topologyPath)
			if err != nil {
				absTopology = topologyPath // fallback: show as-is
			}
			fmt.Printf("Topology file:    %s\n", absTopology)

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
				// B1 fix: count only real node directories; skip .bak.* backup
				// dirs created by the upgrade driver (e.g. "master-01.bak.20260418").
				// Without this filter, backup dirs appear as phantom nodes with
				// "incomplete" status and inflate the node count.
				nodeCount := 0
				for _, e := range entries {
					if e.IsDir() && !isBackupDir(e.Name()) {
						nodeCount++
					}
				}
				fmt.Printf("Nodes:            %d configured\n", nodeCount)
				for _, e := range entries {
					if !e.IsDir() {
						continue
					}
					name := e.Name()
					// B1 fix: skip .bak.* dirs in the per-node listing.
					if isBackupDir(name) {
						continue
					}
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

			// Check topology (use the already-resolved absolute path).
			if _, err := os.Stat(absTopology); err == nil {
				fmt.Printf("Topology:         %s (exists)\n", absTopology)
			} else {
				fmt.Printf("Topology:         %s (not found)\n", absTopology)
			}

			transportFile := filepath.Join(configDir, "transport.yml")
			if _, err := os.Stat(transportFile); err == nil {
				if data, err := os.ReadFile(transportFile); err == nil {
					var ts struct {
						Pool         string `yaml:"pool"`
						PrefixLength int    `yaml:"prefix_length"`
						Allocations  []struct {
							Tunnel     string `yaml:"tunnel"`
							Subnet     string `yaml:"subnet"`
							MasterIP   string `yaml:"master_ip"`
							EndpointIP string `yaml:"endpoint_ip"`
						} `yaml:"allocations"`
					}
					if yaml.Unmarshal(data, &ts) == nil {
						fmt.Printf("Transport pool:   %s/%d\n", ts.Pool, ts.PrefixLength)
						fmt.Printf("Transport allocs: %d\n", len(ts.Allocations))
						for _, a := range ts.Allocations {
							fmt.Printf("  %-20s %s  master:%s  endpoint:%s\n", a.Tunnel, a.Subnet, a.MasterIP, a.EndpointIP)
						}
					}
				}
			} else {
				fmt.Printf("Transport:        not initialized\n")
			}

			return nil
		},
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// isBackupDir reports whether name looks like an upgrade-driver backup directory
// (e.g. "master-01.bak.20260418T182300Z"). These are created by phasePrepare in
// pkg/upgrade/driver.go and must not be surfaced as real node entries.
func isBackupDir(name string) bool {
	return strings.Contains(name, ".bak.")
}
