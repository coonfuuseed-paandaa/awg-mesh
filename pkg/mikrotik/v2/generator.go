package v2

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"text/template"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

const (
	DefaultInterfaceName     = "awg-mesh"
	DefaultClientListenPort  = 13231
	defaultKeepaliveInterval = "25s"
)

type Peer struct {
	Name                string
	PublicKey           string
	EndpointAddress     string
	EndpointPort        int
	AllowedIPs          []string
	PersistentKeepalive string
	Comment             string
}

type StaticScript struct {
	MeshName         string
	ClientName       string
	InterfaceName    string
	ListenPort       int
	PrivateKey       string
	ClientAddress    string
	InterfaceComment string
	AddressComment   string
	Peers            []Peer
}

func GenerateStaticRSC(topo *topology.TopologyV2, clientName string, clientPrivateKey wg.Key, masterPublicKeys map[string]wg.Key) (string, error) {
	script, err := BuildStaticScript(topo, clientName, clientPrivateKey, masterPublicKeys)
	if err != nil {
		return "", err
	}
	return renderStaticScript(script)
}

func BuildStaticScript(topo *topology.TopologyV2, clientName string, clientPrivateKey wg.Key, masterPublicKeys map[string]wg.Key) (StaticScript, error) {
	if topo == nil {
		return StaticScript{}, fmt.Errorf("topology is required")
	}
	if err := topology.ValidateV2(topo); err != nil {
		return StaticScript{}, err
	}
	if clientPrivateKey.IsZero() {
		return StaticScript{}, fmt.Errorf("client private key is required")
	}
	client, err := findClientNode(topo, clientName)
	if err != nil {
		return StaticScript{}, err
	}
	if platform := strings.TrimSpace(client.Platform); platform != "" && !strings.EqualFold(platform, "mikrotik") {
		return StaticScript{}, fmt.Errorf("node %q platform %q is not mikrotik", client.Name, client.Platform)
	}
	clientAddress, err := nodeOverlayCIDR(client)
	if err != nil {
		return StaticScript{}, err
	}
	masters, err := collectMasterNodes(topo)
	if err != nil {
		return StaticScript{}, err
	}
	peers, err := buildMasterPeers(topo, client, masters, masterPublicKeys)
	if err != nil {
		return StaticScript{}, err
	}

	interfaceName := DefaultInterfaceName
	return StaticScript{
		MeshName:         strings.TrimSpace(topo.Mesh.Name),
		ClientName:       client.Name,
		InterfaceName:    interfaceName,
		ListenPort:       DefaultClientListenPort,
		PrivateKey:       clientPrivateKey.String(),
		ClientAddress:    clientAddress,
		InterfaceComment: "awg-mesh: " + client.Name + " native WireGuard",
		AddressComment:   "awg-mesh: " + client.Name + " overlay address",
		Peers:            peers,
	}, nil
}

func findClientNode(topo *topology.TopologyV2, clientName string) (topology.NodeV2, error) {
	trimmed := strings.TrimSpace(clientName)
	if trimmed == "" {
		return topology.NodeV2{}, fmt.Errorf("client name is required")
	}
	for _, node := range topo.Nodes {
		if node.Name != trimmed {
			continue
		}
		if !hasRole(node, role.RoleClient) {
			return topology.NodeV2{}, fmt.Errorf("node %q is not a client", trimmed)
		}
		return node, nil
	}
	return topology.NodeV2{}, fmt.Errorf("client node %q is not declared", trimmed)
}

func collectMasterNodes(topo *topology.TopologyV2) ([]topology.NodeV2, error) {
	masters := make([]topology.NodeV2, 0)
	for _, node := range topo.Nodes {
		if !hasRole(node, role.RoleMaster) {
			continue
		}
		protocol := strings.TrimSpace(node.ClientProtocol)
		if protocol != "" && protocol != string(wg.ProtocolVanilla) {
			return nil, fmt.Errorf("master %q client_protocol must be %q for mikrotik native mode, got %q", node.Name, wg.ProtocolVanilla, node.ClientProtocol)
		}
		if _, err := masterEndpointAddress(node); err != nil {
			return nil, err
		}
		masters = append(masters, node)
	}
	if len(masters) == 0 {
		return nil, fmt.Errorf("at least one master node is required")
	}
	sort.Slice(masters, func(i, j int) bool { return masters[i].Name < masters[j].Name })
	return masters, nil
}

func buildMasterPeers(topo *topology.TopologyV2, client topology.NodeV2, masters []topology.NodeV2, masterPublicKeys map[string]wg.Key) ([]Peer, error) {
	peerBuilders := make([]peerBuilder, 0, len(masters))
	for _, master := range masters {
		publicKey, ok := masterPublicKeys[master.Name]
		if !ok || publicKey.IsZero() {
			return nil, fmt.Errorf("master %q public key is required", master.Name)
		}
		endpoint, err := masterEndpointAddress(master)
		if err != nil {
			return nil, err
		}
		allowed, err := nodeOverlayCIDR(master)
		if err != nil {
			return nil, err
		}
		builder := peerBuilder{
			peer: Peer{
				Name:                master.Name,
				PublicKey:           publicKey.String(),
				EndpointAddress:     endpoint,
				EndpointPort:        wg.DefaultClientListenPort,
				PersistentKeepalive: defaultKeepaliveInterval,
				Comment:             "awg-mesh: " + master.Name + " client-facing peer",
			},
			seenAllowedIPs: make(map[string]struct{}, len(topo.Nodes)),
		}
		builder.addAllowedIP(allowed)
		peerBuilders = append(peerBuilders, builder)
	}

	routeTargets := make([]routeTarget, 0, len(topo.Nodes))
	for _, node := range topo.Nodes {
		if node.Name == client.Name || hasRole(node, role.RoleMaster) {
			continue
		}
		cidr, err := nodeOverlayCIDR(node)
		if err != nil {
			return nil, err
		}
		routeTargets = append(routeTargets, routeTarget{Name: node.Name, CIDR: cidr})
	}
	sort.Slice(routeTargets, func(i, j int) bool { return routeTargets[i].Name < routeTargets[j].Name })
	for i, target := range routeTargets {
		peerBuilders[i%len(peerBuilders)].addAllowedIP(target.CIDR)
	}

	peers := make([]Peer, 0, len(peerBuilders))
	for _, builder := range peerBuilders {
		sort.Strings(builder.peer.AllowedIPs)
		peers = append(peers, builder.peer)
	}
	return peers, nil
}

type peerBuilder struct {
	peer           Peer
	seenAllowedIPs map[string]struct{}
}

func (b *peerBuilder) addAllowedIP(cidr string) {
	if _, seen := b.seenAllowedIPs[cidr]; seen {
		return
	}
	b.seenAllowedIPs[cidr] = struct{}{}
	b.peer.AllowedIPs = append(b.peer.AllowedIPs, cidr)
}

type routeTarget struct {
	Name string
	CIDR string
}

func hasRole(node topology.NodeV2, want role.Role) bool {
	for _, current := range node.Roles {
		if current == want {
			return true
		}
	}
	return false
}

func nodeOverlayCIDR(node topology.NodeV2) (string, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(node.OverlayIP))
	if err != nil {
		return "", fmt.Errorf("node %q overlay_ip %q is invalid: %w", node.Name, node.OverlayIP, err)
	}
	bits := 32
	if addr.Is6() {
		bits = 128
	}
	prefix, err := addr.Prefix(bits)
	if err != nil {
		return "", fmt.Errorf("node %q overlay_ip %q cannot be converted to prefix: %w", node.Name, node.OverlayIP, err)
	}
	return prefix.String(), nil
}

func masterEndpointAddress(master topology.NodeV2) (string, error) {
	for _, candidate := range []string{master.PublicIP, master.BridgeIP} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed, nil
		}
	}
	return "", fmt.Errorf("master %q requires public_ip or bridge_ip for mikrotik endpoint-address", master.Name)
}

func renderStaticScript(script StaticScript) (string, error) {
	tmpl, err := template.New("mikrotik-v2-static").Funcs(template.FuncMap{
		"quote":   quoteRouterOSValue,
		"ros":     escapeRouterOSToken,
		"rosList": escapeRouterOSList,
	}).Parse(staticScriptTemplate)
	if err != nil {
		return "", fmt.Errorf("parse mikrotik v2 template: %w", err)
	}
	var out strings.Builder
	if err := tmpl.Execute(&out, script); err != nil {
		return "", fmt.Errorf("render mikrotik v2 script: %w", err)
	}
	return out.String(), nil
}

func escapeRouterOSList(values []string) string {
	return escapeRouterOSToken(strings.Join(values, ","))
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
