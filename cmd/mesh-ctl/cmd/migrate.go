package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type migrateOptions struct {
	fromPath string
	toPath   string
	output   string
	force    bool
	stdout   io.Writer
}

func newMigrateCommand() *cobra.Command {
	options := migrateOptions{output: topologyOutputHuman}

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Convert a v1.x topology file to schema v2",
		RunE: func(cmd *cobra.Command, _ []string) error {
			options.stdout = cmd.OutOrStdout()
			return runMigrateCommand(options)
		},
	}

	cmd.Flags().StringVar(&options.fromPath, "from", "", "Path to legacy v1.x topology YAML")
	cmd.Flags().StringVar(&options.toPath, "to", "", "Path to write schema v2 topology YAML")
	cmd.Flags().StringVar(&options.output, "output", topologyOutputHuman, "Output format (human, json)")
	cmd.Flags().BoolVar(&options.force, "force", false, "Overwrite --to path when it already exists")
	return cmd
}

func runMigrateCommand(options migrateOptions) error {
	output, err := normalizeTopologyOutput(options.output)
	if err != nil {
		return err
	}
	fromPath := strings.TrimSpace(options.fromPath)
	if fromPath == "" {
		return fmt.Errorf("--from is required")
	}
	data, err := os.ReadFile(fromPath)
	if err != nil {
		return fmt.Errorf("read legacy topology %q: %w", fromPath, err)
	}
	result, err := topology.MigrateV1ToV2WithReport(data)
	if err != nil {
		return fmt.Errorf("migrate %q: %w", fromPath, err)
	}

	out := commandOutput(options.stdout)
	if output == topologyOutputJSON {
		if strings.TrimSpace(options.toPath) != "" {
			return fmt.Errorf("--output json writes converted topology to stdout; omit --to")
		}
		return writeMigratedTopologyJSON(out, result.Topology)
	}

	toPath := strings.TrimSpace(options.toPath)
	if toPath == "" {
		return fmt.Errorf("--to is required unless --output json is used")
	}
	if _, err := os.Stat(toPath); err == nil && !options.force {
		return fmt.Errorf("refusing to overwrite %q without --force", toPath)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat output topology %q: %w", toPath, err)
	}
	if err := topology.SaveTopologyV2(toPath, result.Topology); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(out, "migration written: %s\nschema_version=2 nodes=%d warnings=%d\n",
		toPath, len(result.Topology.Nodes), len(result.Warnings)); err != nil {
		return err
	}
	for _, warning := range result.Warnings {
		if _, err := fmt.Fprintf(out, "warning: %s\n", warning); err != nil {
			return err
		}
	}
	return nil
}

func writeMigratedTopologyJSON(out io.Writer, topo *topology.TopologyV2) error {
	data, err := yaml.Marshal(topo)
	if err != nil {
		return fmt.Errorf("marshal converted topology: %w", err)
	}
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("normalize converted topology: %w", err)
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(normalizeYAMLForJSON(raw))
}

func normalizeYAMLForJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, val := range typed {
			out[key] = normalizeYAMLForJSON(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, val := range typed {
			out[fmt.Sprint(key)] = normalizeYAMLForJSON(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, normalizeYAMLForJSON(item))
		}
		return out
	default:
		return typed
	}
}
