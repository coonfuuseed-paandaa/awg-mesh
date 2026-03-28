package grpcserver

import (
	"time"

	"github.com/thebtf/awg-mesh/pkg/wg"
)

// TunnelManager handles tunnel lifecycle operations.
type TunnelManager interface {
	AddTunnel(name, endpointHost, overlayIP, balancerIP, transportSubnet, masterTransportIP, endpointTransportIP string, weight int, peerPublicKey wg.Key) error
	RemoveTunnel(name string) error
	ListTunnels() []TunnelInfo
	GetParams(tunnelName string) (wg.Config, error)
}

// ParamApplier applies AWG runtime parameter updates to an interface/tunnel.
type ParamApplier interface {
	ApplyParams(tunnelName string, cfg wg.Config) error
}

// PeerManager handles peer operations for endpoint mode.
type PeerManager interface {
	ListPeers() []PeerInfo
	AddPeer(publicKey []byte, presharedKey []byte, allowedIPs []string, endpointHost string, persistentKeepalive int32) error
	RemovePeer(publicKey []byte) error
}

// TransportConfigurator is an optional extension of PeerManager for modes that
// require per-peer transport IP assignment and routing (client mode).
// It is called by the AddPeer handler after saving transport state.
type TransportConfigurator interface {
	ConfigureTransport(pubkeyHex, localIP, peerIP string) error
}

// KeyProvider returns the node's WireGuard public key.
type KeyProvider interface {
	GetPublicKey() (wg.Key, error)
}

// NodeStateProvider exposes live node status metadata.
type NodeStateProvider interface {
	GetNodeState() NodeState
}

// CaptureFunc performs capture using a network interface and returns how many
// packets were captured.
type CaptureFunc func(interfaceName string, domains []string, countPerDomain int, timeout time.Duration) (int, error)

// CaptureScheduler manages autonomous capture scheduling on a node.
// SetSchedule configures domains, interval, and retention. The node runs capture
// autonomously — admin PC is not needed after configuration.
type CaptureScheduler interface {
	SetSchedule(domains []string, countPerDomain int, schedule string, retentionDays int) error
	StopSchedule()
}

type TunnelInfo struct {
	Name          string
	OverlayIP     string
	Healthy       bool
	Weight        int
	PeerPublicKey []byte
}

type PeerInfo struct {
	PublicKey     []byte
	Endpoint      string
	AllowedIPs    []string
	LastHandshake int64
	TxBytes       int64
	RxBytes       int64
}

type NodeState struct {
	Name      string
	Mode      string
	OverlayIP string
	Tunnels   []TunnelInfo
	StartTime time.Time
}
