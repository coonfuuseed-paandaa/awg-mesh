package mikrotik

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"text/template"
)

const deployScriptTemplate = `# awg-mesh RouterOS deployment script
# Generated for container {{.ContainerName}}
# Topology node name: {{.TopologyName}}
{{- if .CoordinationTarget }}
# Responsible master coordination target: {{.CoordinationTarget}}
{{- end }}
#
# KEYPAIR NOTE:
# The awg-mesh-client container generates its own AmneziaWG keypair on first boot.
# After running this script and starting the container, run:
#
#   mesh-ctl client init {{.TopologyName}}
#
# from the admin workstation. The init RPC reads the node's public key and
# registers it with all masters via gRPC — no manual key exchange needed.

# === Veth ===
{{- range .VethCommands }}
{{ . }}
{{- end }}

# === Bridge (idempotent — shared across awg-mesh containers) ===
{{- range .BridgeCommands }}
{{ . }}
{{- end }}

# === NAT ===
{{- range .NATCommands }}
{{ . }}
{{- end }}

# === Firewall (placed before defconf drop rules) ===
{{- range .FirewallCommands }}
{{ . }}
{{- end }}

# === Route: overlay space → container ===
{{- range .RouteCommands }}
{{ . }}
{{- end }}

# === Mount + Container ===
{{- range .ContainerCommands }}
{{ . }}
{{- end }}

{{ .StartCommand }}
`

const rotateScriptTemplate = `# awg-mesh AWG parameter rotation script
# Generated for container {{.ContainerName}}

:local envList {{ .EnvListLiteral }}
:local params {{ .ParamsLiteral }}
:local existing [/container/envs/find where list=$envList and key="MESH_AWG_PARAMS"]
:if ([:len $existing] = 0) do={
    /container/envs/add list=$envList key=MESH_AWG_PARAMS value=$params
} else={
    /container/envs/set $existing value=$params
}
/container/restart [find where name={{ .ContainerNameLiteral }}]
`

// DeployScript holds data for generating a full RouterOS .rsc deployment script.
//
// TokenHash is the bcrypt hash of the bearer token — never the plaintext. The
// node binary bootstraps /config/mesh.token from this value on first boot
// (see cmd/awg-mesh-node/main.go:bootstrapTokenHash).
type DeployScript struct {
	TopologyName  string   // original name from topology (e.g. "mikrotik-home")
	ContainerName string   // CAPS name for RouterOS (e.g. "AWG_MESH_HOME")
	BridgeName    string   // shared bridge name (default: BR_AWG_MESH)
	Image         string   // container image reference
	Veth          string   // veth interface name (CAPS, e.g. "AWG_MESH_HOME")
	VethGateway   string   // gateway IP or CIDR for veth (default: 100.127.0.1)
	OverlayIP     string   // client overlay IP from topology
	OverlayNet    string   // overlay CIDR from topology (e.g. "172.20.70.0/24")
	TokenHash     string   // bcrypt hash of bearer token
	NodeCertB64   string   // base64-encoded node.crt copied into /config on first boot
	NodeKeyB64    string   // base64-encoded node.key copied into /config on first boot
	CACertB64     string   // optional base64-encoded ca.crt copied into /config
	ControlPlane  string   // optional responsible master coordination target for runnable clientd command
	Region        string   // node region passed to clientd
	Protocol      string   // clientd protocol (default: amneziawg)
	InterfaceName string   // clientd interface name (default: awg-mesh0)
	TargetROS     string   // RouterOS target version for container CLI dialect selection
	DNS           []string // DNS servers (default: ["1.1.1.1", "8.8.8.8"])
	GRPCPort      int      // gRPC management port for dstnat (default: 9090)
	StorageRoot   string   // container storage root on MikroTik (default: "docker" when empty)
}

// GenerateDeployRSC generates a full .rsc script importable via /import.
func GenerateDeployRSC(ds DeployScript) (string, error) {
	if err := validateDeployScript(ds); err != nil {
		return "", err
	}
	coordinationTarget := strings.TrimSpace(ds.ControlPlane)

	envVars := buildDeployEnvVars(ds)
	mountName := DeriveMountName(ds.ContainerName)
	storageRoot := ds.StorageRoot
	if storageRoot == "" {
		storageRoot = "docker"
	}
	mountSrc := "/" + storageRoot + "/etc/awg-mesh-client-" + strings.ToLower(ds.TopologyName) + "-config"

	containerCfg := ContainerConfig{
		Name:             ds.ContainerName,
		Image:            ds.Image,
		Interface:        ds.Veth,
		RootDir:          "/" + storageRoot + "/awg-mesh-client-" + strings.ToLower(ds.TopologyName),
		MountName:        mountName,
		MountSrc:         mountSrc,
		DNS:              ds.DNS,
		EnvVars:          envVars,
		Command:          buildClientCommand(ds),
		TargetROSVersion: ds.TargetROS,
	}
	containerCommands, err := GenerateContainerCommandsForTarget(containerCfg)
	if err != nil {
		return "", err
	}

	templateData := struct {
		ContainerName      string
		TopologyName       string
		BridgeCommands     []string
		VethCommands       []string
		NATCommands        []string
		FirewallCommands   []string
		RouteCommands      []string
		ContainerCommands  []string
		StartCommand       string
		CoordinationTarget string
	}{
		ContainerName:      ds.ContainerName,
		TopologyName:       ds.TopologyName,
		BridgeCommands:     GenerateBridgeCommands(ds.BridgeName, ds.Veth, ds.VethGateway),
		VethCommands:       GenerateVethCommands(ds.Veth, ds.VethGateway),
		NATCommands:        GenerateNATCommands(ds.VethGateway, ds.GRPCPort),
		FirewallCommands:   GenerateFirewallCommands(ds.BridgeName),
		RouteCommands:      GenerateRouteCommands(ds.OverlayNet, ds.VethGateway),
		ContainerCommands:  containerCommands,
		StartCommand:       "# RouterOS starts the container after local image import; start-on-boot=yes keeps it running after reboot.",
		CoordinationTarget: coordinationTarget,
	}

	script, err := executeTemplate("deploy-rsc", deployScriptTemplate, templateData)
	if err != nil {
		return "", fmt.Errorf("render deploy script: %w", err)
	}
	return script, nil
}

// RotateParams holds parameters for an AWG rotation script.
type RotateParams struct {
	Jc   int
	Jmin int
	Jmax int
	S1   int
	S2   int
	H1   int
	H2   int
	H3   int
	H4   int
}

// GenerateRotateRSC generates a minimal RouterOS script to update AWG parameters.
func GenerateRotateRSC(containerName string, params RotateParams) (string, error) {
	trimmedName := strings.TrimSpace(containerName)
	if trimmedName == "" {
		return "", fmt.Errorf("container name is required")
	}
	if err := validateRotateParams(params); err != nil {
		return "", err
	}

	encodedParams := fmt.Sprintf(
		"jc=%d,jmin=%d,jmax=%d,s1=%d,s2=%d,h1=%d,h2=%d,h3=%d,h4=%d",
		params.Jc, params.Jmin, params.Jmax,
		params.S1, params.S2,
		params.H1, params.H2, params.H3, params.H4,
	)

	templateData := struct {
		ContainerName        string
		EnvListLiteral       string
		ParamsLiteral        string
		ContainerNameLiteral string
	}{
		ContainerName:        trimmedName,
		EnvListLiteral:       quoteRouterOSValue(DeriveEnvListName(trimmedName)),
		ParamsLiteral:        quoteRouterOSValue(encodedParams),
		ContainerNameLiteral: quoteRouterOSValue(trimmedName),
	}

	script, err := executeTemplate("rotate-rsc", rotateScriptTemplate, templateData)
	if err != nil {
		return "", fmt.Errorf("render rotate script: %w", err)
	}
	return script, nil
}

func validateDeployScript(ds DeployScript) error {
	if strings.TrimSpace(ds.ContainerName) == "" {
		return fmt.Errorf("container name is required")
	}
	if strings.TrimSpace(ds.TopologyName) == "" {
		return fmt.Errorf("topology name is required")
	}
	if strings.TrimSpace(ds.Image) == "" {
		return fmt.Errorf("container image is required")
	}
	if strings.TrimSpace(ds.Veth) == "" {
		return fmt.Errorf("veth interface is required")
	}
	if strings.TrimSpace(ds.TokenHash) == "" {
		return fmt.Errorf("token hash is required")
	}
	if strings.TrimSpace(ds.OverlayIP) == "" {
		return fmt.Errorf("overlay IP is required")
	}
	if strings.TrimSpace(ds.OverlayNet) == "" {
		return fmt.Errorf("overlay network is required")
	}
	if err := validateControlPlaneTarget(ds.ControlPlane); err != nil {
		return err
	}

	if _, err := netip.ParseAddr(ds.OverlayIP); err != nil {
		return fmt.Errorf("invalid overlay IP %q: %w", ds.OverlayIP, err)
	}
	if _, err := netip.ParsePrefix(ds.OverlayNet); err != nil {
		return fmt.Errorf("invalid overlay network %q: %w", ds.OverlayNet, err)
	}

	return nil
}

func validateControlPlaneTarget(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	fields := strings.Fields(trimmed)
	if len(fields) != 1 || !strings.Contains(trimmed, ":") {
		return fmt.Errorf("control-plane must be a single-line host:port value")
	}
	host, port, err := net.SplitHostPort(trimmed)
	if err != nil || strings.TrimSpace(host) == "" || strings.TrimSpace(port) == "" {
		return fmt.Errorf("control-plane must be a single-line host:port value")
	}
	return nil
}

func buildDeployEnvVars(ds DeployScript) map[string]string {
	envVars := map[string]string{
		"MESH_TOKEN_HASH": strings.TrimSpace(ds.TokenHash),
		"MESH_MODE":       "client",
		"MESH_NAME":       strings.TrimSpace(ds.TopologyName),
		"MESH_OVERLAY_IP": strings.TrimSpace(ds.OverlayIP),
	}
	if strings.TrimSpace(ds.NodeCertB64) != "" {
		envVars["MESH_NODE_CERT_B64"] = strings.TrimSpace(ds.NodeCertB64)
	}
	if strings.TrimSpace(ds.NodeKeyB64) != "" {
		envVars["MESH_NODE_KEY_B64"] = strings.TrimSpace(ds.NodeKeyB64)
	}
	if strings.TrimSpace(ds.CACertB64) != "" {
		envVars["MESH_CA_CERT_B64"] = strings.TrimSpace(ds.CACertB64)
	}
	return envVars
}

func buildClientCommand(ds DeployScript) string {
	controlPlane := strings.TrimSpace(ds.ControlPlane)
	if controlPlane == "" {
		return ""
	}
	region := strings.TrimSpace(ds.Region)
	if region == "" {
		region = "default"
	}
	protocol := strings.TrimSpace(ds.Protocol)
	if protocol == "" {
		protocol = "amneziawg"
	}
	iface := strings.TrimSpace(ds.InterfaceName)
	if iface == "" {
		iface = "awg-mesh0"
	}
	args := []string{
		"--mode", "client",
		"--control-plane", controlPlane,
		"--name", strings.TrimSpace(ds.TopologyName),
		"--overlay-ip", strings.TrimSpace(ds.OverlayIP),
		"--region", region,
		"--cert", "/config/node.crt",
		"--key", "/config/node.key",
		"--ca-cert", "/config/ca.crt",
		"--state-dir", "/config",
		"--iface", iface,
		"--protocol", protocol,
	}
	return strings.Join(args, " ")
}

func validateRotateParams(params RotateParams) error {
	values := []struct {
		name  string
		value int
	}{
		{name: "jc", value: params.Jc},
		{name: "jmin", value: params.Jmin},
		{name: "jmax", value: params.Jmax},
		{name: "s1", value: params.S1},
		{name: "s2", value: params.S2},
		{name: "h1", value: params.H1},
		{name: "h2", value: params.H2},
		{name: "h3", value: params.H3},
		{name: "h4", value: params.H4},
	}

	for _, current := range values {
		if current.value < 0 {
			return fmt.Errorf("%s must be >= 0", current.name)
		}
	}
	if params.Jmin > params.Jmax {
		return fmt.Errorf("jmin (%d) must be <= jmax (%d)", params.Jmin, params.Jmax)
	}

	return nil
}

func executeTemplate(name, source string, data any) (string, error) {
	compiledTemplate, err := template.New(name).Option("missingkey=error").Parse(source)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	var output strings.Builder
	if err := compiledTemplate.Execute(&output, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}

	return output.String(), nil
}
