package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	topologyOutputHuman = "human"
	topologyOutputJSON  = "json"
	defaultTopologyJob  = "awg-mesh"
)

type topologyValidateOptions struct {
	topologyPath string
	output       string
	stdout       io.Writer
}

type topologyPrometheusOptions struct {
	topologyPath string
	jobName      string
	stdout       io.Writer
}

type topologyValidationSummary struct {
	Status        string `json:"status"`
	SchemaVersion int    `json:"schema_version"`
	Mesh          string `json:"mesh"`
	Nodes         int    `json:"nodes"`
	Services      int    `json:"services"`
}

type prometheusConfig struct {
	ScrapeConfigs []prometheusScrapeConfig `yaml:"scrape_configs"`
}

type prometheusScrapeConfig struct {
	JobName       string                   `yaml:"job_name"`
	StaticConfigs []prometheusStaticConfig `yaml:"static_configs,omitempty"`
}

type prometheusStaticConfig struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels,omitempty"`
}

func newTopologyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "topology",
		Short: "Manage v2 topology files",
	}

	cmd.AddCommand(newTopologyValidateCommand())
	cmd.AddCommand(newTopologyGeneratePrometheusConfigCommand())
	return cmd
}

func newTopologyValidateCommand() *cobra.Command {
	options := topologyValidateOptions{output: topologyOutputHuman}

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate a schema v2 topology file",
		RunE: func(cmd *cobra.Command, _ []string) error {
			options.topologyPath = topologyPath
			options.stdout = cmd.OutOrStdout()
			return runTopologyValidateCommand(options)
		},
	}

	cmd.Flags().StringVar(&options.output, "output", topologyOutputHuman, "Output format (human, json)")
	return cmd
}

func newTopologyGeneratePrometheusConfigCommand() *cobra.Command {
	options := topologyPrometheusOptions{jobName: defaultTopologyJob}

	cmd := &cobra.Command{
		Use:   "generate-prometheus-config",
		Short: "Generate Prometheus scrape_configs for v2 topology nodes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			options.topologyPath = topologyPath
			options.stdout = cmd.OutOrStdout()
			return runTopologyGeneratePrometheusConfig(options)
		},
	}

	cmd.Flags().StringVar(&options.jobName, "job-name", defaultTopologyJob, "Prometheus scrape job name")
	return cmd
}

func runTopologyValidateCommand(options topologyValidateOptions) error {
	output, err := normalizeTopologyOutput(options.output)
	if err != nil {
		return err
	}
	topo, err := loadTopologyV2(options.topologyPath)
	if err != nil {
		return err
	}

	summary := topologyValidationSummary{
		Status:        "valid",
		SchemaVersion: int(topo.SchemaVersion),
		Mesh:          topo.Mesh.Name,
		Nodes:         len(topo.Nodes),
		Services:      len(topo.Services),
	}

	out := commandOutput(options.stdout)
	switch output {
	case topologyOutputHuman:
		_, err = fmt.Fprintf(out, "topology %q valid: schema_version=%d mesh=%q nodes=%d services=%d\n",
			options.topologyPath, summary.SchemaVersion, summary.Mesh, summary.Nodes, summary.Services)
		return err
	case topologyOutputJSON:
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(summary)
	default:
		return fmt.Errorf("unsupported topology output %q", output)
	}
}

func runTopologyGeneratePrometheusConfig(options topologyPrometheusOptions) error {
	topo, err := loadTopologyV2(options.topologyPath)
	if err != nil {
		return err
	}
	cfg, err := buildPrometheusConfig(topo, options.jobName)
	if err != nil {
		return err
	}

	enc := yaml.NewEncoder(commandOutput(options.stdout))
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		_ = enc.Close()
		return fmt.Errorf("encode prometheus config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("close prometheus config encoder: %w", err)
	}
	return nil
}

func loadTopologyV2(path string) (*topology.TopologyV2, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("expected schema v2 topology: topology path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read topology %q: %w", path, err)
	}
	version, err := topology.DetectSchemaVersion(data)
	if err != nil {
		return nil, fmt.Errorf("expected schema v2 topology in %q: %w", path, err)
	}
	if version == topology.SchemaV1 {
		return nil, fmt.Errorf("expected schema v2 topology in %q: %w", path, topology.ErrSchemaV1Deprecated)
	}
	if version != topology.SchemaV2 {
		return nil, fmt.Errorf("expected schema v2 topology in %q: got schema version %d", path, version)
	}

	var topo topology.TopologyV2
	if err := yaml.Unmarshal(data, &topo); err != nil {
		return nil, fmt.Errorf("parse schema v2 topology %q: %w", path, err)
	}
	if err := topology.ValidateV2(&topo); err != nil {
		return nil, fmt.Errorf("validate schema v2 topology %q: %w", path, err)
	}
	return &topo, nil
}

func buildPrometheusConfig(topo *topology.TopologyV2, jobName string) (prometheusConfig, error) {
	if topo == nil {
		return prometheusConfig{}, fmt.Errorf("topology is required")
	}
	port, enabled, err := prometheusPort(topo.Observability.PrometheusListen)
	if err != nil {
		return prometheusConfig{}, err
	}
	if !enabled {
		return prometheusConfig{ScrapeConfigs: []prometheusScrapeConfig{}}, nil
	}

	trimmedJobName := strings.TrimSpace(jobName)
	if trimmedJobName == "" {
		trimmedJobName = defaultTopologyJob
	}

	nodes := append([]topology.NodeV2(nil), topo.Nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Name < nodes[j].Name
	})

	staticConfigs := make([]prometheusStaticConfig, 0, len(nodes))
	for _, node := range nodes {
		labels := map[string]string{
			"node":  node.Name,
			"roles": roleListLabel(node.Roles),
		}
		if strings.TrimSpace(node.Region) != "" {
			labels["region"] = strings.TrimSpace(node.Region)
		}
		staticConfigs = append(staticConfigs, prometheusStaticConfig{
			Targets: []string{net.JoinHostPort(node.OverlayIP, port)},
			Labels:  labels,
		})
	}

	return prometheusConfig{
		ScrapeConfigs: []prometheusScrapeConfig{{
			JobName:       trimmedJobName,
			StaticConfigs: staticConfigs,
		}},
	}, nil
}

func prometheusPort(listen string) (string, bool, error) {
	trimmed := strings.TrimSpace(listen)
	if trimmed == "" {
		return "", false, nil
	}

	if _, port, err := net.SplitHostPort(trimmed); err == nil {
		if err := validatePort(port); err != nil {
			return "", false, err
		}
		return port, true, nil
	}

	if err := validatePort(trimmed); err == nil {
		return trimmed, true, nil
	}

	return "", false, fmt.Errorf("observability.prometheus_listen %q must be host:port, :port, or port", listen)
}

func validatePort(port string) error {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("observability.prometheus_listen port %q is invalid", port)
	}
	return nil
}

func roleListLabel(roles []role.Role) string {
	parts := make([]string, 0, len(roles))
	for _, r := range roles {
		parts = append(parts, string(r))
	}
	return strings.Join(parts, ",")
}

func normalizeTopologyOutput(output string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(output)) {
	case "", topologyOutputHuman:
		return topologyOutputHuman, nil
	case topologyOutputJSON:
		return topologyOutputJSON, nil
	default:
		return "", fmt.Errorf("unsupported --output %q (supported: human, json)", output)
	}
}

func commandOutput(out io.Writer) io.Writer {
	if out != nil {
		return out
	}
	return os.Stdout
}
