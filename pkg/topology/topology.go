package topology

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ImageDefaults holds default Docker image references for node roles.
// Zero value means "unset" — callers fall back to their built-in defaults.
type ImageDefaults struct {
	Node   string `yaml:"node,omitempty"`
	Client string `yaml:"client,omitempty"`
}

// Defaults holds optional topology-wide default values.
type Defaults struct {
	Image ImageDefaults `yaml:"image,omitempty"`
}

// Topology is the full mesh topology configuration.
type Topology struct {
	Defaults  Defaults       `yaml:"defaults,omitempty"`
	Overlay   OverlayConfig  `yaml:"overlay"`
	Masters   []MasterNode   `yaml:"masters"`
	Endpoints []EndpointNode `yaml:"endpoints"`
	Clients   []ClientNode   `yaml:"clients"`
	Capture   CaptureConfig  `yaml:"capture"`
	Rotation  RotationConfig `yaml:"rotation"`
}

// OverlayConfig contains overlay network settings.
type OverlayConfig struct {
	Space       string       `yaml:"space"`
	PhysicalMTU int          `yaml:"physical_mtu"`
	AWGOverhead int          `yaml:"awg_overhead"`
	Ranges      []NamedRange `yaml:"ranges"`
}

// NamedRange describes a named address range in the overlay.
type NamedRange struct {
	Name       string `yaml:"name"`
	CIDR       string `yaml:"cidr"`
	BalancerIP string `yaml:"balancer_ip,omitempty"`
}

// MasterNode describes a master node.
type MasterNode struct {
	Name       string   `yaml:"name"`
	Host       string   `yaml:"host"`
	PeerHost   string   `yaml:"peer_host,omitempty"`
	OverlayIP  string   `yaml:"overlay_ip"`
	ListenPort int      `yaml:"listen_port"`
	GRPCPort   int      `yaml:"grpc_port,omitempty"`
	Endpoints  []string `yaml:"endpoints"`
	Exit       bool     `yaml:"exit,omitempty"`
}

// GRPCAddr returns host:grpc_port for this node (default 9090).
func (m MasterNode) GRPCAddr() string {
	port := m.GRPCPort
	if port == 0 {
		port = 9090
	}
	return m.Host + ":" + strconv.Itoa(port)
}

// PeerAddr returns the WG peering address (peer_host:listen_port).
// Falls back to host when peer_host is not set.
func (m MasterNode) PeerAddr() string {
	h := m.PeerHost
	if h == "" {
		h = m.Host
	}
	return h + ":" + strconv.Itoa(m.ListenPort)
}

// EndpointNode describes an endpoint node.
type EndpointNode struct {
	Name       string `yaml:"name"`
	Host       string `yaml:"host"`
	PeerHost   string `yaml:"peer_host,omitempty"`
	OverlayIP  string `yaml:"overlay_ip"`
	ListenPort int    `yaml:"listen_port"`
	GRPCPort   int    `yaml:"grpc_port,omitempty"`
	Region     string `yaml:"region"`
}

// GRPCAddr returns host:grpc_port for this node (default 9090).
func (e EndpointNode) GRPCAddr() string {
	port := e.GRPCPort
	if port == 0 {
		port = 9090
	}
	return e.Host + ":" + strconv.Itoa(port)
}

// PeerAddr returns the WG peering address (peer_host:listen_port).
// Falls back to host when peer_host is not set.
func (e EndpointNode) PeerAddr() string {
	h := e.PeerHost
	if h == "" {
		h = e.Host
	}
	return h + ":" + strconv.Itoa(e.ListenPort)
}

// VethConfig holds the MikroTik veth bridge settings for a client node.
// When present in the topology, these values override the built-in defaults
// used by 'mesh-ctl client prepare' when generating the RouterOS deploy script.
//
// B4 fix: the previous implementation hardcoded VethGateway to "192.168.100.1/24"
// which conflicted with common home-router subnets. Operators can now configure
// both the veth interface name suffix and the gateway CIDR via topology YAML.
type VethConfig struct {
	// Name is the veth interface name on the MikroTik (default: "veth-<clientName>").
	Name string `yaml:"name,omitempty"`
	// Gateway is the CIDR address assigned to the veth bridge port
	// (default: "192.168.100.1/24"). Change this when it conflicts with an
	// existing subnet on the router (e.g. "10.99.0.1/30").
	Gateway string `yaml:"gateway,omitempty"`
}

// MikrotikConfig holds optional MikroTik-specific settings for a client node.
// When nil, generator defaults apply (e.g. storage_root defaults to "docker").
type MikrotikConfig struct {
	// StorageRoot is the container storage root directory on the MikroTik (default: "docker").
	// Paths become /<storage_root>/... in the generated RouterOS script.
	// Allowed characters: alphanumeric, underscore, slash, hyphen (^[a-zA-Z0-9_/-]+$).
	// Must not contain ".." (path traversal) or start with "/" (the leading slash is added by generator).
	StorageRoot string `yaml:"storage_root,omitempty"`
}

// ClientNode describes a client node.
type ClientNode struct {
	Name      string `yaml:"name"`
	Type      string `yaml:"type"`
	Host      string `yaml:"host,omitempty"`
	OverlayIP string `yaml:"overlay_ip"`
	GRPCPort  int    `yaml:"grpc_port,omitempty"`
	// Veth holds MikroTik veth bridge configuration (mikrotik type only).
	// B4 fix: nil means use defaults (veth-<name>, 192.168.100.1/24).
	Veth *VethConfig `yaml:"veth,omitempty"`
	// Mikrotik holds optional MikroTik-specific settings (mikrotik type only).
	// When nil, all generator defaults apply.
	Mikrotik        *MikrotikConfig `yaml:"mikrotik,omitempty"`
	Masters         []string        `yaml:"masters"`
	RoutingPolicies []RoutingPolicy `yaml:"routing_policies,omitempty"`
	DNS             *DNSConfig      `yaml:"dns,omitempty"`
}

// RoutingPolicy defines a DSCP-to-target routing policy for client nodes.
type RoutingPolicy struct {
	Name    string   `yaml:"name"`
	DSCP    int      `yaml:"dscp"`
	Targets []string `yaml:"targets"`
}

// DNSConfig defines embedded DNS server settings for client nodes.
type DNSConfig struct {
	Zone     string `yaml:"zone"`
	Listen   string `yaml:"listen,omitempty"`
	Upstream string `yaml:"upstream,omitempty"`
}

// GRPCAddr returns host:grpc_port for this node (default 9090).
func (c ClientNode) GRPCAddr() string {
	h := c.Host
	if h == "" {
		h = "localhost"
	}
	port := c.GRPCPort
	if port == 0 {
		port = 9090
	}
	return h + ":" + strconv.Itoa(port)
}

// CaptureConfig contains capture job settings.
type CaptureConfig struct {
	DomainsFile   string `yaml:"domains_file"`
	Schedule      string `yaml:"schedule"`
	RetentionDays int    `yaml:"retention_days"`
}

// RotationConfig contains AWG rotation settings.
type RotationConfig struct {
	Defaults RotationDefaults `yaml:"defaults"`
}

// RotationDefaults contains default rotation intervals.
type RotationDefaults struct {
	Tier1Interval string `yaml:"tier1_interval"`
	Tier2Interval string `yaml:"tier2_interval"`
	Tier3Interval string `yaml:"tier3_interval"`
	Preset        string `yaml:"preset"`
}

// LoadTopology loads topology from YAML file.
func LoadTopology(path string) (*Topology, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("topology path is required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read topology file: %w", err)
	}

	var topology Topology
	if err := yaml.Unmarshal(data, &topology); err != nil {
		return nil, fmt.Errorf("unmarshal topology yaml: %w", err)
	}

	return &topology, nil
}

// FindMaster finds a master by name.
func (t *Topology) FindMaster(name string) *MasterNode {
	for i := range t.Masters {
		if t.Masters[i].Name == name {
			return &t.Masters[i]
		}
	}
	return nil
}

// FindEndpoint finds an endpoint by name.
func (t *Topology) FindEndpoint(name string) *EndpointNode {
	for i := range t.Endpoints {
		if t.Endpoints[i].Name == name {
			return &t.Endpoints[i]
		}
	}
	return nil
}

// FindClient finds a client by name.
func (t *Topology) FindClient(name string) *ClientNode {
	for i := range t.Clients {
		if t.Clients[i].Name == name {
			return &t.Clients[i]
		}
	}
	return nil
}

// MastersForEndpoint returns MasterNode entries whose Endpoints list contains
// endpointName, sorted by Name for deterministic iface creation order.
// The sort order defines the listen-port offset (index 0 = ListenPort + 0, etc.).
// Returns an empty (non-nil) slice when no master binds the endpoint.
func (t *Topology) MastersForEndpoint(endpointName string) []MasterNode {
	result := make([]MasterNode, 0)
	for _, master := range t.Masters {
		for _, ep := range master.Endpoints {
			if ep == endpointName {
				// Return a detached copy so callers cannot mutate topology state
				// via result[i].Endpoints (shallow struct copy would alias the slice).
				mCopy := master
				mCopy.Endpoints = append([]string(nil), master.Endpoints...)
				result = append(result, mCopy)
				break
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

// SaveTopology marshals t to YAML and writes it to path atomically.
func SaveTopology(path string, t *Topology) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("topology path is required")
	}
	if t == nil {
		return fmt.Errorf("topology value is required")
	}

	data, err := yaml.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal topology yaml: %w", err)
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
