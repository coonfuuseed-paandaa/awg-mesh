package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunNodeListCommandOutputsHuman(t *testing.T) {
	var out bytes.Buffer
	err := runNodeListCommand(nodeListOptions{
		topologyPath: v2TopologyFixture,
		output:       "human",
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runNodeListCommand: %v", err)
	}
	text := out.String()
	for _, want := range []string{"NAME", "OVERLAY_IP", "ROLES", "PLATFORM", "STATUS", "master-01", "172.21.92.2", "master,balancer,egress", "declared"} {
		if !strings.Contains(text, want) {
			t.Fatalf("node list output missing %q:\n%s", want, text)
		}
	}
}

func TestRunNodeListCommandOutputsStableJSON(t *testing.T) {
	var out bytes.Buffer
	err := runNodeListCommand(nodeListOptions{
		topologyPath: v2TopologyFixture,
		output:       "json",
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runNodeListCommand: %v", err)
	}

	var got nodeListJSONOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode JSON output: %v\n%s", err, out.String())
	}
	if got.Count != 5 || len(got.Nodes) != 5 {
		t.Fatalf("unexpected node list JSON: %+v", got)
	}
	if got.Nodes[0].Name != "control-plane-01" || got.Nodes[0].Status != "declared" {
		t.Fatalf("JSON output is not sorted/stable: %+v", got.Nodes[0])
	}
	if strings.Join(got.Nodes[0].Roles, ",") != "master,balancer" {
		t.Fatalf("roles = %#v, want master,balancer", got.Nodes[0].Roles)
	}
}

func TestRunNodeListCommandPreservesPlatform(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(path, []byte(`
schema_version: 2
mesh:
  name: platform-test
  overlay_supernet: 172.21.92.0/24
nodes:
  - name: router-01
    roles: [client]
    overlay_ip: 172.21.92.10
    platform: mikrotik
`), 0o600); err != nil {
		t.Fatalf("write topology fixture: %v", err)
	}

	var out bytes.Buffer
	err := runNodeListCommand(nodeListOptions{
		topologyPath: path,
		output:       "json",
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runNodeListCommand: %v", err)
	}

	var got nodeListJSONOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode JSON output: %v\n%s", err, out.String())
	}
	if len(got.Nodes) != 1 || got.Nodes[0].Platform != "mikrotik" {
		t.Fatalf("platform not preserved: %+v", got)
	}
}
