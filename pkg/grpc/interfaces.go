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
	AddPeer(publicKey []byte, presharedKey []byte, allowedIPs []string, endpointHost string, persistentKeepalive int32) error
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
// Implementations MUST be safe to call concurrently with each other but
// SHOULD serialize multiple calls to the same tunnel via internal locking.
type TransportConfigurator interface {
	ConfigureTransport(pubkeyHex, localIP, peerIP string, allowedIPs []string) error
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

// NodeStatePersister extends NodeStateProvider with the ability to load and
// atomically persist the node's on-disk keypair (node.yml).
//
// Implemented only by endpoint mode. Handlers that need to rotate an endpoint's
// keypair perform a type assertion on stateProvider — if the assertion fails,
// the mode does not support keypair rotation (reject with codes.Unimplemented).
//
// Contract:
//   - LoadKeypair reads the current keypair from disk; returns error if the
//     state file is missing, malformed, or the keypair fields are absent.
//   - PersistKeypair writes atomically (.tmp + rename) with mode 0600.
//   - On success the on-disk state has the new PrivateKey and PublicKey; other
//     fields (Name, Mode, OverlayIP) are preserved.
//   - On error the on-disk state is unchanged (caller relies on this for rollback).
//   - LockRotation serializes the entire rotation sequence (Load → Persist →
//     Apply → optional rollback Persist+Apply). Returns an unlock closure that
//     callers MUST defer. Concurrent RotateKeypair invocations interleaving the
//     load/persist/apply steps would otherwise leave the on-disk and runtime
//     state inconsistent.
type NodeStatePersister interface {
	LoadKeypair() (priv wg.Key, pub wg.Key, err error)
	PersistKeypair(priv wg.Key, pub wg.Key) error
	LockRotation() (unlock func())
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
