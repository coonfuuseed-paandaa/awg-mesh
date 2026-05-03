package topology

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
	"gopkg.in/yaml.v3"
)

// V1ToV2MigrationResult contains the converted topology and operator-facing notes.
type V1ToV2MigrationResult struct {
	Topology *TopologyV2
	Warnings []string
}

// MigrateV1ToV2 converts a v1.x topology document to a v2.0 TopologyV2 struct.
func MigrateV1ToV2(in []byte) (*TopologyV2, error) {
	result, err := MigrateV1ToV2WithReport(in)
	if err != nil {
		return nil, err
	}
	return result.Topology, nil
}

// MigrateV1ToV2WithReport converts v1.x YAML and returns conversion warnings.
func MigrateV1ToV2WithReport(in []byte) (V1ToV2MigrationResult, error) {
	version, err := DetectSchemaVersion(in)
	if err != nil {
		return V1ToV2MigrationResult{}, err
	}
	if version == SchemaV2 {
		return V1ToV2MigrationResult{}, errors.New("topology: input is already schema_version 2")
	}
	if version != SchemaV1 {
		return V1ToV2MigrationResult{}, fmt.Errorf("topology: expected v1.x schema, got %d", version)
	}

	var legacy Topology
	if err := yaml.Unmarshal(in, &legacy); err != nil {
		return V1ToV2MigrationResult{}, fmt.Errorf("topology: unmarshal v1 topology: %w", err)
	}

	result := V1ToV2MigrationResult{
		Topology: &TopologyV2{
			SchemaVersion: SchemaV2,
			Mesh: MeshConfig{
				Name:            "migrated-mesh",
				OverlaySupernet: migratedOverlaySupernet(legacy.Overlay),
			},
			Capture:  legacy.Capture,
			Rotation: legacy.Rotation,
		},
	}
	if fields := legacyTopologyDroppedFields(legacy); len(fields) > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("topology-wide v1-only fields were dropped: %s", strings.Join(fields, ", ")))
	}

	seenNames := make(map[string]struct{}, len(legacy.Masters)+len(legacy.Endpoints)+len(legacy.Clients))
	for _, master := range legacy.Masters {
		if fields := legacyMasterDroppedFields(master); len(fields) > 0 {
			result.Warnings = append(result.Warnings, fmt.Sprintf("master %q dropped v1-only fields: %s", master.Name, strings.Join(fields, ", ")))
		}
		roles := []role.Role{role.RoleMaster, role.RoleBalancer}
		if master.Exit {
			roles = append(roles, role.RoleEgress)
			result.Warnings = append(result.Warnings, fmt.Sprintf("master %q has exit=true and was mapped to roles [master balancer egress]", master.Name))
		} else {
			result.Warnings = append(result.Warnings, fmt.Sprintf("master %q was mapped to roles [master balancer]", master.Name))
		}
		if err := appendMigratedNode(result.Topology, seenNames, NodeV2{
			Name:           strings.TrimSpace(master.Name),
			Roles:          roles,
			OverlayIP:      strings.TrimSpace(master.OverlayIP),
			MeshProtocol:   "amneziawg",
			ClientProtocol: "vanilla-wg",
		}); err != nil {
			return V1ToV2MigrationResult{}, err
		}
	}

	for _, endpoint := range legacy.Endpoints {
		if fields := legacyEndpointDroppedFields(endpoint); len(fields) > 0 {
			result.Warnings = append(result.Warnings, fmt.Sprintf("endpoint %q dropped v1-only fields: %s", endpoint.Name, strings.Join(fields, ", ")))
		}
		result.Warnings = append(result.Warnings, fmt.Sprintf("endpoint %q was mapped to role [egress]", endpoint.Name))
		if err := appendMigratedNode(result.Topology, seenNames, NodeV2{
			Name:         strings.TrimSpace(endpoint.Name),
			Roles:        []role.Role{role.RoleEgress},
			OverlayIP:    strings.TrimSpace(endpoint.OverlayIP),
			Region:       strings.TrimSpace(endpoint.Region),
			MeshProtocol: "amneziawg",
		}); err != nil {
			return V1ToV2MigrationResult{}, err
		}
	}

	for _, client := range legacy.Clients {
		if fields := legacyClientDroppedFields(client); len(fields) > 0 {
			result.Warnings = append(result.Warnings, fmt.Sprintf("client %q dropped v1-only fields: %s", client.Name, strings.Join(fields, ", ")))
		}
		preferredMaster := ""
		if len(client.Masters) > 0 {
			preferredMaster = strings.TrimSpace(client.Masters[0])
		}
		platform := strings.TrimSpace(client.Type)
		result.Warnings = append(result.Warnings, fmt.Sprintf("client %q was mapped to role [client]", client.Name))
		if err := appendMigratedNode(result.Topology, seenNames, NodeV2{
			Name:            strings.TrimSpace(client.Name),
			Roles:           []role.Role{role.RoleClient},
			OverlayIP:       strings.TrimSpace(client.OverlayIP),
			Platform:        platform,
			PreferredMaster: preferredMaster,
			ClientProtocol:  "vanilla-wg",
		}); err != nil {
			return V1ToV2MigrationResult{}, err
		}
	}

	if err := ValidateV2(result.Topology); err != nil {
		return V1ToV2MigrationResult{}, fmt.Errorf("topology: migrated v2 topology invalid: %w", err)
	}
	return result, nil
}

// SaveTopologyV2 marshals t to YAML and writes it atomically to path.
func SaveTopologyV2(path string, t *TopologyV2) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("topology path is required")
	}
	if t == nil {
		return fmt.Errorf("topology value is required")
	}

	data, err := yaml.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal v2 topology yaml: %w", err)
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tempFile, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary topology file: %w", err)
	}

	tempPath := tempFile.Name()
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if _, err := tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("write temporary topology file: %w", err)
	}
	if err := tempFile.Chmod(0644); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("set temporary topology file permissions: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temporary topology file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace topology file: %w", err)
	}

	cleanupTemp = false
	return nil
}

func migratedOverlaySupernet(overlay OverlayConfig) string {
	if trimmed := strings.TrimSpace(overlay.Space); trimmed != "" {
		return trimmed
	}
	if len(overlay.Ranges) > 0 {
		return strings.TrimSpace(overlay.Ranges[0].CIDR)
	}
	return ""
}

func legacyTopologyDroppedFields(legacy Topology) []string {
	fields := make([]string, 0, 4)
	if strings.TrimSpace(legacy.Defaults.Image.Node) != "" {
		fields = append(fields, "defaults.image.node")
	}
	if strings.TrimSpace(legacy.Defaults.Image.Client) != "" {
		fields = append(fields, "defaults.image.client")
	}
	if legacy.Overlay.PhysicalMTU != 0 {
		fields = append(fields, "overlay.physical_mtu")
	}
	if legacy.Overlay.AWGOverhead != 0 {
		fields = append(fields, "overlay.awg_overhead")
	}
	if len(legacy.Overlay.Ranges) > 0 {
		fields = append(fields, "overlay.ranges")
	}
	return fields
}

func legacyMasterDroppedFields(master MasterNode) []string {
	fields := make([]string, 0, 5)
	if strings.TrimSpace(master.Host) != "" {
		fields = append(fields, "host")
	}
	if strings.TrimSpace(master.PeerHost) != "" {
		fields = append(fields, "peer_host")
	}
	if master.ListenPort != 0 {
		fields = append(fields, "listen_port")
	}
	if master.GRPCPort != 0 {
		fields = append(fields, "grpc_port")
	}
	if len(master.Endpoints) > 0 {
		fields = append(fields, "endpoints")
	}
	return fields
}

func legacyEndpointDroppedFields(endpoint EndpointNode) []string {
	fields := make([]string, 0, 4)
	if strings.TrimSpace(endpoint.Host) != "" {
		fields = append(fields, "host")
	}
	if strings.TrimSpace(endpoint.PeerHost) != "" {
		fields = append(fields, "peer_host")
	}
	if endpoint.ListenPort != 0 {
		fields = append(fields, "listen_port")
	}
	if endpoint.GRPCPort != 0 {
		fields = append(fields, "grpc_port")
	}
	return fields
}

func legacyClientDroppedFields(client ClientNode) []string {
	fields := make([]string, 0, 7)
	if strings.TrimSpace(client.Host) != "" {
		fields = append(fields, "host")
	}
	if client.GRPCPort != 0 {
		fields = append(fields, "grpc_port")
	}
	if client.Veth != nil {
		fields = append(fields, "veth")
	}
	if client.Mikrotik != nil {
		fields = append(fields, "mikrotik")
	}
	if len(client.Masters) > 1 {
		fields = append(fields, "masters[1:]")
	}
	if len(client.RoutingPolicies) > 0 {
		fields = append(fields, "routing_policies")
	}
	if client.DNS != nil {
		fields = append(fields, "dns")
	}
	return fields
}

func appendMigratedNode(topo *TopologyV2, seenNames map[string]struct{}, node NodeV2) error {
	if node.Name == "" {
		return errors.New("topology: legacy node missing name")
	}
	if _, exists := seenNames[node.Name]; exists {
		return fmt.Errorf("topology: duplicate legacy node name %q", node.Name)
	}
	seenNames[node.Name] = struct{}{}
	topo.Nodes = append(topo.Nodes, node)
	return nil
}
