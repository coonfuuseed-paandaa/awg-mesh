package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	controlpb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
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

func TestRunNodeRemoveCommandSendsRequestAndOutputsJSON(t *testing.T) {
	server := &capturingDecommissionServer{
		response: &controlpb.DecommissionResponse{Success: true, ReassignedOverlayCount: 3},
	}
	addr, teardown := startAuditLogTestServer(t, server)
	defer teardown()

	var out bytes.Buffer
	err := runNodeRemoveCommand(nodeRemoveOptions{
		nodeName:     "master-01",
		controlPlane: addr,
		drain:        15 * time.Second,
		output:       "json",
		timeout:      2 * time.Second,
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runNodeRemoveCommand: %v", err)
	}

	req := server.capturedRequest(t)
	if req.GetNodeName() != "master-01" || req.GetDrainSeconds() != 15 {
		t.Fatalf("unexpected DecommissionNode request: %+v", req)
	}

	var got nodeRemoveJSONOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode JSON output: %v\n%s", err, out.String())
	}
	if got.NodeName != "master-01" || !got.Success || got.ReassignedOverlayCount != 3 {
		t.Fatalf("unexpected node remove JSON: %+v", got)
	}
}

func TestRunNodeRemoveCommandReportsServerFailure(t *testing.T) {
	server := &capturingDecommissionServer{
		response: &controlpb.DecommissionResponse{Success: false, Error: "node not in registry"},
	}
	addr, teardown := startAuditLogTestServer(t, server)
	defer teardown()

	var out bytes.Buffer
	err := runNodeRemoveCommand(nodeRemoveOptions{
		nodeName:     "missing-node",
		controlPlane: addr,
		output:       "human",
		timeout:      2 * time.Second,
		stdout:       &out,
	})
	if err == nil {
		t.Fatal("expected server failure")
	}
	if !strings.Contains(err.Error(), "missing-node") || !strings.Contains(err.Error(), "node not in registry") {
		t.Fatalf("unexpected error: %v", err)
	}
}

type capturingDecommissionServer struct {
	controlpb.UnimplementedControlPlaneServer
	mu       sync.Mutex
	request  *controlpb.DecommissionRequest
	response *controlpb.DecommissionResponse
}

func (s *capturingDecommissionServer) DecommissionNode(_ context.Context, req *controlpb.DecommissionRequest) (*controlpb.DecommissionResponse, error) {
	cp := *req
	s.mu.Lock()
	s.request = &cp
	s.mu.Unlock()
	if s.response == nil {
		return &controlpb.DecommissionResponse{Success: true}, nil
	}
	return s.response, nil
}

func (s *capturingDecommissionServer) capturedRequest(t *testing.T) *controlpb.DecommissionRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.request == nil {
		t.Fatal("DecommissionNode was not called")
	}
	return s.request
}
