package cmd

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/tls"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
	controlpb "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto/control_plane"
	"github.com/spf13/cobra"
)

const (
	nodeStatusDeclared       = "declared"
	defaultNodeInitTimeout   = 10 * time.Second
	defaultNodeRemoveTimeout = 10 * time.Second
	maxNodeDrainSeconds      = 1<<31 - 1
)

type nodePrepareOptions struct {
	nodeName     string
	topologyPath string
	configDir    string
	platform     string
	controlPlane string
	targetROS    string
	output       string
	stdout       io.Writer
}

type nodeInitOptions struct {
	nodeName     string
	topologyPath string
	configDir    string
	controlPlane string
	nodeVersion  string
	output       string
	timeout      time.Duration
	stdout       io.Writer
}

type nodeListOptions struct {
	topologyPath string
	output       string
	stdout       io.Writer
}

type nodeRemoveOptions struct {
	nodeName     string
	configDir    string
	controlPlane string
	drain        time.Duration
	output       string
	timeout      time.Duration
	stdout       io.Writer
}

type nodePrepareJSONOutput struct {
	NodeName                string `json:"node_name"`
	NodeDir                 string `json:"node_dir"`
	TokenPath               string `json:"token_path"`
	TokenHashPath           string `json:"token_hash_path"`
	CertPath                string `json:"cert_path"`
	KeyPath                 string `json:"key_path"`
	RouterOSScriptPath      string `json:"routeros_script_path,omitempty"`
	WireGuardPrivateKeyPath string `json:"wireguard_private_key_path,omitempty"`
	WireGuardPublicKeyPath  string `json:"wireguard_public_key_path,omitempty"`
}

type nodeInitJSONOutput struct {
	NodeName         string `json:"node_name"`
	Accepted         bool   `json:"accepted"`
	RegisteredAtUnix int64  `json:"registered_at_unix,omitempty"`
	RejectReason     string `json:"reject_reason,omitempty"`
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

func newNodeCommand(version string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Manage v2 mesh nodes",
	}

	cmd.AddCommand(newNodePrepareCommand())
	cmd.AddCommand(newNodeInitCommand(version))
	cmd.AddCommand(newNodeListCommand())
	cmd.AddCommand(newNodeRemoveCommand())
	return cmd
}

func newNodePrepareCommand() *cobra.Command {
	options := nodePrepareOptions{output: topologyOutputHuman}

	cmd := &cobra.Command{
		Use:   "prepare <name>",
		Short: "Prepare v2 node token and certificate material",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.nodeName = args[0]
			options.topologyPath = topologyPath
			options.configDir = configDir
			options.stdout = cmd.OutOrStdout()
			return runNodePrepareCommand(options)
		},
	}

	cmd.Flags().StringVar(&options.platform, "platform", "", "Prepare platform override (mikrotik)")
	cmd.Flags().StringVar(&options.controlPlane, "control-plane", "", "Responsible master coordination target address to embed in platform-specific runtime artifacts")
	cmd.Flags().StringVar(&options.targetROS, "target-ros", "", "Target RouterOS version for MikroTik container script dialect (default: 7.21+ canonical)")
	cmd.Flags().StringVar(&options.output, "output", topologyOutputHuman, "Output format (human, json)")
	return cmd
}

func newNodeInitCommand(version string) *cobra.Command {
	options := nodeInitOptions{
		nodeVersion: strings.TrimSpace(version),
		output:      topologyOutputHuman,
		timeout:     defaultNodeInitTimeout,
	}

	cmd := &cobra.Command{
		Use:   "init <name>",
		Short: "Register a prepared v2 node with the responsible master coordination target",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.nodeName = args[0]
			options.topologyPath = topologyPath
			options.configDir = configDir
			options.stdout = cmd.OutOrStdout()
			return runNodeInitCommand(options)
		},
	}

	cmd.Flags().StringVar(&options.controlPlane, "control-plane", "", "Responsible master coordination gRPC address (compatibility flag name)")
	cmd.Flags().StringVar(&options.output, "output", topologyOutputHuman, "Output format (human, json)")
	cmd.Flags().DurationVar(&options.timeout, "timeout", defaultNodeInitTimeout, "Init timeout")
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
		Short: "Decommission a node through the responsible master coordination target",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.nodeName = args[0]
			options.configDir = configDir
			options.stdout = cmd.OutOrStdout()
			return runNodeRemoveCommand(options)
		},
	}

	cmd.Flags().StringVar(&options.controlPlane, "control-plane", "", "Responsible master coordination gRPC address (compatibility flag name)")
	cmd.Flags().DurationVar(&options.drain, "drain", 0, "Drain window before peer removal")
	cmd.Flags().StringVar(&options.output, "output", topologyOutputHuman, "Output format (human, json)")
	cmd.Flags().DurationVar(&options.timeout, "timeout", defaultNodeRemoveTimeout, "Remove timeout")
	return cmd
}

func runNodePrepareCommand(options nodePrepareOptions) error {
	output, err := normalizeTopologyOutput(options.output)
	if err != nil {
		return err
	}
	topo, err := loadTopologyV2(options.topologyPath)
	if err != nil {
		return err
	}
	node, err := findTopologyNode(topo, options.nodeName)
	if err != nil {
		return err
	}
	nd, err := safeNodeConfigDir(options.configDir, node.Name)
	if err != nil {
		return err
	}

	caCert, caKey, err := loadOrCreateMeshCA(options.configDir, topo.Mesh.Name)
	if err != nil {
		return fmt.Errorf("prepare mesh CA: %w", err)
	}
	certPEM, keyPEM, err := pkgtls.IssueCert(caCert, caKey, node.Name, nodeCertificateHosts(node))
	if err != nil {
		return fmt.Errorf("issue node certificate for %q: %w", node.Name, err)
	}
	if err := pkgtls.SaveCertKey(nd, certPEM, keyPEM); err != nil {
		return fmt.Errorf("save node certificate for %q: %w", node.Name, err)
	}

	token, err := pkgtls.GenerateToken()
	if err != nil {
		return fmt.Errorf("generate token for %q: %w", node.Name, err)
	}
	tokenHash, err := pkgtls.HashToken(token)
	if err != nil {
		return fmt.Errorf("hash token for %q: %w", node.Name, err)
	}
	if err := saveToken(nd, token); err != nil {
		return fmt.Errorf("save raw token for %q: %w", node.Name, err)
	}
	if err := pkgtls.SaveTokenHash(nd, tokenHash); err != nil {
		return fmt.Errorf("save token hash for %q: %w", node.Name, err)
	}

	result := nodePrepareJSONOutput{
		NodeName:      node.Name,
		NodeDir:       nd,
		TokenPath:     filepath.Join(nd, "token"),
		TokenHashPath: filepath.Join(nd, "mesh.token"),
		CertPath:      filepath.Join(nd, "node.crt"),
		KeyPath:       filepath.Join(nd, "node.key"),
	}
	switch platform := preparePlatform(options.platform, node); platform {
	case "", "linux":
		_, _, wgPaths, err := loadOrCreateWireGuardKeyPair(nd, "wireguard-private.key", "wireguard-public.key")
		if err != nil {
			return fmt.Errorf("load or create wireguard key pair for %q: %w", node.Name, err)
		}
		result.WireGuardPrivateKeyPath = wgPaths.PrivateKeyPath
		result.WireGuardPublicKeyPath = wgPaths.PublicKeyPath
	case "mikrotik":
		if strings.TrimSpace(options.controlPlane) == "" {
			return fmt.Errorf("--control-plane is required as the responsible master coordination target for --platform mikrotik")
		}
		artifacts, err := prepareMikrotikRouterOS(topo, node, options.configDir, nd, tokenHash, options.controlPlane, options.targetROS)
		if err != nil {
			return err
		}
		result.RouterOSScriptPath = artifacts.RouterOSScriptPath
		result.WireGuardPrivateKeyPath = artifacts.WireGuardPrivateKeyPath
		result.WireGuardPublicKeyPath = artifacts.WireGuardPublicKeyPath
	default:
		return fmt.Errorf("unsupported prepare platform %q (supported: mikrotik)", platform)
	}
	return writeNodePrepareResult(commandOutput(options.stdout), output, result)
}

func runNodeInitCommand(options nodeInitOptions) error {
	validated, err := validateNodeInitOptions(options)
	if err != nil {
		return err
	}
	topo, err := loadTopologyV2(validated.topologyPath)
	if err != nil {
		return err
	}
	node, err := findTopologyNode(topo, validated.nodeName)
	if err != nil {
		return err
	}
	nd, err := safeNodeConfigDir(validated.configDir, node.Name)
	if err != nil {
		return err
	}
	if _, err := pkgtls.LoadCertKey(nd); err != nil {
		return fmt.Errorf("load prepared node cert/key for %q: %w", node.Name, err)
	}
	certPEM, err := os.ReadFile(filepath.Join(nd, "node.crt"))
	if err != nil {
		return fmt.Errorf("read prepared node certificate for %q: %w", node.Name, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), validated.timeout)
	defer cancel()

	conn, err := newControlPlaneAdminConn(validated.controlPlane, validated.configDir)
	if err != nil {
		return fmt.Errorf("connect coordination target %q: %w", validated.controlPlane, err)
	}
	defer func() { _ = conn.Close() }()

	var pubkeyBytes []byte
	pubkeyPath := filepath.Join(nd, "wireguard-public.key")
	if raw, err := os.ReadFile(pubkeyPath); err == nil {
		trimmed := strings.TrimSpace(string(raw))
		if decoded, decErr := base64.StdEncoding.DecodeString(trimmed); decErr == nil && len(decoded) == 32 {
			pubkeyBytes = decoded
		}
	}

	endpointHost, err := nodeAdvertisedMeshEndpoint(node)
	if err != nil {
		return err
	}

	protocol := node.MeshProtocol
	if protocol == "" {
		protocol = "amneziawg"
	}

	resp, err := controlpb.NewControlPlaneClient(conn).RegisterNode(ctx, &controlpb.RegisterNodeRequest{
		NodeName:     node.Name,
		Roles:        roleStrings(node.Roles),
		NodeCertPem:  append([]byte(nil), certPEM...),
		NodeVersion:  validated.nodeVersion,
		OverlayIp:    node.OverlayIP,
		Region:       node.Region,
		Pubkey:       pubkeyBytes,
		EndpointHost: endpointHost,
		Protocol:     protocol,
	})
	if err != nil {
		return fmt.Errorf("register node %q: %w", node.Name, err)
	}
	if resp == nil {
		return fmt.Errorf("register node %q: coordination target returned nil response", node.Name)
	}

	result := nodeInitJSONOutput{
		NodeName:         node.Name,
		Accepted:         resp.GetAccepted(),
		RegisteredAtUnix: resp.GetRegisteredAtUnix(),
		RejectReason:     resp.GetRejectReason(),
	}
	if !resp.GetAccepted() {
		detail := strings.TrimSpace(resp.GetRejectReason())
		if detail == "" {
			detail = "coordination target rejected registration"
		}
		return fmt.Errorf("register node %q rejected: %s", node.Name, detail)
	}
	return writeNodeInitResult(commandOutput(validated.stdout), validated.output, result)
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

	conn, err := newControlPlaneAdminConn(validated.controlPlane, validated.configDir)
	if err != nil {
		return fmt.Errorf("connect coordination target %q: %w", validated.controlPlane, err)
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
			detail = "coordination target rejected decommission"
		}
		return fmt.Errorf("decommission node %q: %s", validated.nodeName, detail)
	}
	return writeNodeRemoveResult(commandOutput(validated.stdout), validated.output, result)
}

func validateNodeInitOptions(options nodeInitOptions) (nodeInitOptions, error) {
	nodeName := strings.TrimSpace(options.nodeName)
	if nodeName == "" {
		return nodeInitOptions{}, fmt.Errorf("node name is required")
	}
	controlPlane := strings.TrimSpace(options.controlPlane)
	if controlPlane == "" {
		return nodeInitOptions{}, fmt.Errorf("--control-plane is required as the responsible master coordination target")
	}
	nodeVersion := strings.TrimSpace(options.nodeVersion)
	if nodeVersion == "" {
		nodeVersion = "dev"
	}
	output, err := normalizeTopologyOutput(options.output)
	if err != nil {
		return nodeInitOptions{}, err
	}
	timeout := options.timeout
	if timeout <= 0 {
		timeout = defaultNodeInitTimeout
	}
	return nodeInitOptions{
		nodeName:     nodeName,
		topologyPath: options.topologyPath,
		configDir:    options.configDir,
		controlPlane: controlPlane,
		nodeVersion:  nodeVersion,
		output:       output,
		timeout:      timeout,
		stdout:       options.stdout,
	}, nil
}

func validateNodeRemoveOptions(options nodeRemoveOptions) (nodeRemoveOptions, error) {
	nodeName := strings.TrimSpace(options.nodeName)
	if nodeName == "" {
		return nodeRemoveOptions{}, fmt.Errorf("node name is required")
	}
	controlPlane := strings.TrimSpace(options.controlPlane)
	if controlPlane == "" {
		return nodeRemoveOptions{}, fmt.Errorf("--control-plane is required as the responsible master coordination target")
	}
	if options.drain < 0 {
		return nodeRemoveOptions{}, fmt.Errorf("--drain must be >= 0")
	}
	if options.drain%time.Second != 0 {
		return nodeRemoveOptions{}, fmt.Errorf("--drain must be specified in whole seconds")
	}
	drainSec := int64(options.drain / time.Second)
	if drainSec > maxNodeDrainSeconds {
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
		configDir:    options.configDir,
		controlPlane: controlPlane,
		drain:        time.Duration(drainSec) * time.Second,
		output:       output,
		timeout:      timeout,
		stdout:       options.stdout,
	}, nil
}

func writeNodePrepareResult(out io.Writer, output string, result nodePrepareJSONOutput) error {
	switch output {
	case topologyOutputHuman:
		line := fmt.Sprintf("node %q prepared: node_dir=%s token=%s token_hash=%s cert=%s key=%s",
			result.NodeName, result.NodeDir, result.TokenPath, result.TokenHashPath, result.CertPath, result.KeyPath)
		if result.RouterOSScriptPath != "" {
			line += fmt.Sprintf(" routeros_script=%s wireguard_private_key=%s wireguard_public_key=%s",
				result.RouterOSScriptPath, result.WireGuardPrivateKeyPath, result.WireGuardPublicKeyPath)
		} else if result.WireGuardPrivateKeyPath != "" {
			line += fmt.Sprintf(" wireguard_private_key=%s wireguard_public_key=%s",
				result.WireGuardPrivateKeyPath, result.WireGuardPublicKeyPath)
		}
		_, err := fmt.Fprintln(out, line)
		return err
	case topologyOutputJSON:
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	default:
		return fmt.Errorf("unsupported node prepare output %q", output)
	}
}

func writeNodeInitResult(out io.Writer, output string, result nodeInitJSONOutput) error {
	switch output {
	case topologyOutputHuman:
		_, err := fmt.Fprintf(out, "node %q registered: registered_at_unix=%d\n", result.NodeName, result.RegisteredAtUnix)
		return err
	case topologyOutputJSON:
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	default:
		return fmt.Errorf("unsupported node init output %q", output)
	}
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

func findTopologyNode(topo *topology.TopologyV2, name string) (topology.NodeV2, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return topology.NodeV2{}, fmt.Errorf("node name is required")
	}
	if topo == nil {
		return topology.NodeV2{}, fmt.Errorf("topology is required")
	}
	for _, node := range topo.Nodes {
		if node.Name == trimmed {
			return node, nil
		}
	}
	return topology.NodeV2{}, fmt.Errorf("node %q is not declared in schema v2 topology", trimmed)
}

func safeNodeConfigDir(configDir, name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("node name is required")
	}
	if trimmed == "." || trimmed == ".." || strings.ContainsAny(trimmed, `/\`) {
		return "", fmt.Errorf("node name %q must be a single path segment; legacy role-specific output paths are not supported", name)
	}

	root := filepath.Clean(filepath.Join(configDir, "nodes"))
	dir := filepath.Clean(filepath.Join(root, trimmed))
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("node name %q resolves outside nodes directory", name)
	}
	return dir, nil
}

func loadOrCreateMeshCA(configDir, meshName string) (*x509.Certificate, crypto.PrivateKey, error) {
	certPath := filepath.Join(configDir, "ca.crt")
	keyPath := filepath.Join(configDir, "ca.key")
	certMissing := fileMissing(certPath)
	keyMissing := fileMissing(keyPath)
	if certMissing && keyMissing {
		commonName := strings.TrimSpace(meshName)
		if commonName == "" {
			commonName = "awg-mesh"
		}
		cert, key, err := pkgtls.GenerateCA(commonName + " CA")
		if err != nil {
			return nil, nil, err
		}
		if err := pkgtls.SaveCA(configDir, cert, key); err != nil {
			return nil, nil, err
		}
		return cert, key, nil
	}
	if certMissing != keyMissing {
		return nil, nil, fmt.Errorf("mesh CA is incomplete: both ca.crt and ca.key are required")
	}
	return pkgtls.LoadCA(configDir)
}

func fileMissing(path string) bool {
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

func nodeCertificateHosts(node topology.NodeV2) []string {
	candidates := []string{node.Name, node.OverlayIP, node.BridgeIP, node.PublicIP}
	hosts := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		hosts = append(hosts, trimmed)
	}
	return hosts
}

func nodeAdvertisedMeshEndpoint(node topology.NodeV2) (string, error) {
	if endpoint := strings.TrimSpace(node.MeshEndpoint); endpoint != "" {
		if err := validateNodeMeshEndpoint(endpoint); err != nil {
			return "", fmt.Errorf("node %q mesh_endpoint: %w", node.Name, err)
		}
		return endpoint, nil
	}
	publicIP := strings.TrimSpace(node.PublicIP)
	if publicIP == "" {
		return "", nil
	}
	if _, _, err := net.SplitHostPort(publicIP); err == nil {
		return "", fmt.Errorf("node %q public_ip must not include a port; use mesh_endpoint for explicit host:port", node.Name)
	}
	endpoint := net.JoinHostPort(strings.Trim(publicIP, "[]"), strconv.Itoa(nodeMeshListenPort(node)))
	if err := validateNodeMeshEndpoint(endpoint); err != nil {
		return "", fmt.Errorf("node %q public_ip: %w", node.Name, err)
	}
	return endpoint, nil
}

func nodeMeshListenPort(node topology.NodeV2) int {
	if node.MeshListenPort > 0 {
		return node.MeshListenPort
	}
	return wg.DefaultMeshListenPort
}

func validateNodeMeshEndpoint(endpoint string) error {
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return fmt.Errorf("must be host:port: %w", err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("must include a non-empty host")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("has invalid port %q", portText)
	}
	return nil
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
