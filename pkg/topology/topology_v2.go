package topology

import (
	"errors"
	"fmt"
	"net"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
)

// SchemaVersion identifies the topology.yml schema generation.
type SchemaVersion int

const (
	// SchemaV1 is the v1.x schema with masters/endpoints/clients + transport pool.
	// Detected by absence of schema_version key OR presence of transport.pool.
	SchemaV1 SchemaVersion = 1

	// SchemaV2 is the v2.0 flat-mesh schema with role-tagged nodes + ingresses.
	// Detected by schema_version: 2.
	SchemaV2 SchemaVersion = 2
)

// V2 errors.
var (
	ErrV2SchemaMissing      = errors.New("topology: schema_version: 2 required for v2.0 topology")
	ErrV2OverlayMissing     = errors.New("topology: mesh.overlay_supernet is required")
	ErrV2OverlayInvalid     = errors.New("topology: mesh.overlay_supernet must be a valid CIDR")
	ErrV2NoNodes            = errors.New("topology: at least one node required")
	ErrV2NodeMissingName    = errors.New("topology: node missing name")
	ErrV2NodeMissingRoles   = errors.New("topology: node missing roles")
	ErrV2NodeMissingOverlay = errors.New("topology: node missing overlay_ip")
	ErrV2OverlayDuplicate   = errors.New("topology: duplicate overlay_ip across nodes")
	ErrV2OverlayOutOfRange  = errors.New("topology: node overlay_ip not in mesh.overlay_supernet")
	ErrSchemaV1Deprecated   = errors.New("SCHEMA-V1-DEPRECATED: this is a v1.x topology file. Run 'mesh-ctl migrate' to convert (CR-013).")
)

// TopologyV2 is the v2.0 flat-mesh schema.
//
// Distinct from the v1.x Topology struct (which is preserved for migrate.go to
// parse legacy files). Both types live side-by-side during the v1.x→v2.0
// migration window. See plan F-009 §Data Model for full field semantics.
type TopologyV2 struct {
	SchemaVersion SchemaVersion       `yaml:"schema_version"`
	Mesh          MeshConfig          `yaml:"mesh"`
	Nodes         []NodeV2            `yaml:"nodes"`
	Services      []ServiceV2         `yaml:"services,omitempty"`
	Rotation      RotationConfig      `yaml:"rotation,omitempty"`
	Capture       CaptureConfig       `yaml:"capture,omitempty"`
	Observability ObservabilityConfig `yaml:"observability,omitempty"`
}

// MeshConfig is mesh-wide configuration.
type MeshConfig struct {
	Name            string   `yaml:"name"`
	OverlaySupernet string   `yaml:"overlay_supernet"` // e.g. "172.21.92.0/24"
	Tenants         []string `yaml:"tenants,omitempty"`
}

// NodeV2 is a single mesh node with role flags.
type NodeV2 struct {
	Name            string      `yaml:"name"`
	Roles           []role.Role `yaml:"roles"`
	OverlayIP       string      `yaml:"overlay_ip"`
	BridgeIP        string      `yaml:"bridge_ip,omitempty"`
	PublicIP        string      `yaml:"public_ip,omitempty"` // ingress only
	Region          string      `yaml:"region,omitempty"`
	PreferredMaster string      `yaml:"preferred_master,omitempty"` // clients (HA-2 metric hint)
	InternetIface   string      `yaml:"internet_iface,omitempty"`   // egress only
	ClientProtocol  string      `yaml:"client_protocol,omitempty"`  // "vanilla-wg" | "amneziawg"
	MeshProtocol    string      `yaml:"mesh_protocol,omitempty"`    // "amneziawg"
}

// ServiceV2 declares an exposed application reachable via ingress.
type ServiceV2 struct {
	Name      string             `yaml:"name"`
	OwnerNode string             `yaml:"owner_node"` // overlay address resolved from this node
	Protocol  string             `yaml:"protocol"`   // "tcp" | "udp"
	LocalPort int                `yaml:"local_port"`
	Ingress   []IngressBindingV2 `yaml:"ingress,omitempty"`
	Tenant    string             `yaml:"tenant,omitempty"`
}

// IngressBindingV2 binds a service to an ingress node hostname/port.
type IngressBindingV2 struct {
	Hostname    string `yaml:"hostname"`
	Mode        string `yaml:"mode"` // "sni_passthrough" | "tls_terminate" | "tcp_forward" | "udp_forward"
	IngressNode string `yaml:"ingress_node"`
	Port        int    `yaml:"port,omitempty"`
	HTTP3       bool   `yaml:"http3,omitempty"`
}

// ObservabilityConfig holds Prometheus + audit-log + log-level settings.
type ObservabilityConfig struct {
	AuditRetentionDays int    `yaml:"audit_retention_days,omitempty"` // default 90
	CertRotationDays   int    `yaml:"cert_rotation_days,omitempty"`   // default 90
	PrometheusListen   string `yaml:"prometheus_listen,omitempty"`    // ":9091"
	LogLevel           string `yaml:"log_level,omitempty"`            // "info" | "debug" | etc
}

// ValidateV2 checks the v2.0 topology for structural and semantic correctness.
//
// Checks performed (anti-stub: each consults its corresponding field):
//   - schema_version == 2
//   - mesh.overlay_supernet is non-empty and parseable as CIDR
//   - at least one node declared
//   - each node has name + non-empty roles + overlay_ip
//   - role composability per pkg/role (client exclusive)
//   - no two nodes share the same overlay_ip
//   - every node's overlay_ip belongs to mesh.overlay_supernet
func ValidateV2(t *TopologyV2) error {
	if t == nil {
		return errors.New("topology: nil topology")
	}
	if t.SchemaVersion != SchemaV2 {
		return ErrV2SchemaMissing
	}
	if t.Mesh.OverlaySupernet == "" {
		return ErrV2OverlayMissing
	}
	_, supernet, err := net.ParseCIDR(t.Mesh.OverlaySupernet)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrV2OverlayInvalid, t.Mesh.OverlaySupernet, err)
	}
	if len(t.Nodes) == 0 {
		return ErrV2NoNodes
	}

	seenOverlay := make(map[string]string, len(t.Nodes)) // overlay_ip → node name (first owner)
	for i := range t.Nodes {
		n := &t.Nodes[i]
		if n.Name == "" {
			return fmt.Errorf("%w (index %d)", ErrV2NodeMissingName, i)
		}
		if len(n.Roles) == 0 {
			return fmt.Errorf("%w (node %q)", ErrV2NodeMissingRoles, n.Name)
		}
		if err := role.ValidateComposability(n.Roles); err != nil {
			return fmt.Errorf("topology: node %q role composability: %w", n.Name, err)
		}
		if n.OverlayIP == "" {
			return fmt.Errorf("%w (node %q)", ErrV2NodeMissingOverlay, n.Name)
		}
		ip := net.ParseIP(n.OverlayIP)
		if ip == nil {
			return fmt.Errorf("topology: node %q overlay_ip %q is not a valid IP", n.Name, n.OverlayIP)
		}
		if !supernet.Contains(ip) {
			return fmt.Errorf("%w: node %q overlay_ip %s not in %s",
				ErrV2OverlayOutOfRange, n.Name, n.OverlayIP, t.Mesh.OverlaySupernet)
		}
		if prev, dup := seenOverlay[n.OverlayIP]; dup {
			return fmt.Errorf("%w: nodes %q and %q both claim %s",
				ErrV2OverlayDuplicate, prev, n.Name, n.OverlayIP)
		}
		seenOverlay[n.OverlayIP] = n.Name
	}

	return nil
}
