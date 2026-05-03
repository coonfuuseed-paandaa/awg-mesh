package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	pkgupgrade "github.com/coonfuuseed-paandaa/awg-mesh/pkg/upgrade"
	"github.com/spf13/cobra"
)

const (
	upgradeStateFileName = "upgrade-state.json"

	upgradeStatusRunning = "running"
	upgradeStatusPaused  = "paused"
	upgradeStatusBlocked = "blocked"
)

type upgradeOptions struct {
	version      string
	topologyPath string
	configDir    string
	order        string
	dryRun       bool
	ssh          bool
	stdout       io.Writer
}

type upgradeStateOptions struct {
	configDir string
	stdout    io.Writer
}

type meshUpgradeState struct {
	Version       string                 `json:"version"`
	Status        string                 `json:"status"`
	Paused        bool                   `json:"paused"`
	Message       string                 `json:"message,omitempty"`
	Plan          []meshUpgradePlanEntry `json:"plan"`
	UpdatedAtUnix int64                  `json:"updated_at_unix"`
}

type meshUpgradePlanEntry struct {
	Phase         int      `json:"phase"`
	PhaseName     string   `json:"phase_name"`
	NodeName      string   `json:"node_name"`
	Roles         []string `json:"roles"`
	TargetVersion string   `json:"target_version"`
	Parallel      bool     `json:"parallel"`
	Status        string   `json:"status"`
}

func newUpgradeCommand() *cobra.Command {
	options := upgradeOptions{}
	var pause bool
	var resume bool

	cmd := &cobra.Command{
		Use:   "upgrade [version]",
		Short: "Plan a v2 rolling upgrade",
		Args: func(cmd *cobra.Command, args []string) error {
			if pause || resume {
				if len(args) != 0 {
					return fmt.Errorf("upgrade --pause/--resume does not accept a version argument")
				}
				return nil
			}
			if len(args) != 1 {
				return fmt.Errorf("upgrade requires exactly one version argument")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			stateOptions := upgradeStateOptions{
				configDir: configDir,
				stdout:    cmd.OutOrStdout(),
			}
			switch {
			case pause && resume:
				return fmt.Errorf("--pause and --resume are mutually exclusive")
			case pause:
				return runUpgradePauseCommand(stateOptions)
			case resume:
				return runUpgradeResumeCommand(stateOptions)
			}

			options.version = args[0]
			options.topologyPath = topologyPath
			options.configDir = configDir
			options.stdout = cmd.OutOrStdout()
			return runUpgradeCommand(options)
		},
	}

	cmd.Flags().BoolVar(&options.dryRun, "dry-run", false, "Print the v2 rolling-upgrade plan without executing")
	cmd.Flags().StringVar(&options.order, "order", "", "Comma-separated manual node upgrade order")
	cmd.Flags().BoolVar(&options.ssh, "ssh", false, "Use SSH deploy when v2 execution support is available")
	cmd.Flags().BoolVar(&pause, "pause", false, "Pause the persisted upgrade state")
	cmd.Flags().BoolVar(&resume, "resume", false, "Resume the persisted upgrade state")

	cmd.AddCommand(newUpgradeStatusCommand())
	cmd.AddCommand(newUpgradePauseCommand())
	cmd.AddCommand(newUpgradeResumeCommand())
	return cmd
}

func newUpgradeStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show persisted upgrade state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgradeStatusCommand(upgradeStateOptions{
				configDir: configDir,
				stdout:    cmd.OutOrStdout(),
			})
		},
	}
}

func newUpgradePauseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "pause",
		Short: "Persist a pause marker for the active upgrade",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgradePauseCommand(upgradeStateOptions{
				configDir: configDir,
				stdout:    cmd.OutOrStdout(),
			})
		},
	}
}

func newUpgradeResumeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Clear a persisted upgrade pause marker",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgradeResumeCommand(upgradeStateOptions{
				configDir: configDir,
				stdout:    cmd.OutOrStdout(),
			})
		},
	}
}

func runUpgradeCommand(options upgradeOptions) error {
	version := strings.TrimSpace(options.version)
	if version == "" {
		return fmt.Errorf("version is required")
	}
	topo, err := loadTopologyV2(options.topologyPath)
	if err != nil {
		return err
	}
	manualOrder, err := parseUpgradeOrder(options.order)
	if err != nil {
		return err
	}
	plan, err := buildMeshUpgradePlan(topo, version, manualOrder)
	if err != nil {
		return err
	}

	out := commandOutput(options.stdout)
	if options.dryRun {
		return writeUpgradePlan(out, version, plan, true)
	}

	state := meshUpgradeState{
		Version: version,
		Status:  upgradeStatusBlocked,
		Message: "v2 upgrade execution is not supported until v2 deploy metadata and executor are available",
		Plan:    plan,
	}
	if err := saveUpgradeState(options.configDir, state); err != nil {
		return err
	}
	return fmt.Errorf("%s; use --dry-run to inspect the phased plan", state.Message)
}

func runUpgradeStatusCommand(options upgradeStateOptions) error {
	out := commandOutput(options.stdout)
	state, err := loadUpgradeState(options.configDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, writeErr := fmt.Fprintln(out, "No upgrade state"); writeErr != nil {
			return writeErr
		}
		logPath, logErr := pkgupgrade.MostRecentLogPath(options.configDir)
		if logErr != nil {
			return logErr
		}
		if logPath != "" {
			_, writeErr := fmt.Fprintf(out, "Latest upgrade log: %s\n", logPath)
			return writeErr
		}
		return nil
	}

	if _, err := fmt.Fprintf(out, "upgrade version=%s status=%s paused=%t updated_at_unix=%d\n",
		state.Version, state.Status, state.Paused, state.UpdatedAtUnix); err != nil {
		return err
	}
	if strings.TrimSpace(state.Message) != "" {
		if _, err := fmt.Fprintf(out, "message: %s\n", state.Message); err != nil {
			return err
		}
	}
	return writeUpgradePlan(out, state.Version, state.Plan, false)
}

func runUpgradePauseCommand(options upgradeStateOptions) error {
	state, err := loadUpgradeState(options.configDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no upgrade state to pause")
		}
		return err
	}
	if state.Status == upgradeStatusBlocked {
		return fmt.Errorf("cannot pause blocked upgrade: %s", state.Message)
	}
	state.Paused = true
	state.Status = upgradeStatusPaused
	if err := saveUpgradeState(options.configDir, state); err != nil {
		return err
	}
	_, err = fmt.Fprintf(commandOutput(options.stdout), "upgrade %s paused\n", state.Version)
	return err
}

func runUpgradeResumeCommand(options upgradeStateOptions) error {
	state, err := loadUpgradeState(options.configDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no upgrade state to resume")
		}
		return err
	}
	if state.Status == upgradeStatusBlocked {
		return fmt.Errorf("cannot resume blocked upgrade: %s", state.Message)
	}
	state.Paused = false
	state.Status = upgradeStatusRunning
	if err := saveUpgradeState(options.configDir, state); err != nil {
		return err
	}
	_, err = fmt.Fprintf(commandOutput(options.stdout), "upgrade %s resumed\n", state.Version)
	return err
}

func buildMeshUpgradePlan(topo *topology.TopologyV2, version string, manualOrder []string) ([]meshUpgradePlanEntry, error) {
	if topo == nil {
		return nil, fmt.Errorf("topology is required")
	}
	nodeByName := make(map[string]topology.NodeV2, len(topo.Nodes))
	for _, node := range topo.Nodes {
		nodeByName[node.Name] = node
	}
	if len(manualOrder) > 0 {
		entries := make([]meshUpgradePlanEntry, 0, len(manualOrder))
		seen := make(map[string]struct{}, len(manualOrder))
		for i, name := range manualOrder {
			node, ok := nodeByName[name]
			if !ok {
				return nil, fmt.Errorf("node %q in --order is not present in topology", name)
			}
			if _, exists := seen[name]; exists {
				return nil, fmt.Errorf("node %q appears more than once in --order", name)
			}
			seen[name] = struct{}{}
			entries = append(entries, makeUpgradePlanEntry(i+1, "manual", node, version, false))
		}
		return entries, nil
	}

	nodes := append([]topology.NodeV2(nil), topo.Nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Name < nodes[j].Name
	})

	var masters []topology.NodeV2
	var meshRoles []topology.NodeV2
	var clients []topology.NodeV2
	for _, node := range nodes {
		switch {
		case hasUpgradeRole(node, role.RoleMaster):
			masters = append(masters, node)
		case hasUpgradeRole(node, role.RoleClient):
			clients = append(clients, node)
		default:
			meshRoles = append(meshRoles, node)
		}
	}

	entries := make([]meshUpgradePlanEntry, 0, len(nodes))
	for _, node := range masters {
		entries = append(entries, makeUpgradePlanEntry(1, "masters", node, version, false))
	}
	for _, node := range meshRoles {
		entries = append(entries, makeUpgradePlanEntry(2, "mesh-roles", node, version, true))
	}
	for _, node := range clients {
		entries = append(entries, makeUpgradePlanEntry(3, "clients", node, version, true))
	}
	return entries, nil
}

func makeUpgradePlanEntry(phase int, phaseName string, node topology.NodeV2, version string, parallel bool) meshUpgradePlanEntry {
	return meshUpgradePlanEntry{
		Phase:         phase,
		PhaseName:     phaseName,
		NodeName:      node.Name,
		Roles:         roleStrings(node.Roles),
		TargetVersion: version,
		Parallel:      parallel,
		Status:        "planned",
	}
}

func hasUpgradeRole(node topology.NodeV2, want role.Role) bool {
	for _, candidate := range node.Roles {
		if candidate == want {
			return true
		}
	}
	return false
}

func writeUpgradePlan(out io.Writer, version string, plan []meshUpgradePlanEntry, dryRun bool) error {
	if dryRun {
		if _, err := fmt.Fprintf(out, "Dry run: no changes will be made for upgrade %s\n", version); err != nil {
			return err
		}
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "PHASE\tPHASE_NAME\tPARALLEL\tNODE\tROLES\tTARGET\tSTATUS"); err != nil {
		return err
	}
	for _, entry := range plan {
		if _, err := fmt.Fprintf(w, "%d\t%s\t%t\t%s\t%s\t%s\t%s\n",
			entry.Phase,
			entry.PhaseName,
			entry.Parallel,
			entry.NodeName,
			strings.Join(entry.Roles, ","),
			entry.TargetVersion,
			entry.Status,
		); err != nil {
			return err
		}
	}
	return w.Flush()
}

func parseUpgradeOrder(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	parts := strings.Split(trimmed, ",")
	order := make([]string, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, fmt.Errorf("--order contains an empty node name")
		}
		order = append(order, name)
	}
	return order, nil
}

func upgradeStatePath(configDir string) string {
	return filepath.Join(configDir, pkgupgrade.BackupsDirName, upgradeStateFileName)
}

func saveUpgradeState(configDir string, state meshUpgradeState) error {
	state.UpdatedAtUnix = time.Now().Unix()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode upgrade state: %w", err)
	}
	path := upgradeStatePath(configDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create upgrade state directory: %w", err)
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write upgrade state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace upgrade state: %w", err)
	}
	return nil
}

func loadUpgradeState(configDir string) (meshUpgradeState, error) {
	data, err := os.ReadFile(upgradeStatePath(configDir))
	if err != nil {
		return meshUpgradeState{}, err
	}
	var state meshUpgradeState
	if err := json.Unmarshal(data, &state); err != nil {
		return meshUpgradeState{}, fmt.Errorf("decode upgrade state: %w", err)
	}
	return state, nil
}
