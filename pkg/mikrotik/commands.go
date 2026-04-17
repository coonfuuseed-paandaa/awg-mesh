package mikrotik

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

const defaultVethPrefixBits = 24

// ContainerConfig holds parameters for generating MikroTik container CLI commands.
type ContainerConfig struct {
	Name        string
	Image       string
	Interface   string
	VethGateway string
	OverlayIP   string
	ListenPort  int
	EnvVars     map[string]string
}

// GenerateContainerCommands returns /container add CLI commands.
func GenerateContainerCommands(cfg ContainerConfig) []string {
	envListName := buildEnvListName(cfg.Name)
	envCommands := buildEnvCommands(envListName, cfg.EnvVars)
	addCommand := buildContainerAddCommand(cfg, envListName)

	return append(envCommands, addCommand)
}

// GenerateVethCommands returns /interface/veth add CLI commands.
func GenerateVethCommands(name, gateway string) []string {
	vethAddress, normalizedGateway := deriveVethAddressAndGateway(gateway)
	command := fmt.Sprintf(
		"/interface/veth add name=%s address=%s gateway=%s",
		escapeRouterOSToken(name),
		escapeRouterOSToken(vethAddress),
		escapeRouterOSToken(normalizedGateway),
	)
	return []string{command}
}

// GenerateRouteCommands returns /ip/route add CLI commands for overlay routing.
func GenerateRouteCommands(overlayNet, gateway string) []string {
	command := fmt.Sprintf(
		"/ip/route add dst-address=%s gateway=%s",
		escapeRouterOSToken(overlayNet),
		escapeRouterOSToken(gateway),
	)
	return []string{command}
}

// GenerateFirewallCommands returns /ip/firewall/filter add CLI commands for the veth interface.
func GenerateFirewallCommands(iface string) []string {
	safeInterface := escapeRouterOSToken(iface)
	return []string{
		fmt.Sprintf("/ip/firewall/filter add chain=forward in-interface=%s action=accept", safeInterface),
		fmt.Sprintf("/ip/firewall/filter add chain=forward out-interface=%s action=accept", safeInterface),
	}
}

func buildEnvListName(containerName string) string {
	trimmedName := strings.TrimSpace(containerName)
	if trimmedName == "" {
		return "awg-mesh-envs"
	}
	return trimmedName + "-envs"
}

func buildEnvCommands(envListName string, envVars map[string]string) []string {
	if len(envVars) == 0 {
		return []string{}
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
		// supported release (see README and ADR-0002), so we emit `list=`
		// here. Operators on 7.20 or earlier would need to downgrade the
		// parameter manually — that's out of support scope.
		command := fmt.Sprintf(
			"/container/envs/add list=%s key=%s value=%s",
			escapeRouterOSToken(envListName),
			escapeRouterOSToken(key),
			quoteRouterOSValue(value),
		)
		commands = append(commands, command)
	}

	return commands
}

func buildContainerAddCommand(cfg ContainerConfig, envListName string) string {
	return fmt.Sprintf(
		"/container/add interface=%s image=%s hostname=%s root-dir=%s envlist=%s name=%s",
		escapeRouterOSToken(cfg.Interface),
		escapeRouterOSToken(cfg.Image),
		escapeRouterOSToken(cfg.Name),
		escapeRouterOSToken("/data/"+cfg.Name),
		escapeRouterOSToken(envListName),
		escapeRouterOSToken(cfg.Name),
	)
}

func deriveVethAddressAndGateway(gateway string) (string, string) {
	trimmedGateway := strings.TrimSpace(gateway)
	if trimmedGateway == "" {
		return "192.168.100.1/24", "192.168.100.2"
	}

	if prefix, err := netip.ParsePrefix(trimmedGateway); err == nil {
		prefixBits := prefix.Bits()
		prefixAddr := prefix.Addr()
		nextAddr := prefixAddr.Next()
		if nextAddr.IsValid() {
			return prefixAddr.String() + "/" + strconv.Itoa(prefixBits), nextAddr.String()
		}
		return prefixAddr.String() + "/" + strconv.Itoa(prefixBits), prefixAddr.String()
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
