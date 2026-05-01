package wg

// Protocol identifies a WireGuard transport variant.
//
// Protocols differ at the wire-protocol level (vanilla WireGuard vs.
// AmneziaWG header obfuscation), but share the same UAPI control surface.
// The Transport interface abstracts both behind a common Go API so callers
// (master, clientd, egress, ingress, balancer) can be transport-agnostic
// at the configuration layer.
type Protocol string

const (
	// ProtocolVanilla is standard WireGuard. No header obfuscation. Used on
	// master's client-facing listener (FR-12) for Mikrotik native compatibility.
	ProtocolVanilla Protocol = "vanilla-wg"

	// ProtocolAmneziaWG is WireGuard with AmneziaWG (S/H/I/J) obfuscation.
	// Used on every mesh-internal link per F-009 architecture.
	ProtocolAmneziaWG Protocol = "amneziawg"
)

// Transport abstracts a WireGuard or AmneziaWG TUN device under a common
// control API. Implementations (transport_vanilla.go, transport_awg.go) wrap
// kernel WG / amneziawg-go with a Protocol-specific Configure path.
//
// Each Transport owns exactly one TUN device and one UAPI control socket. A
// node holds one Transport instance per protocol it serves: client/egress/
// ingress/balancer hold one (mesh-internal AmneziaWG); master holds two
// (vanilla on client-facing port, AmneziaWG on mesh-internal port).
//
// Implementations MUST be safe for concurrent AddPeer / RemovePeer / Stats
// calls (per FR-5 partitioned-ownership reload semantics, multiple Goroutines
// may modify peer state under control-plane direction).
type Transport interface {
	// Protocol reports which wire protocol this transport instance serves.
	Protocol() Protocol

	// Name returns the underlying TUN device name (e.g. "wg-mesh", "wg-clients").
	Name() string

	// Configure applies a full device-level configuration. Used at startup
	// and during mesh-wide rotation (FR-8) to replace AWG params atomically.
	// Calling Configure with an empty cfg.Peers list and ReplacePeers=true
	// drops all existing peers.
	Configure(cfg Config) error

	// AddPeer adds or updates a peer. If the peer's public key matches an
	// existing entry, the entry is updated in place (UpdateOnly semantics).
	AddPeer(p PeerConfig) error

	// RemovePeer removes the peer identified by public key. No-op if absent.
	RemovePeer(key Key) error

	// Stats returns a snapshot of the device including all current peers
	// (handshake timestamps, byte counters, AllowedIPs).
	Stats() (*Device, error)

	// Close tears down the TUN device and releases the UAPI socket. After
	// Close, all other methods return an error.
	Close() error
}
