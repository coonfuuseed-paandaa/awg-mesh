package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/spf13/cobra"
)

const nodeStatusDeclared = "declared"

type nodeListOptions struct {
	topologyPath string
	output       string
	stdout       io.Writer
}

type nodeListJSONOutput struct {
	Count int             `json:"count"`
	Nodes []nodeListEntry `json:"nodes"`
}

type nodeListEntry struct {
	Name      string   `json:"name"`
	OverlayIP string   `json:"overlay_ip"`
	Roles     []string `json:"roles"`
	Platform  string   `json:"platform"`
	Region    string   `json:"region,omitempty"`
	Status    string   `json:"status"`
}

func newNodeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Manage v2 mesh nodes",
	}

	cmd.AddCommand(newNodeListCommand())
	return cmd
}

func newNodeListCommand() *cobra.Command {
	options := nodeListOptions{output: topologyOutputHuman}

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List nodes declared in a schema v2 topology",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options.topologyPath = topologyPath
			options.stdout = cmd.OutOrStdout()
			return runNodeListCommand(options)
		},
	}

	cmd.Flags().StringVar(&options.output, "output", topologyOutputHuman, "Output format (human, json)")
	return cmd
}

func runNodeListCommand(options nodeListOptions) error {
	output, err := normalizeTopologyOutput(options.output)
	if err != nil {
		return err
	}
	topo, err := loadTopologyV2(options.topologyPath)
	if err != nil {
		return err
	}
	entries := buildNodeListEntries(topo)

	out := commandOutput(options.stdout)
	switch output {
	case topologyOutputHuman:
		return writeNodeListHuman(out, entries)
	case topologyOutputJSON:
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(nodeListJSONOutput{Count: len(entries), Nodes: entries})
	default:
		return fmt.Errorf("unsupported node output %q", output)
	}
}

func buildNodeListEntries(topo *topology.TopologyV2) []nodeListEntry {
	nodes := append([]topology.NodeV2(nil), topo.Nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Name < nodes[j].Name
	})

	entries := make([]nodeListEntry, 0, len(nodes))
	for _, node := range nodes {
		entries = append(entries, nodeListEntry{
			Name:      node.Name,
			OverlayIP: node.OverlayIP,
			Roles:     roleStrings(node.Roles),
			Platform:  nodePlatform(node),
			Region:    node.Region,
			Status:    nodeStatusDeclared,
		})
	}
	return entries
}

func writeNodeListHuman(out io.Writer, entries []nodeListEntry) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NAME\tOVERLAY_IP\tROLES\tPLATFORM\tSTATUS"); err != nil {
		return err
	}
	for _, entry := range entries {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			entry.Name, entry.OverlayIP, roleListLabelFromStrings(entry.Roles), entry.Platform, entry.Status); err != nil {
			return err
		}
	}
	return w.Flush()
}

func nodePlatform(node topology.NodeV2) string {
	if platform := strings.TrimSpace(node.Platform); platform != "" {
		return platform
	}
	return "-"
}

func roleStrings(roles []role.Role) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, string(r))
	}
	return out
}

func roleListLabelFromStrings(roles []string) string {
	if len(roles) == 0 {
		return "-"
	}
	return strings.Join(roles, ",")
}
