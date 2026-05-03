package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	controlpb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
	"google.golang.org/protobuf/proto"
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

func TestRunNodePrepareCommandWritesV2TokenMaterialAndCerts(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeNodeLifecycleTopology(t, dir, "master-01")
	configDir := filepath.Join(dir, "config")

	var out bytes.Buffer
	err := runNodePrepareCommand(nodePrepareOptions{
		nodeName:     "master-01",
		topologyPath: topologyPath,
		configDir:    configDir,
		output:       "json",
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runNodePrepareCommand: %v", err)
	}

	var got nodePrepareJSONOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode prepare JSON: %v\n%s", err, out.String())
	}
	if got.NodeName != "master-01" || got.NodeDir != filepath.Join(configDir, "nodes", "master-01") {
		t.Fatalf("unexpected prepare JSON: %+v", got)
	}

	nd := filepath.Join(configDir, "nodes", "master-01")
	tokenBytes, err := os.ReadFile(filepath.Join(nd, "token"))
	if err != nil {
		t.Fatalf("read raw token: %v", err)
	}
	hashBytes, err := os.ReadFile(filepath.Join(nd, "mesh.token"))
	if err != nil {
		t.Fatalf("read token hash: %v", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	hash := strings.TrimSpace(string(hashBytes))
	if !strings.HasPrefix(hash, "mesh1.") {
		t.Fatalf("token hash %q does not use v2 mesh1 format", hash)
	}
	if err := pkgtls.VerifyToken(token, hash); err != nil {
		t.Fatalf("token hash does not verify against raw token: %v", err)
	}

	certPEM, err := os.ReadFile(filepath.Join(nd, "node.crt"))
	if err != nil {
		t.Fatalf("read node.crt: %v", err)
	}
	commonName, _, err := pkgtls.CertInfo(certPEM)
	if err != nil {
		t.Fatalf("parse node cert: %v", err)
	}
	if commonName != "master-01" {
		t.Fatalf("node cert common name = %q, want master-01", commonName)
	}
	if _, err := os.Stat(filepath.Join(nd, "node.key")); err != nil {
		t.Fatalf("node.key missing: %v", err)
	}

	for _, legacy := range []string{"masters", "endpoints", "clients"} {
		if _, err := os.Stat(filepath.Join(configDir, legacy)); !os.IsNotExist(err) {
			t.Fatalf("prepare wrote legacy role-specific directory %q", legacy)
		}
	}
}

func TestRunNodePrepareCommandWritesMikrotikRouterOSScriptAndKeys(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeMikrotikPrepareTopology(t, dir)
	configDir := filepath.Join(dir, "config")

	var out bytes.Buffer
	err := runNodePrepareCommand(nodePrepareOptions{
		nodeName:     "router-01",
		topologyPath: topologyPath,
		configDir:    configDir,
		platform:     "mikrotik",
		output:       "json",
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runNodePrepareCommand: %v", err)
	}

	var got nodePrepareJSONOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode prepare JSON: %v\n%s", err, out.String())
	}
	nodeDir := filepath.Join(configDir, "nodes", "router-01")
	if got.RouterOSScriptPath != filepath.Join(nodeDir, routerOSScriptFilename) {
		t.Fatalf("routeros_script_path = %q, want %q", got.RouterOSScriptPath, filepath.Join(nodeDir, routerOSScriptFilename))
	}
	if got.WireGuardPrivateKeyPath != filepath.Join(nodeDir, mikrotikWGPrivateKeyFile) {
		t.Fatalf("wireguard_private_key_path = %q", got.WireGuardPrivateKeyPath)
	}
	if got.WireGuardPublicKeyPath != filepath.Join(nodeDir, mikrotikWGPublicKeyFile) {
		t.Fatalf("wireguard_public_key_path = %q", got.WireGuardPublicKeyPath)
	}

	scriptBytes, err := os.ReadFile(got.RouterOSScriptPath)
	if err != nil {
		t.Fatalf("read RouterOS script: %v", err)
	}
	script := string(scriptBytes)
	masterAKey := readTrimmedFile(t, filepath.Join(configDir, "nodes", "master-a", masterClientWGPublicKeyFile))
	masterBKey := readTrimmedFile(t, filepath.Join(configDir, "nodes", "master-b", masterClientWGPublicKeyFile))
	for _, want := range []string{
		"/interface/wireguard",
		"private-key=\"",
		"add address=172.21.92.130/32 interface=awg-mesh",
		"public-key=\"" + masterAKey + "\" endpoint-address=203.0.113.10 endpoint-port=51820",
		"public-key=\"" + masterBKey + "\" endpoint-address=198.51.100.11 endpoint-port=51820",
		"allowed-address=172.21.92.2/32,172.21.92.34/32",
		"allowed-address=172.21.92.20/32,172.21.92.3/32",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("RouterOS script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(strings.ToLower(script), "amnezia") {
		t.Fatalf("native RouterOS script must not contain AmneziaWG settings:\n%s", script)
	}

	var secondOut bytes.Buffer
	if err := runNodePrepareCommand(nodePrepareOptions{
		nodeName:     "router-01",
		topologyPath: topologyPath,
		configDir:    configDir,
		platform:     "mikrotik",
		output:       "json",
		stdout:       &secondOut,
	}); err != nil {
		t.Fatalf("runNodePrepareCommand second run: %v", err)
	}
	secondScript, err := os.ReadFile(got.RouterOSScriptPath)
	if err != nil {
		t.Fatalf("read second RouterOS script: %v", err)
	}
	if script != string(secondScript) {
		t.Fatalf("RouterOS script changed after key reuse\nfirst:\n%s\nsecond:\n%s", script, string(secondScript))
	}
}

func TestRunNodePrepareCommandRejectsLegacyRoleSpecificNodePath(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeNodeLifecycleTopology(t, dir, "masters/master-01")

	err := runNodePrepareCommand(nodePrepareOptions{
		nodeName:     "masters/master-01",
		topologyPath: topologyPath,
		configDir:    filepath.Join(dir, "config"),
		output:       "human",
		stdout:       &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected legacy role-specific path rejection")
	}
	if !strings.Contains(err.Error(), "legacy role-specific output paths are not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunNodePrepareCommandRejectsUnknownNode(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeNodeLifecycleTopology(t, dir, "master-01")

	err := runNodePrepareCommand(nodePrepareOptions{
		nodeName:     "missing-node",
		topologyPath: topologyPath,
		configDir:    filepath.Join(dir, "config"),
		output:       "human",
		stdout:       &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected unknown node error")
	}
	if !strings.Contains(err.Error(), "missing-node") || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunNodeInitCommandRegistersPreparedNode(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeNodeLifecycleTopology(t, dir, "master-01")
	configDir := filepath.Join(dir, "config")

	if err := runNodePrepareCommand(nodePrepareOptions{
		nodeName:     "master-01",
		topologyPath: topologyPath,
		configDir:    configDir,
		output:       "human",
		stdout:       &bytes.Buffer{},
	}); err != nil {
		t.Fatalf("prepare node: %v", err)
	}

	server := &capturingRegisterServer{
		response: &controlpb.RegisterNodeResponse{Accepted: true, RegisteredAtUnix: 123},
	}
	addr, teardown := startAuditLogTestServer(t, server)
	defer teardown()

	var out bytes.Buffer
	err := runNodeInitCommand(nodeInitOptions{
		nodeName:     "master-01",
		topologyPath: topologyPath,
		configDir:    configDir,
		controlPlane: addr,
		nodeVersion:  "test-build-version",
		output:       "json",
		timeout:      2 * time.Second,
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runNodeInitCommand: %v", err)
	}

	req := server.capturedRegisterRequest(t)
	if req.GetNodeName() != "master-01" || req.GetOverlayIp() != "172.21.92.2" || req.GetRegion() != "ru" {
		t.Fatalf("unexpected RegisterNode identity: %+v", req)
	}
	if strings.Join(req.GetRoles(), ",") != "master,balancer" {
		t.Fatalf("roles = %#v, want master,balancer", req.GetRoles())
	}
	certPEM, err := os.ReadFile(filepath.Join(configDir, "nodes", "master-01", "node.crt"))
	if err != nil {
		t.Fatalf("read prepared cert: %v", err)
	}
	if !bytes.Equal(req.GetNodeCertPem(), certPEM) {
		t.Fatal("RegisterNode did not send the prepared node certificate PEM")
	}
	if req.GetNodeVersion() != "test-build-version" {
		t.Fatalf("RegisterNode node_version = %q, want test-build-version", req.GetNodeVersion())
	}

	var got nodeInitJSONOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode init JSON: %v\n%s", err, out.String())
	}
	if got.NodeName != "master-01" || !got.Accepted || got.RegisteredAtUnix != 123 {
		t.Fatalf("unexpected init JSON: %+v", got)
	}
}

func TestRunNodeInitCommandReportsRejectedRegistration(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeNodeLifecycleTopology(t, dir, "master-01")
	configDir := filepath.Join(dir, "config")

	if err := runNodePrepareCommand(nodePrepareOptions{
		nodeName:     "master-01",
		topologyPath: topologyPath,
		configDir:    configDir,
		output:       "human",
		stdout:       &bytes.Buffer{},
	}); err != nil {
		t.Fatalf("prepare node: %v", err)
	}

	server := &capturingRegisterServer{
		response: &controlpb.RegisterNodeResponse{Accepted: false, RejectReason: "missing cert"},
	}
	addr, teardown := startAuditLogTestServer(t, server)
	defer teardown()

	err := runNodeInitCommand(nodeInitOptions{
		nodeName:     "master-01",
		topologyPath: topologyPath,
		configDir:    configDir,
		controlPlane: addr,
		output:       "human",
		timeout:      2 * time.Second,
		stdout:       &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected rejected registration error")
	}
	if !strings.Contains(err.Error(), "master-01") || !strings.Contains(err.Error(), "missing cert") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunNodeInitCommandRequiresPreparedKeyPair(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeNodeLifecycleTopology(t, dir, "master-01")
	configDir := filepath.Join(dir, "config")

	if err := runNodePrepareCommand(nodePrepareOptions{
		nodeName:     "master-01",
		topologyPath: topologyPath,
		configDir:    configDir,
		output:       "human",
		stdout:       &bytes.Buffer{},
	}); err != nil {
		t.Fatalf("prepare node: %v", err)
	}
	if err := os.Remove(filepath.Join(configDir, "nodes", "master-01", "node.key")); err != nil {
		t.Fatalf("remove prepared key: %v", err)
	}

	err := runNodeInitCommand(nodeInitOptions{
		nodeName:     "master-01",
		topologyPath: topologyPath,
		configDir:    configDir,
		controlPlane: "127.0.0.1:1",
		output:       "human",
		timeout:      2 * time.Second,
		stdout:       &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected missing keypair error")
	}
	if !strings.Contains(err.Error(), "load prepared node cert/key") {
		t.Fatalf("unexpected error: %v", err)
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

func TestValidateNodeRemoveOptionsRejectsSubSecondDrain(t *testing.T) {
	_, err := validateNodeRemoveOptions(nodeRemoveOptions{
		nodeName:     "master-01",
		controlPlane: "127.0.0.1:51820",
		drain:        500 * time.Millisecond,
		output:       "human",
		timeout:      2 * time.Second,
		stdout:       &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected sub-second drain to fail")
	}
	if !strings.Contains(err.Error(), "whole seconds") {
		t.Fatalf("unexpected error: %v", err)
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

type capturingRegisterServer struct {
	controlpb.UnimplementedControlPlaneServer
	mu       sync.Mutex
	request  *controlpb.RegisterNodeRequest
	response *controlpb.RegisterNodeResponse
}

func (s *capturingRegisterServer) RegisterNode(_ context.Context, req *controlpb.RegisterNodeRequest) (*controlpb.RegisterNodeResponse, error) {
	cp := proto.Clone(req).(*controlpb.RegisterNodeRequest)
	s.mu.Lock()
	s.request = cp
	s.mu.Unlock()
	if s.response == nil {
		return &controlpb.RegisterNodeResponse{Accepted: true}, nil
	}
	return s.response, nil
}

func (s *capturingRegisterServer) capturedRegisterRequest(t *testing.T) *controlpb.RegisterNodeRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.request == nil {
		t.Fatal("RegisterNode was not called")
	}
	return s.request
}

type capturingDecommissionServer struct {
	controlpb.UnimplementedControlPlaneServer
	mu       sync.Mutex
	request  *controlpb.DecommissionRequest
	response *controlpb.DecommissionResponse
}

func (s *capturingDecommissionServer) DecommissionNode(_ context.Context, req *controlpb.DecommissionRequest) (*controlpb.DecommissionResponse, error) {
	cp := proto.Clone(req).(*controlpb.DecommissionRequest)
	s.mu.Lock()
	s.request = cp
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

func writeNodeLifecycleTopology(t *testing.T, dir, nodeName string) string {
	t.Helper()
	path := filepath.Join(dir, "topology.yml")
	data := fmt.Sprintf(`
schema_version: 2
mesh:
  name: lifecycle-test
  overlay_supernet: 172.21.92.0/24
nodes:
  - name: %q
    roles: [master, balancer]
    overlay_ip: 172.21.92.2
    bridge_ip: 192.168.93.2
    region: ru
`, nodeName)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write topology fixture: %v", err)
	}
	return path
}

func writeMikrotikPrepareTopology(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "mikrotik-topology.yml")
	data := `
schema_version: 2
mesh:
  name: mikrotik-test
  overlay_supernet: 172.21.92.0/24
nodes:
  - name: router-01
    roles: [client]
    platform: mikrotik
    overlay_ip: 172.21.92.130
  - name: ingress-de
    roles: [ingress]
    overlay_ip: 172.21.92.20
  - name: master-b
    roles: [master, balancer]
    overlay_ip: 172.21.92.3
    bridge_ip: 198.51.100.11
  - name: egress-us
    roles: [egress]
    overlay_ip: 172.21.92.34
  - name: master-a
    roles: [master, balancer]
    overlay_ip: 172.21.92.2
    public_ip: 203.0.113.10
    client_protocol: vanilla-wg
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write mikrotik topology fixture: %v", err)
	}
	return path
}

func readTrimmedFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(data))
}
