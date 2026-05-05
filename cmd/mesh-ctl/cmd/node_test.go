package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/awgmesh"
	controlplane "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/control_plane"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/tls"
	controlpb "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto/control_plane"
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
	if got.Nodes[0].Name != "egress-us-01" || got.Nodes[0].Status != "declared" {
		t.Fatalf("JSON output is not sorted/stable: %+v", got.Nodes[0])
	}
	if strings.Join(got.Nodes[0].Roles, ",") != "egress" {
		t.Fatalf("roles = %#v, want egress", got.Nodes[0].Roles)
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

func TestRunNodePrepareCommandWritesMikrotikContainerRouterOSScript(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeMikrotikPrepareTopology(t, dir)
	configDir := filepath.Join(dir, "config")

	var out bytes.Buffer
	err := runNodePrepareCommand(nodePrepareOptions{
		nodeName:     "router-01",
		topologyPath: topologyPath,
		configDir:    configDir,
		platform:     "mikrotik",
		controlPlane: "192.0.2.5:9090",
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
	if got.WireGuardPrivateKeyPath != "" {
		t.Fatalf("wireguard_private_key_path = %q, want empty for container path", got.WireGuardPrivateKeyPath)
	}
	if got.WireGuardPublicKeyPath != "" {
		t.Fatalf("wireguard_public_key_path = %q, want empty for container path", got.WireGuardPublicKeyPath)
	}
	if _, err := os.Stat(filepath.Join(nodeDir, mikrotikWGPrivateKeyFile)); !os.IsNotExist(err) {
		t.Fatalf("native WireGuard private key should not be generated for container path, stat err=%v", err)
	}

	scriptBytes, err := os.ReadFile(got.RouterOSScriptPath)
	if err != nil {
		t.Fatalf("read RouterOS script: %v", err)
	}
	script := string(scriptBytes)
	expectedClientImage := "ghcr.io/coonfuuseed-paandaa/awg-mesh-client:" + awgmesh.Version
	for _, want := range []string{
		"# awg-mesh RouterOS deployment script",
		"/interface/veth add name=AWG_MESH_ROUTER_01",
		"/container/mounts/add list=AWG_MESH_ROUTER_01_CONFIG",
		"/container/add interface=AWG_MESH_ROUTER_01",
		"remote-image=" + expectedClientImage,
		"envlist=AWG_MESH_ROUTER_01_ENVS",
		"key=MESH_TOKEN_HASH",
		"key=MESH_NODE_CERT_B64",
		"key=MESH_NODE_KEY_B64",
		"key=MESH_CA_CERT_B64",
		`cmd="--mode client --control-plane 192.0.2.5:9090 --name router-01 --overlay-ip 172.21.92.130 --region default --cert /config/node.crt --key /config/node.key --ca-cert /config/ca.crt --state-dir /config --iface awg-mesh0 --protocol amneziawg"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("RouterOS script missing %q:\n%s", want, redactRouterOSScript(script))
		}
	}
	if strings.Contains(script, "/interface/wireguard") {
		t.Fatalf("container RouterOS script must not configure native WireGuard:\n%s", redactRouterOSScript(script))
	}
}

func TestRunNodePrepareCommandRequiresMikrotikControlPlane(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeMikrotikPrepareTopology(t, dir)

	err := runNodePrepareCommand(nodePrepareOptions{
		nodeName:     "router-01",
		topologyPath: topologyPath,
		configDir:    filepath.Join(dir, "config"),
		platform:     "mikrotik",
		output:       "json",
		stdout:       io.Discard,
	})
	if err == nil {
		t.Fatal("expected --control-plane requirement")
	}
	if !strings.Contains(err.Error(), "--control-plane") {
		t.Fatalf("error %q does not mention --control-plane", err)
	}
	for _, want := range []string{"coordination target", "responsible master"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestPrepareMikrotikRouterOSRequiresResponsibleMasterCoordinationTarget(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeMikrotikPrepareTopology(t, dir)
	topo, err := loadTopologyV2(topologyPath)
	if err != nil {
		t.Fatalf("load topology: %v", err)
	}
	node, err := findTopologyNode(topo, "router-01")
	if err != nil {
		t.Fatalf("find node: %v", err)
	}

	_, err = prepareMikrotikRouterOS(topo, node, filepath.Join(dir, "config"), t.TempDir(), "mesh1.test", "", "7.21.4")
	if err == nil {
		t.Fatal("expected missing coordination target error")
	}
	for _, want := range []string{"--control-plane", "coordination target", "responsible master"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestValidateNodeInitOptionsRequiresResponsibleMasterCoordinationTarget(t *testing.T) {
	_, err := validateNodeInitOptions(nodeInitOptions{
		nodeName: "master-01",
		output:   "human",
		timeout:  2 * time.Second,
		stdout:   &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected missing coordination target error")
	}
	for _, want := range []string{"--control-plane", "coordination target", "responsible master"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}

	validated, err := validateNodeInitOptions(nodeInitOptions{
		nodeName:     "master-01",
		controlPlane: "master-01.example:9090",
		output:       "human",
		timeout:      2 * time.Second,
		stdout:       &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("validate responsible master target: %v", err)
	}
	if validated.controlPlane != "master-01.example:9090" {
		t.Fatalf("control-plane compatibility flag did not preserve coordination target: %+v", validated)
	}
}

func TestValidateNodeRemoveOptionsRequiresResponsibleMasterCoordinationTarget(t *testing.T) {
	_, err := validateNodeRemoveOptions(nodeRemoveOptions{
		nodeName: "master-01",
		output:   "human",
		timeout:  2 * time.Second,
		stdout:   &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected missing coordination target error")
	}
	for _, want := range []string{"--control-plane", "coordination target", "responsible master"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestRunNodePrepareCommandWritesLegacyMikrotikContainerDialect(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeMikrotikPrepareTopology(t, dir)
	configDir := filepath.Join(dir, "config")

	err := runNodePrepareCommand(nodePrepareOptions{
		nodeName:     "router-01",
		topologyPath: topologyPath,
		configDir:    configDir,
		platform:     "mikrotik",
		controlPlane: "192.0.2.5:9090",
		targetROS:    "7.16.2",
		output:       "json",
		stdout:       io.Discard,
	})
	if err != nil {
		t.Fatalf("runNodePrepareCommand: %v", err)
	}

	scriptBytes, err := os.ReadFile(filepath.Join(configDir, "nodes", "router-01", routerOSScriptFilename))
	if err != nil {
		t.Fatalf("read RouterOS script: %v", err)
	}
	script := string(scriptBytes)
	expectedClientImage := "ghcr.io/coonfuuseed-paandaa/awg-mesh-client:" + awgmesh.Version
	for _, want := range []string{
		"/container/mounts/add name=AWG_MESH_ROUTER_01_CONFIG",
		"/container/envs/add name=AWG_MESH_ROUTER_01_ENVS",
		"image=" + expectedClientImage,
		"mounts=AWG_MESH_ROUTER_01_CONFIG",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("RouterOS 7.16 script missing %q:\n%s", want, redactRouterOSScript(script))
		}
	}
	if strings.Contains(script, "mountlists=") || strings.Contains(script, "/container/envs/add list=") {
		t.Fatalf("RouterOS 7.16 script contains canonical dialect:\n%s", redactRouterOSScript(script))
	}
}

func TestPrepareMikrotikNativeWireGuardRouterOSRemainsDeferredGenerator(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeMikrotikPrepareTopology(t, dir)
	configDir := filepath.Join(dir, "config")

	topo, err := loadTopologyV2(topologyPath)
	if err != nil {
		t.Fatalf("load topology: %v", err)
	}
	node, err := findTopologyNode(topo, "router-01")
	if err != nil {
		t.Fatalf("find router-01: %v", err)
	}
	nodeDir, err := safeNodeConfigDir(configDir, node.Name)
	if err != nil {
		t.Fatalf("node dir: %v", err)
	}

	artifacts, err := prepareMikrotikNativeWireGuardRouterOS(topo, node, configDir, nodeDir)
	if err != nil {
		t.Fatalf("prepare native WireGuard generator: %v", err)
	}
	if artifacts.WireGuardPrivateKeyPath == "" || artifacts.WireGuardPublicKeyPath == "" {
		t.Fatalf("native generator did not report WireGuard key paths: %+v", artifacts)
	}
	scriptBytes, err := os.ReadFile(artifacts.RouterOSScriptPath)
	if err != nil {
		t.Fatalf("read native RouterOS script: %v", err)
	}
	script := string(scriptBytes)
	for _, want := range []string{
		"# awg-mesh v2 RouterOS native WireGuard client script",
		"/interface/wireguard/add",
		"/interface/wireguard/peers/add",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("native generator output missing %q:\n%s", want, script)
		}
	}
}

func TestLoadOrCreateWireGuardKeyPairConcurrentCreatesConsistentKeys(t *testing.T) {
	dir := t.TempDir()

	const runs = 16
	type result struct {
		private string
		public  string
		err     error
	}
	results := make(chan result, runs)

	var wait sync.WaitGroup
	for i := 0; i < runs; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			privateKey, publicKey, _, err := loadOrCreateWireGuardKeyPair(dir, "private.key", "public.key")
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{
				private: privateKey.String(),
				public:  publicKey.String(),
			}
		}()
	}
	wait.Wait()
	close(results)

	var first result
	for current := range results {
		if current.err != nil {
			t.Fatalf("loadOrCreateWireGuardKeyPair: %v", current.err)
		}
		if first.private == "" {
			first = current
			continue
		}
		if current.private != first.private || current.public != first.public {
			t.Fatalf("concurrent keypair creation returned inconsistent keys")
		}
	}

	if got := readTrimmedFile(t, filepath.Join(dir, "private.key")); got != first.private {
		t.Fatalf("private key file does not match returned key")
	}
	if got := readTrimmedFile(t, filepath.Join(dir, "public.key")); got != first.public {
		t.Fatalf("public key file does not match returned key")
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
	addr, _, teardown := startMTLSControlPlaneTestServer(t, configDir, server)
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

func TestRunNodeInitAndRemoveUsePreparedMTLS(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeClientLifecycleTopology(t, dir, "client-01")
	configDir := filepath.Join(dir, "config")

	if err := runNodePrepareCommand(nodePrepareOptions{
		nodeName:     "client-01",
		topologyPath: topologyPath,
		configDir:    configDir,
		output:       "human",
		stdout:       &bytes.Buffer{},
	}); err != nil {
		t.Fatalf("prepare node: %v", err)
	}

	addr, teardown := startCAControlPlaneDaemon(t, configDir)
	defer teardown()

	var initOut bytes.Buffer
	if err := runNodeInitCommand(nodeInitOptions{
		nodeName:     "client-01",
		topologyPath: topologyPath,
		configDir:    configDir,
		controlPlane: addr,
		nodeVersion:  "test-build-version",
		output:       "json",
		timeout:      2 * time.Second,
		stdout:       &initOut,
	}); err != nil {
		t.Fatalf("runNodeInitCommand against mTLS daemon: %v", err)
	}

	var initResult nodeInitJSONOutput
	if err := json.Unmarshal(initOut.Bytes(), &initResult); err != nil {
		t.Fatalf("decode init JSON: %v\n%s", err, initOut.String())
	}
	if initResult.NodeName != "client-01" || !initResult.Accepted {
		t.Fatalf("unexpected init JSON: %+v", initResult)
	}

	var removeOut bytes.Buffer
	if err := runNodeRemoveCommand(nodeRemoveOptions{
		nodeName:     "client-01",
		configDir:    configDir,
		controlPlane: addr,
		output:       "json",
		timeout:      2 * time.Second,
		stdout:       &removeOut,
	}); err != nil {
		t.Fatalf("runNodeRemoveCommand against mTLS daemon: %v", err)
	}

	var removeResult nodeRemoveJSONOutput
	if err := json.Unmarshal(removeOut.Bytes(), &removeResult); err != nil {
		t.Fatalf("decode remove JSON: %v\n%s", err, removeOut.String())
	}
	if removeResult.NodeName != "client-01" || !removeResult.Success {
		t.Fatalf("unexpected remove JSON: %+v", removeResult)
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
	addr, _, teardown := startMTLSControlPlaneTestServer(t, configDir, server)
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
	configDir := writeControlPlaneMTLSConfig(t)
	server := &capturingDecommissionServer{
		response: &controlpb.DecommissionResponse{Success: true, ReassignedOverlayCount: 3},
	}
	addr, _, teardown := startMTLSControlPlaneTestServer(t, configDir, server)
	defer teardown()

	var out bytes.Buffer
	err := runNodeRemoveCommand(nodeRemoveOptions{
		nodeName:     "master-01",
		configDir:    configDir,
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
	configDir := writeControlPlaneMTLSConfig(t)
	server := &capturingDecommissionServer{
		response: &controlpb.DecommissionResponse{Success: false, Error: "node not in registry"},
	}
	addr, _, teardown := startMTLSControlPlaneTestServer(t, configDir, server)
	defer teardown()

	var out bytes.Buffer
	err := runNodeRemoveCommand(nodeRemoveOptions{
		nodeName:     "missing-node",
		configDir:    configDir,
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

func startCAControlPlaneDaemon(t *testing.T, caDir string) (string, func()) {
	t.Helper()
	daemon, err := controlplane.NewDaemon(controlplane.Config{
		ListenAddr: "127.0.0.1:0",
		StateDir:   t.TempDir(),
		CADir:      caDir,
	})
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() { doneCh <- daemon.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for daemon.ListenerAddr() == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	addr := daemon.ListenerAddr()
	if addr == "" {
		cancel()
		t.Fatal("control-plane daemon never bound listener")
	}

	return addr, func() {
		cancel()
		select {
		case err := <-doneCh:
			if err != nil {
				t.Fatalf("control-plane daemon returned error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("control-plane daemon did not shut down within 5s")
		}
	}
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

func writeClientLifecycleTopology(t *testing.T, dir, nodeName string) string {
	t.Helper()
	path := filepath.Join(dir, "topology.yml")
	data := fmt.Sprintf(`
schema_version: 2
mesh:
  name: lifecycle-test
  overlay_supernet: 172.21.92.0/24
nodes:
  - name: %q
    roles: [client]
    overlay_ip: 172.21.92.130
    region: home
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

func redactRouterOSScript(script string) string {
	lines := strings.Split(script, "\n")
	for i, line := range lines {
		lines[i] = redactRouterOSPrivateKey(line)
	}
	return strings.Join(lines, "\n")
}

func redactRouterOSPrivateKey(line string) string {
	const marker = "private-key="
	start := strings.Index(line, marker)
	if start < 0 {
		return line
	}

	valueStart := start + len(marker)
	if valueStart >= len(line) {
		return line
	}

	if line[valueStart] == '"' {
		valueStart++
		valueEnd := strings.IndexByte(line[valueStart:], '"')
		if valueEnd < 0 {
			return line[:valueStart] + "<redacted>"
		}
		return line[:valueStart] + "<redacted>" + line[valueStart+valueEnd:]
	}

	valueEnd := strings.IndexAny(line[valueStart:], " \t")
	if valueEnd < 0 {
		return line[:valueStart] + "<redacted>"
	}
	return line[:valueStart] + "<redacted>" + line[valueStart+valueEnd:]
}
