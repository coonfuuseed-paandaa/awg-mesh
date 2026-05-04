package mikrotik

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultVethPrefixBits = 24
	// defaultVethSubnet is CGN space (RFC 6598) — avoids conflicts with
	// LAN subnets (192.168.x, 10.x, 172.16-31.x) on real routers.
	defaultVethSubnet  = "100.127.0.0/24"
	defaultVethAddress = "100.127.0.2/24" // container address
	defaultVethGateway = "100.127.0.1"    // router-side bridge address
	defaultBridgeName  = "BR_AWG_MESH"
)

type containerDialect int

const (
	containerDialectLegacy containerDialect = iota
	containerDialectTransitional
	containerDialectCanonical
)

// ContainerConfig holds parameters for generating MikroTik container CLI commands.
type ContainerConfig struct {
	Name             string
	Image            string
	Interface        string
	RootDir          string   // e.g. /docker/awg-mesh-client-home
	MountName        string   // e.g. AWG_MESH_HOME_CONFIG
	MountSrc         string   // e.g. /docker/etc/awg-mesh-client-home-config
	DNS              []string // e.g. ["1.1.1.1", "8.8.8.8"]
	EnvVars          map[string]string
	Command          string
	TargetROSVersion string
}

// ToUpperSnake converts a topology name like "mikrotik-home" to "MIKROTIK_HOME".
func ToUpperSnake(name string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(name), "-", "_"))
}

// DeriveContainerName returns the CAPS convention container name: AWG_MESH_<NAME>.
func DeriveContainerName(topologyName string) string {
	return "AWG_MESH_" + ToUpperSnake(topologyName)
}

// DeriveEnvListName returns the env list name in CAPS convention.
func DeriveEnvListName(containerName string) string {
	if containerName == "" {
		return "AWG_MESH_ENVS"
	}
	return containerName + "_ENVS"
}

// DeriveMountName returns the mount name in CAPS convention.
func DeriveMountName(containerName string) string {
	return containerName + "_CONFIG"
}

// GenerateContainerCommands returns /container/mounts + /container/envs + /container/add CLI commands.
func GenerateContainerCommands(cfg ContainerConfig) []string {
	commands, _ := GenerateContainerCommandsForTarget(cfg)
	return commands
}

// GenerateContainerCommandsForTarget returns RouterOS-version-specific container commands.
func GenerateContainerCommandsForTarget(cfg ContainerConfig) ([]string, error) {
	dialect, err := selectMikrotikDialect(cfg.TargetROSVersion)
	if err != nil {
		return nil, err
	}
	envListName := DeriveEnvListName(cfg.Name)

	var cmds []string

	// Mount for persistent /config
	// per https://help.mikrotik.com/docs/spaces/ROS/pages/84901929/Container
	if cfg.MountName != "" && cfg.MountSrc != "" {
		mountListParam := "list"
		if dialect != containerDialectCanonical {
			mountListParam = "name"
		}
		cmds = append(cmds, fmt.Sprintf(
			"/container/mounts/add %s=%s src=%s dst=/config",
			mountListParam,
			escapeRouterOSToken(cfg.MountName),
			escapeRouterOSToken(cfg.MountSrc),
		))
	}

	// Environment variables
	cmds = append(cmds, buildEnvCommands(envListName, cfg.EnvVars, dialect)...)

	// Container add with production settings
	cmds = append(cmds, buildContainerAddCommand(cfg, envListName, dialect))

	return cmds, nil
}

// GenerateBridgeCommands returns commands to create a bridge, assign the gateway
// IP, and add the veth as a bridge port. Without a bridge + IP the container has
// no L2/L3 connectivity to the router.
func GenerateBridgeCommands(bridgeName, vethName, gateway string) []string {
	if bridgeName == "" {
		bridgeName = defaultBridgeName
	}
	_, normalizedGateway := deriveVethAddressAndGateway(gateway)
	safeBridge := escapeRouterOSToken(bridgeName)
	return []string{
		fmt.Sprintf("/interface/bridge/add name=%s comment=%s",
			safeBridge, quoteRouterOSValue("awg-mesh container bridge")),
		fmt.Sprintf("/ip/address/add address=%s/%d interface=%s comment=%s",
			escapeRouterOSToken(normalizedGateway), defaultVethPrefixBits, safeBridge,
			quoteRouterOSValue("awg-mesh container gateway")),
		fmt.Sprintf("/interface/bridge/port add bridge=%s interface=%s",
			safeBridge, escapeRouterOSToken(vethName)),
	}
}

// GenerateNATCommands returns srcnat masquerade for the container subnet so the
// container can reach the internet, and optionally a dstnat rule for gRPC management.
func GenerateNATCommands(gateway string, grpcPort int) []string {
	_, normalizedGateway := deriveVethAddressAndGateway(gateway)
	subnet := deriveSubnetCIDR(normalizedGateway)
	cmds := []string{
		fmt.Sprintf("/ip/firewall/nat add chain=srcnat action=masquerade src-address=%s comment=%s",
			escapeRouterOSToken(subnet), quoteRouterOSValue("awg-mesh: container masquerade")),
	}
	if grpcPort > 0 {
		vethAddr, _ := deriveVethAddressAndGateway(gateway)
		ip := strings.SplitN(vethAddr, "/", 2)[0]
		cmds = append(cmds, fmt.Sprintf(
			"/ip/firewall/nat add chain=dstnat protocol=tcp dst-port=%d action=dst-nat to-addresses=%s to-ports=%d comment=%s",
			grpcPort, escapeRouterOSToken(ip), grpcPort,
			quoteRouterOSValue("awg-mesh: gRPC management port")))
	}
	return cmds
}

// GenerateVethCommands returns /interface/veth add CLI commands.
func GenerateVethCommands(name, gateway string) []string {
	vethAddress, normalizedGateway := deriveVethAddressAndGateway(gateway)
	return []string{
		fmt.Sprintf("/interface/veth add name=%s address=%s gateway=%s",
			escapeRouterOSToken(name),
			escapeRouterOSToken(vethAddress),
			escapeRouterOSToken(normalizedGateway)),
	}
}

// GenerateRouteCommands returns /ip/route add CLI commands for overlay routing.
func GenerateRouteCommands(overlayNet, gateway string) []string {
	vethAddr, _ := deriveVethAddressAndGateway(gateway)
	ip := strings.SplitN(vethAddr, "/", 2)[0]
	return []string{
		fmt.Sprintf("/ip/route add dst-address=%s gateway=%s comment=%s",
			escapeRouterOSToken(overlayNet),
			escapeRouterOSToken(ip),
			quoteRouterOSValue("awg-mesh: overlay network")),
	}
}

// GenerateFirewallCommands returns conntrack-aware /ip/firewall/filter rules
// anchored before the universal action=fasttrack-connection rule (present on
// every standard RouterOS install). Falls back to chain-end append with a
// warning log when the fasttrack rule is absent (stripped-install or custom
// firewall). RouterOS evaluates the if/else at /import time.
// per https://help.mikrotik.com/docs/spaces/ROS/pages/84901929/Container
func GenerateFirewallCommands(bridgeName string) []string {
	if bridgeName == "" {
		bridgeName = defaultBridgeName
	}
	safeBridge := escapeRouterOSToken(bridgeName)
	established := quoteRouterOSValue("awg-mesh: established return traffic")
	outbound := quoteRouterOSValue("awg-mesh: container outbound")

	block := fmt.Sprintf(
		":local fastTrackId [/ip/firewall/filter find where action=fasttrack-connection chain=forward]\n"+
			":if ([:len $fastTrackId] > 0) do={\n"+
			"    /ip/firewall/filter add chain=forward action=accept connection-state=established,related in-interface=%s comment=%s place-before=$fastTrackId\n"+
			"    /ip/firewall/filter add chain=forward action=accept in-interface=%s comment=%s place-before=$fastTrackId\n"+
			"} else={\n"+
			"    /ip/firewall/filter add chain=forward action=accept connection-state=established,related in-interface=%s comment=%s\n"+
			"    /ip/firewall/filter add chain=forward action=accept in-interface=%s comment=%s\n"+
			"    # WARNING: no fasttrack-connection rule found, appended to chain end\n"+
			"    :log warning \"awg-mesh: no fasttrack-connection rule, appended to chain end\"\n"+
			"}",
		safeBridge, established,
		safeBridge, outbound,
		safeBridge, established,
		safeBridge, outbound,
	)
	return []string{block}
}

func buildEnvCommands(envListName string, envVars map[string]string, dialect containerDialect) []string {
	if len(envVars) == 0 {
		return nil
	}

	keys := make([]string, 0, len(envVars))
	for key := range envVars {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	commands := make([]string, 0, len(keys))
	envListParam := "list"
	if dialect != containerDialectCanonical {
		envListParam = "name"
	}
	for _, key := range keys {
		value := envVars[key]
		// MESH_TOKEN_HASH uses the v2 charset [A-Za-z0-9._-] — no RouterOS-
		// meaningful characters — so quoting is unnecessary and may interfere
		// with template parsing. All other env var values are quoted normally.
		var emittedValue string
		if key == "MESH_TOKEN_HASH" {
			emittedValue = value
		} else {
			emittedValue = quoteRouterOSValue(value)
		}
		commands = append(commands, fmt.Sprintf(
			"/container/envs/add %s=%s key=%s value=%s",
			envListParam,
			escapeRouterOSToken(envListName),
			escapeRouterOSToken(key),
			emittedValue,
		))
	}
	return commands
}

func buildContainerAddCommand(cfg ContainerConfig, envListName string, dialect containerDialect) string {
	rootDir := cfg.RootDir
	if rootDir == "" {
		rootDir = "/docker/awg-mesh-client-" + strings.ToLower(strings.TrimSpace(cfg.Name))
	}

	imageParam := "remote-image"
	if dialect == containerDialectLegacy {
		imageParam = "image"
	}

	// per https://help.mikrotik.com/docs/spaces/ROS/pages/84901929/Container
	parts := []string{
		"/container/add",
		"interface=" + escapeRouterOSToken(cfg.Interface),
		imageParam + "=" + escapeRouterOSToken(cfg.Image),
		"hostname=" + escapeRouterOSToken(strings.ReplaceAll(strings.ToLower(cfg.Name), "_", "-")),
		"root-dir=" + escapeRouterOSToken(rootDir),
		"envlist=" + escapeRouterOSToken(envListName),
		"name=" + escapeRouterOSToken(cfg.Name),
	}

	if cfg.MountName != "" {
		mountRefParam := "mountlists"
		if dialect != containerDialectCanonical {
			mountRefParam = "mounts"
		}
		parts = append(parts, mountRefParam+"="+escapeRouterOSToken(cfg.MountName))
	}

	if len(cfg.DNS) > 0 {
		parts = append(parts, "dns="+escapeRouterOSToken(strings.Join(cfg.DNS, ",")))
	}

	if strings.TrimSpace(cfg.Command) != "" {
		parts = append(parts, "cmd="+quoteRouterOSValue(strings.TrimSpace(cfg.Command)))
	}

	parts = append(parts,
		"logging=yes",
		"start-on-boot=yes",
	)

	return strings.Join(parts, " ")
}

func selectMikrotikDialect(version string) (containerDialect, error) {
	trimmed := strings.TrimSpace(version)
	if trimmed == "" {
		return containerDialectCanonical, nil
	}

	parts := strings.Split(trimmed, ".")
	if len(parts) < 2 {
		return containerDialectCanonical, fmt.Errorf("target RouterOS version %q must include major.minor", version)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return containerDialectCanonical, fmt.Errorf("target RouterOS version %q has invalid major version: %w", version, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return containerDialectCanonical, fmt.Errorf("target RouterOS version %q has invalid minor version: %w", version, err)
	}
	if major != 7 {
		return containerDialectCanonical, fmt.Errorf("target RouterOS version %q is unsupported: RouterOS 7.5+ is required", version)
	}
	if minor < 5 {
		return containerDialectCanonical, fmt.Errorf("target RouterOS version %q is unsupported: /container requires RouterOS 7.5+", version)
	}
	if minor == 22 {
		return containerDialectCanonical, fmt.Errorf("target RouterOS version %q is unsupported: RouterOS 7.22 has a known container routing regression; use 7.21 LTS or 7.23+", version)
	}
	if minor <= 17 {
		return containerDialectLegacy, nil
	}
	if minor <= 20 {
		return containerDialectTransitional, nil
	}
	return containerDialectCanonical, nil
}

// deriveSubnetCIDR converts a gateway IP like "100.127.0.1" to its /24 subnet "100.127.0.0/24".
func deriveSubnetCIDR(gateway string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(gateway))
	if err != nil {
		return defaultVethSubnet
	}
	prefix, err := addr.Prefix(defaultVethPrefixBits)
	if err != nil {
		return defaultVethSubnet
	}
	return prefix.String()
}

func deriveVethAddressAndGateway(gateway string) (string, string) {
	trimmedGateway := strings.TrimSpace(gateway)
	if trimmedGateway == "" {
		return defaultVethAddress, defaultVethGateway
	}

	// CIDR input: operator specifies "gateway/prefix" — the gateway is the
	// router-side bridge address, the container gets the next address.
	if prefix, err := netip.ParsePrefix(trimmedGateway); err == nil {
		prefixBits := prefix.Bits()
		gatewayAddr := prefix.Addr()
		containerAddr := gatewayAddr.Next()
		if !containerAddr.IsValid() {
			containerAddr = gatewayAddr
		}
		return containerAddr.String() + "/" + strconv.Itoa(prefixBits), gatewayAddr.String()
	}

	addr, err := netip.ParseAddr(trimmedGateway)
	if err != nil {
		if strings.Contains(trimmedGateway, "/") {
			parts := strings.SplitN(trimmedGateway, "/", 2)
			return trimmedGateway, strings.TrimSpace(parts[0])
		}
		return trimmedGateway + "/24", trimmedGateway
	}

	if !addr.Is4() {
		return addr.String() + "/64", addr.String()
	}

	gatewayBytes := addr.As4()
	localBytes := gatewayBytes
	if localBytes[3] > 1 {
		localBytes[3]--
	} else {
		localBytes[3] = 1
	}

	localAddr := netip.AddrFrom4(localBytes)
	return localAddr.String() + "/" + strconv.Itoa(defaultVethPrefixBits), addr.String()
}

func escapeRouterOSToken(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "\"\""
	}

	if strings.ContainsAny(trimmed, " \t\"'") {
		return quoteRouterOSValue(trimmed)
	}
	return trimmed
}

func quoteRouterOSValue(value string) string {
	escaped := strings.ReplaceAll(value, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	return "\"" + escaped + "\""
}
