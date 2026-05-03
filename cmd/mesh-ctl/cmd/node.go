package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	controlpb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	nodeStatusDeclared       = "declared"
	defaultNodeRemoveTimeout = 10 * time.Second
	maxNodeDrainSeconds      = 1<<31 - 1
)

type nodeListOptions struct {
	topologyPath string
	output       string
	stdout       io.Writer
}

type nodeRemoveOptions struct {
	nodeName     string
	controlPlane string
	drain        time.Duration
	output       string
	timeout      time.Duration
	stdout       io.Writer
}

type nodeListJSONOutput struct {
	Count int             `json:"count"`
	Nodes []nodeListEntry `json:"nodes"`
}

type nodeRemoveJSONOutput struct {
	NodeName               string `json:"node_name"`
	Success                bool   `json:"success"`
	ReassignedOverlayCount int32  `json:"reassigned_overlay_count"`
	Error                  string `json:"error,omitempty"`
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
	cmd.AddCommand(newNodeRemoveCommand())
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

func newNodeRemoveCommand() *cobra.Command {
	options := nodeRemoveOptions{
		output:  topologyOutputHuman,
		timeout: defaultNodeRemoveTimeout,
	}

	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Decommission a node through the control plane",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.nodeName = args[0]
			options.stdout = cmd.OutOrStdout()
			return runNodeRemoveCommand(options)
		},
	}

	cmd.Flags().StringVar(&options.controlPlane, "control-plane", "", "Control-plane gRPC address")
	cmd.Flags().DurationVar(&options.drain, "drain", 0, "Drain window before peer removal")
	cmd.Flags().StringVar(&options.output, "output", topologyOutputHuman, "Output format (human, json)")
	cmd.Flags().DurationVar(&options.timeout, "timeout", defaultNodeRemoveTimeout, "Remove timeout")
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

func runNodeRemoveCommand(options nodeRemoveOptions) error {
	validated, err := validateNodeRemoveOptions(options)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), validated.timeout)
	defer cancel()

	conn, err := grpc.NewClient(validated.controlPlane, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connect control-plane %q: %w", validated.controlPlane, err)
	}
	defer func() { _ = conn.Close() }()

	resp, err := controlpb.NewControlPlaneClient(conn).DecommissionNode(ctx, &controlpb.DecommissionRequest{
		NodeName:     validated.nodeName,
		DrainSeconds: int32(validated.drain / time.Second),
	})
	if err != nil {
		return fmt.Errorf("decommission node %q: %w", validated.nodeName, err)
	}
	result := nodeRemoveJSONOutput{
		NodeName:               validated.nodeName,
		Success:                resp.GetSuccess(),
		ReassignedOverlayCount: resp.GetReassignedOverlayCount(),
		Error:                  resp.GetError(),
	}
	if !resp.GetSuccess() {
		detail := strings.TrimSpace(resp.GetError())
		if detail == "" {
			detail = "control plane rejected decommission"
		}
		return fmt.Errorf("decommission node %q: %s", validated.nodeName, detail)
	}
	return writeNodeRemoveResult(commandOutput(validated.stdout), validated.output, result)
}

func validateNodeRemoveOptions(options nodeRemoveOptions) (nodeRemoveOptions, error) {
	nodeName := strings.TrimSpace(options.nodeName)
	if nodeName == "" {
		return nodeRemoveOptions{}, fmt.Errorf("node name is required")
	}
	controlPlane := strings.TrimSpace(options.controlPlane)
	if controlPlane == "" {
		return nodeRemoveOptions{}, fmt.Errorf("--control-plane is required")
	}
	if options.drain < 0 {
		return nodeRemoveOptions{}, fmt.Errorf("--drain must be >= 0")
	}
	drainSeconds := options.drain / time.Second
	if drainSeconds > maxNodeDrainSeconds {
		return nodeRemoveOptions{}, fmt.Errorf("--drain must be <= %ds", maxNodeDrainSeconds)
	}
	output, err := normalizeTopologyOutput(options.output)
	if err != nil {
		return nodeRemoveOptions{}, err
	}
	timeout := options.timeout
	if timeout <= 0 {
		timeout = defaultNodeRemoveTimeout
	}
	return nodeRemoveOptions{
		nodeName:     nodeName,
		controlPlane: controlPlane,
		drain:        drainSeconds * time.Second,
		output:       output,
		timeout:      timeout,
		stdout:       options.stdout,
	}, nil
}

func writeNodeRemoveResult(out io.Writer, output string, result nodeRemoveJSONOutput) error {
	switch output {
	case topologyOutputHuman:
		_, err := fmt.Fprintf(out, "node %q removed: reassigned_overlay_count=%d\n", result.NodeName, result.ReassignedOverlayCount)
		return err
	case topologyOutputJSON:
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	default:
		return fmt.Errorf("unsupported node remove output %q", output)
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
