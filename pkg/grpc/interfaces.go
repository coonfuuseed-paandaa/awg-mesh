package grpcserver

import (
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

// TunnelManager handles tunnel lifecycle operations.
type TunnelManager interface {
	AddTunnel(name, endpointHost, overlayIP, balancerIP, transportSubnet, masterTransportIP, endpointTransportIP string, weight int, peerPublicKey wg.Key) error
	RemoveTunnel(name string) error
	ListTunnels() []TunnelInfo
	GetParams(tunnelName string) (wg.Config, error)
	GetListenPort(tunnelName string) (int, error)

	// UpdateTunnelPeer replaces the existing peer's public key on a named tunnel
	// atomically. Idempotent: when newPubkey matches the stored key, returns
	// (unchanged=true, nil) without touching the data plane.
	// On UAPI failure, restores the previous in-memory key and does NOT persist.
	UpdateTunnelPeer(name string, newPubkey [32]byte, balancerIP string, allowedIPs []string) (unchanged bool, err error)
}

// ParamApplier applies AWG runtime parameter updates to an interface/tunnel.
type ParamApplier interface {
	ApplyParams(tunnelName string, cfg wg.Config) error
}

// PeerManager handles peer operations for endpoint mode.
type PeerManager interface {
	ListPeers() []PeerInfo
	// AddPeer adds a peer to the endpoint. peerName carries the master name (on endpoint side)
	// or endpoint name (on master side) and is used for per-iface routing (v1.12.2+).
	// Empty peerName falls back to the first available interface for backwards compatibility.
	AddPeer(publicKey []byte, presharedKey []byte, allowedIPs []string, endpointHost string, persistentKeepalive int32, peerName string) error
	RemovePeer(publicKey []byte) error
}

// TransportConfigurator configures the transport-layer WireGuard interface
// with peer credentials and (mode-dependent) overlay routing.
//
// The allowedIPs parameter is mode-specific:
//   - In endpoint mode (EndpointRunner): allowedIPs lists overlay CIDRs to
//     install as kernel routes via RouteReplaceLink.
//   - In client mode (ClientRunner): allowedIPs is ignored - overlay routing
//     is handled separately via ECMP in rebuildClientECMP.
//
// peerName carries the master name (on endpoint side) used to route the
// transport IP assignment and overlay routes to the correct per-master
// WireGuard interface (v1.12.2+). Empty peerName falls back to the legacy
// single-interface behaviour.
//
// Implementations MUST be safe to call concurrently with each other but
// SHOULD serialize multiple calls to the same tunnel via internal locking.
type TransportConfigurator interface {
	ConfigureTransport(pubkeyHex, localIP, peerIP string, allowedIPs []string, peerName string, extraRoutes []string) error
}

// BalancerIPSetter is an optional extension for setting balancer IP on a peer
// link before ConfigureTransport triggers ECMP rebuild.
type BalancerIPSetter interface {
	SetBalancerIP(pubkeyHex, balancerIP string)
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
// autonomously - admin PC is not needed after configuration.
type CaptureScheduler interface {
	SetSchedule(domains []string, countPerDomain int, schedule string, retentionDays int) error
	StopSchedule()
}

// ClientStateSaver persists client-mode state after peer configuration changes.
type ClientStateSaver interface {
	SaveClientState() error
}

// NodeStatePersister is implemented by node runners that can persist and
// rebind a wireguard keypair (endpoint mode only; master/client return
// Unimplemented from the RotateKeypair handler). The handler uses this
// interface to gate which node modes can accept keypair-rotation RPCs.
type NodeStatePersister interface {
	// LoadKeypair returns the current private key bytes for the named tunnel.
	// Returns os.ErrNotExist wrapped if the state file does not yet exist.
	LoadKeypair(tunnelName string) ([]byte, error)

	// PersistKeypair atomically writes the new private key for the named
	// tunnel via .tmp + rename at mode 0600. Fail-closed: only synthesizes
	// fresh state on os.ErrNotExist; propagates corrupt/permission errors.
	PersistKeypair(tunnelName string, privateKey []byte) error

	// LockRotation acquires the rotation mutex for the named tunnel and
	// returns an unlock func. Callers MUST defer the unlock immediately.
	// Serializes Load -> Persist -> Apply -> rollback against concurrent RPCs.
	LockRotation(tunnelName string) (unlock func(), err error)
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
