package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/topology"
	"gopkg.in/yaml.v3"
)

const (
	v2TopologyFixture = "../../../pkg/topology/testdata/v2-topology.yml"
	v1TopologyFixture = "../../../pkg/topology/testdata/v1x-topology.yml"
)

func TestLoadTopologyV2RejectsLegacyV1(t *testing.T) {
	_, err := loadTopologyV2(v1TopologyFixture)
	if err == nil {
		t.Fatal("expected legacy v1 topology to fail")
	}
	if !errors.Is(err, topology.ErrSchemaV1Deprecated) {
		t.Fatalf("error = %v, want ErrSchemaV1Deprecated", err)
	}
	if !strings.Contains(err.Error(), "schema v2") {
		t.Fatalf("error should name schema v2, got: %v", err)
	}
}

func TestRunTopologyValidateCommandOutputsHumanAndJSON(t *testing.T) {
	t.Run("human", func(t *testing.T) {
		var out bytes.Buffer
		err := runTopologyValidateCommand(topologyValidateOptions{
			topologyPath: v2TopologyFixture,
			output:       "human",
			stdout:       &out,
		})
		if err != nil {
			t.Fatalf("runTopologyValidateCommand: %v", err)
		}
		text := out.String()
		if !strings.Contains(text, "valid") || !strings.Contains(text, "nodes=5") {
			t.Fatalf("unexpected human output: %q", text)
		}
	})

	t.Run("json", func(t *testing.T) {
		var out bytes.Buffer
		err := runTopologyValidateCommand(topologyValidateOptions{
			topologyPath: v2TopologyFixture,
			output:       "json",
			stdout:       &out,
		})
		if err != nil {
			t.Fatalf("runTopologyValidateCommand: %v", err)
		}
		var got struct {
			Status        string `json:"status"`
			SchemaVersion int    `json:"schema_version"`
			Nodes         int    `json:"nodes"`
			Services      int    `json:"services"`
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("decode JSON output: %v\n%s", err, out.String())
		}
		if got.Status != "valid" || got.SchemaVersion != 2 || got.Nodes != 5 || got.Services != 1 {
			t.Fatalf("unexpected JSON output: %+v", got)
		}
	})
}

func TestRunTopologyValidateCommandRejectsLegacyV1(t *testing.T) {
	var out bytes.Buffer
	err := runTopologyValidateCommand(topologyValidateOptions{
		topologyPath: v1TopologyFixture,
		output:       "json",
		stdout:       &out,
	})
	if err == nil {
		t.Fatal("expected legacy v1 topology to fail")
	}
	if !errors.Is(err, topology.ErrSchemaV1Deprecated) {
		t.Fatalf("error = %v, want ErrSchemaV1Deprecated", err)
	}
}

func TestRunTopologyGeneratePrometheusConfig(t *testing.T) {
	var out bytes.Buffer
	err := runTopologyGeneratePrometheusConfig(topologyPrometheusOptions{
		topologyPath: v2TopologyFixture,
		jobName:      "awg-mesh",
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runTopologyGeneratePrometheusConfig: %v", err)
	}

	got := decodePrometheusConfig(t, out.Bytes())
	if len(got.ScrapeConfigs) != 1 {
		t.Fatalf("scrape_configs count = %d, want 1\n%s", len(got.ScrapeConfigs), out.String())
	}
	cfg := got.ScrapeConfigs[0]
	if cfg.JobName != "awg-mesh" {
		t.Fatalf("job_name = %q, want awg-mesh", cfg.JobName)
	}
	if len(cfg.StaticConfigs) != 5 {
		t.Fatalf("static_configs count = %d, want 5\n%s", len(cfg.StaticConfigs), out.String())
	}

	master := findStaticConfigByNode(cfg.StaticConfigs, "master-01")
	if master == nil {
		t.Fatalf("master-01 labels missing from %#v", cfg.StaticConfigs)
	}
	if len(master.Targets) != 1 || master.Targets[0] != "172.21.92.2:9091" {
		t.Fatalf("master-01 targets = %#v, want [172.21.92.2:9091]", master.Targets)
	}
	if master.Labels["roles"] != "master,balancer,egress" {
		t.Fatalf("master-01 roles label = %q", master.Labels["roles"])
	}
}

func TestRunTopologyGeneratePrometheusConfigSkipsWhenMetricsDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(path, []byte(`
schema_version: 2
mesh:
  name: no-metrics
  overlay_supernet: 172.21.92.0/24
nodes:
  - name: master-01
    roles: [master]
    overlay_ip: 172.21.92.1
`), 0o600); err != nil {
		t.Fatalf("write topology fixture: %v", err)
	}

	var out bytes.Buffer
	err := runTopologyGeneratePrometheusConfig(topologyPrometheusOptions{
		topologyPath: path,
		jobName:      "awg-mesh",
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runTopologyGeneratePrometheusConfig: %v", err)
	}
	got := decodePrometheusConfig(t, out.Bytes())
	if len(got.ScrapeConfigs) != 0 {
		t.Fatalf("scrape_configs = %#v, want empty", got.ScrapeConfigs)
	}
}

type prometheusConfigFixture struct {
	ScrapeConfigs []struct {
		JobName       string `yaml:"job_name"`
		StaticConfigs []struct {
			Targets []string          `yaml:"targets"`
			Labels  map[string]string `yaml:"labels"`
		} `yaml:"static_configs"`
	} `yaml:"scrape_configs"`
}

func decodePrometheusConfig(t *testing.T, data []byte) prometheusConfigFixture {
	t.Helper()
	var got prometheusConfigFixture
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode prometheus YAML: %v\n%s", err, string(data))
	}
	return got
}

func findStaticConfigByNode(configs []struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels"`
}, node string) *struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels"`
} {
	for i := range configs {
		if configs[i].Labels["node"] == node {
			return &configs[i]
		}
	}
	return nil
}
