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

// ContainerConfig holds parameters for generating MikroTik container CLI commands.
type ContainerConfig struct {
	Name      string
	Image     string
	Interface string
	RootDir   string            // e.g. /docker/awg-mesh-client-home
	MountName string            // e.g. AWG_MESH_HOME_CONFIG
	MountSrc  string            // e.g. /docker/etc/awg-mesh-client-home-config
	DNS       []string          // e.g. ["1.1.1.1", "8.8.8.8"]
	EnvVars   map[string]string
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
	envListName := DeriveEnvListName(cfg.Name)

	var cmds []string

	// Mount for persistent /config
	if cfg.MountName != "" && cfg.MountSrc != "" {
		cmds = append(cmds, fmt.Sprintf(
			"/container/mounts/add name=%s src=%s dst=/config",
			escapeRouterOSToken(cfg.MountName),
			escapeRouterOSToken(cfg.MountSrc),
		))
	}

	// Environment variables
	cmds = append(cmds, buildEnvCommands(envListName, cfg.EnvVars)...)

	// Container add with production settings
	cmds = append(cmds, buildContainerAddCommand(cfg, envListName))

	return cmds
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
// placed BEFORE defconf drop rules so they actually match on real routers.
func GenerateFirewallCommands(bridgeName string) []string {
	if bridgeName == "" {
		bridgeName = defaultBridgeName
	}
	safeBridge := escapeRouterOSToken(bridgeName)
	placeBefore := `place-before=[find where comment~"defconf: drop" chain=forward]`
	return []string{
		fmt.Sprintf("/ip/firewall/filter add chain=forward action=accept connection-state=established,related in-interface=%s comment=%s %s",
			safeBridge, quoteRouterOSValue("awg-mesh: established return traffic"), placeBefore),
		fmt.Sprintf("/ip/firewall/filter add chain=forward action=accept in-interface=%s comment=%s %s",
			safeBridge, quoteRouterOSValue("awg-mesh: container outbound"), placeBefore),
	}
}

func buildEnvCommands(envListName string, envVars map[string]string) []string {
	if len(envVars) == 0 {
		return nil
	}

	keys := make([]string, 0, len(envVars))
	for key := range envVars {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	commands := make([]string, 0, len(keys))
	for _, key := range keys {
		value := envVars[key]
		// RouterOS 7.21+ renamed the /container/envs/add parameter from
		// `name=` to `list=`. The project documents 7.21 as the minimum
		// supported release, so we emit `list=` here.
		commands = append(commands, fmt.Sprintf(
			"/container/envs/add list=%s key=%s value=%s",
			escapeRouterOSToken(envListName),
			escapeRouterOSToken(key),
			quoteRouterOSValue(value),
		))
	}
	return commands
}

func buildContainerAddCommand(cfg ContainerConfig, envListName string) string {
	rootDir := cfg.RootDir
	if rootDir == "" {
		rootDir = "/docker/awg-mesh-client-" + strings.ToLower(strings.TrimSpace(cfg.Name))
	}

	parts := []string{
		"/container/add",
		"interface=" + escapeRouterOSToken(cfg.Interface),
		"image=" + escapeRouterOSToken(cfg.Image),
		"hostname=" + escapeRouterOSToken(strings.ToLower(cfg.Name)),
		"root-dir=" + escapeRouterOSToken(rootDir),
		"envlist=" + escapeRouterOSToken(envListName),
		"name=" + escapeRouterOSToken(cfg.Name),
	}

	if cfg.MountName != "" {
		parts = append(parts, "mounts="+escapeRouterOSToken(cfg.MountName))
	}

	if len(cfg.DNS) > 0 {
		parts = append(parts, "dns="+escapeRouterOSToken(strings.Join(cfg.DNS, ",")))
	}

	parts = append(parts,
		"logging=yes",
		"start-on-boot=yes",
	)

	return strings.Join(parts, " ")
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
