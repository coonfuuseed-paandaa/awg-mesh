package clientd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
)

var (
	// ErrPeerPublicKeyRequired is returned when a peer-list entry lacks the kernel peer key.
	ErrPeerPublicKeyRequired = errors.New("peer public key is required")
	// ErrNonMasterPeerRejected is returned when client role receives an explicit non-master peer.
	ErrNonMasterPeerRejected = errors.New("clientd accepts only master peers")
)

// ReloadInput is the validated input for peer reload conversion.
type ReloadInput struct {
	LocalRoles []role.Role
	Peers      []PeerEntry
	Ownership  []OwnershipEntry
}

// TransportConfigurator applies peer snapshots through pkg/wg Transport.
type TransportConfigurator struct {
	Transport  wg.Transport
	LocalRoles []role.Role
}

// Apply validates the state and updates local Transport peers.
func (c TransportConfigurator) Apply(ctx context.Context, state State) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if c.Transport == nil {
		return errors.New("transport is required")
	}
	localRoles := c.LocalRoles
	if len(localRoles) == 0 {
		localRoles = []role.Role{role.RoleClient}
	}
	peers, err := BuildPeerConfigs(ReloadInput{LocalRoles: localRoles, Peers: state.Peers, Ownership: state.Ownership})
	if err != nil {
		if errors.Is(err, ErrPeerPublicKeyRequired) {
			return nil
		}
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if err := c.Transport.Configure(wg.Config{Peers: peers, ReplacePeers: true}); err != nil {
		return fmt.Errorf("configure peers: %w", err)
	}
	return nil
}

// BuildPeerConfigs validates reload input and converts peer entries into wg.PeerConfig values.
func BuildPeerConfigs(input ReloadInput) ([]wg.PeerConfig, error) {
	if localHasClient(input.LocalRoles) {
		for _, peer := range input.Peers {
			if peer.PeerRole != "" && peer.PeerRole != role.RoleMaster {
				return nil, fmt.Errorf("%w: peer %q has role %q", ErrNonMasterPeerRejected, peer.PeerName, peer.PeerRole)
			}
		}
	}
	out := make([]wg.PeerConfig, 0, len(input.Peers))
	for _, peer := range input.Peers {
		cfg, err := PeerEntryToWGConfig(peer)
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	return out, nil
}

// PeerEntryToWGConfig converts one peer entry into a WireGuard peer config.
func PeerEntryToWGConfig(peer PeerEntry) (wg.PeerConfig, error) {
	if strings.TrimSpace(peer.PeerName) == "" {
		return wg.PeerConfig{}, errors.New("peer name is required")
	}
	protocol := peer.Protocol
	if protocol != "" && protocol != wg.ProtocolVanilla && protocol != wg.ProtocolAmneziaWG {
		return wg.PeerConfig{}, fmt.Errorf("unsupported peer protocol %q", protocol)
	}
	if len(peer.PeerPubkey) == 0 {
		return wg.PeerConfig{}, fmt.Errorf("%w for peer %q", ErrPeerPublicKeyRequired, peer.PeerName)
	}
	key, err := wg.NewKey(peer.PeerPubkey)
	if err != nil {
		return wg.PeerConfig{}, fmt.Errorf("parse peer %q public key: %w", peer.PeerName, err)
	}
	allowed, err := parseAllowedIPs(peer.AllowedIPs)
	if err != nil {
		return wg.PeerConfig{}, fmt.Errorf("parse peer %q allowed IPs: %w", peer.PeerName, err)
	}
	cfg := wg.PeerConfig{
		PublicKey:         key,
		ReplaceAllowedIPs: true,
		AllowedIPs:        allowed,
	}
	if strings.TrimSpace(peer.PeerEndpointHost) != "" {
		addr, err := net.ResolveUDPAddr("udp", peer.PeerEndpointHost)
		if err != nil {
			return wg.PeerConfig{}, fmt.Errorf("parse peer %q endpoint: %w", peer.PeerName, err)
		}
		cfg.Endpoint = addr
	}
	if peer.PersistentKeepaliveSecs > 0 {
		keepalive := time.Duration(peer.PersistentKeepaliveSecs) * time.Second
		cfg.PersistentKeepaliveInterval = &keepalive
	}
	return cfg, nil
}

func parseAllowedIPs(values []string) ([]net.IPNet, error) {
	out := make([]net.IPNet, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(trimmed)
		if err != nil {
			ip := net.ParseIP(trimmed)
			if ip == nil {
				return nil, fmt.Errorf("%q is neither CIDR nor IP", value)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			ipNet = &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
		}
		out = append(out, *ipNet)
	}
	return out, nil
}

func localHasClient(roles []role.Role) bool {
	return slices.Contains(roles, role.RoleClient)
}
