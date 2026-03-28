package topology

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Topology is the full mesh topology configuration.
type Topology struct {
	Overlay   OverlayConfig   `yaml:"overlay"`
	Masters   []MasterNode    `yaml:"masters"`
	Endpoints []EndpointNode  `yaml:"endpoints"`
	Clients   []ClientNode    `yaml:"clients"`
	Capture   CaptureConfig   `yaml:"capture"`
	Rotation  RotationConfig  `yaml:"rotation"`
	Transport TransportConfig `yaml:"transport"`
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

// ClientNode describes a client node.
type ClientNode struct {
	Name      string   `yaml:"name"`
	Type      string   `yaml:"type"`
	OverlayIP string   `yaml:"overlay_ip"`
	Masters   []string `yaml:"masters"`
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

// TransportConfig contains transport subnet allocation settings.
type TransportConfig struct {
	Pool         string `yaml:"pool"`
	PrefixLength int    `yaml:"prefix_length"`
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
