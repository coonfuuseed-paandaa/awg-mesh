//go:build linux

package node

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/device"
	grpcserver "github.com/thebtf/awg-mesh/pkg/grpc"
	"github.com/thebtf/awg-mesh/pkg/routing"
	"github.com/thebtf/awg-mesh/pkg/wg"
)

const clientInterfacePrefix = "wg-c"

type transportLink struct {
	iface            *wg.Interface
	pubkeyHex        string
	localTransportIP string
	peerTransportIP  string
	balancerIP       string
	healthy          bool
}

type clientPlatformState struct {
	mu      sync.Mutex
	links   []*transportLink
	byKey   map[string]*transportLink
	nextIdx int
}

func (c *ClientRunner) createInterfaces(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if c == nil || c.node == nil {
		return fmt.Errorf("client runner node is required")
	}

	c.node.logger.Info().Msg("client interfaces will be configured via mesh-ctl client init")
	return nil
}

// AddPeer implements grpcserver.PeerManager.
// It creates a per-peer WireGuard interface and configures the master peer on it.
func (c *ClientRunner) AddPeer(publicKey []byte, presharedKey []byte, allowedIPs []string, endpointHost string, persistentKeepalive int32) error {
	if c == nil || c.node == nil {
		return fmt.Errorf("client runner node is required")
	}
	if len(publicKey) == 0 {
		return fmt.Errorf("public key is required")
	}

	pubkeyHex := hex.EncodeToString(publicKey)

	c.platformState.mu.Lock()
	existingLink, hasExistingLink := c.platformState.byKey[pubkeyHex]
	if hasExistingLink {
		c.platformState.mu.Unlock()
		return c.configurePeerOnIface(existingLink.iface, publicKey, presharedKey, allowedIPs, endpointHost, persistentKeepalive)
	}

	ifaceName := fmt.Sprintf("%s%d", clientInterfacePrefix, c.platformState.nextIdx)
	c.platformState.nextIdx++
	c.platformState.mu.Unlock()

	privateKey, _, err := EnsureKeypair(c.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}

	mtu := CalculateMTU(1420, 80, 1)
	iface, err := wg.NewInterface(ifaceName, mtu, device.NewLogger(device.LogLevelError, "[client] "))
	if err != nil {
		return fmt.Errorf("create client interface %q: %w", ifaceName, err)
	}

	if err := iface.Configure(wg.Config{PrivateKey: &privateKey}); err != nil {
		_ = iface.Close()
		return fmt.Errorf("configure private key on %q: %w", ifaceName, err)
	}

	if err := c.configurePeerOnIface(iface, publicKey, presharedKey, allowedIPs, endpointHost, persistentKeepalive); err != nil {
		_ = iface.Close()
		return fmt.Errorf("configure peer on %q: %w", ifaceName, err)
	}

	newLink := &transportLink{
		iface:     iface,
		pubkeyHex: pubkeyHex,
		healthy:   true,
	}

	c.platformState.mu.Lock()
	if c.platformState.byKey == nil {
		c.platformState.byKey = make(map[string]*transportLink)
	}
	if existing, exists := c.platformState.byKey[pubkeyHex]; exists {
		c.platformState.mu.Unlock()
		_ = iface.Close()
		return c.configurePeerOnIface(existing.iface, publicKey, presharedKey, allowedIPs, endpointHost, persistentKeepalive)
	}

	nextLinks := append(append([]*transportLink(nil), c.platformState.links...), newLink)
	c.platformState.links = nextLinks
	c.platformState.byKey[pubkeyHex] = newLink
	c.platformState.mu.Unlock()

	peerLabel := pubkeyHex
	if len(peerLabel) > 8 {
		peerLabel = peerLabel[:8] + "..."
	}
	c.node.logger.Info().
		Str("interface", ifaceName).
		Str("peer", peerLabel).
		Int("mtu", mtu).
		Msg("client interface created")

	return nil
}

func (c *ClientRunner) configurePeerOnIface(iface *wg.Interface, publicKey []byte, presharedKey []byte, allowedIPs []string, endpointHost string, persistentKeepalive int32) error {
	if iface == nil {
		return fmt.Errorf("interface is required")
	}

	key, err := wg.NewKey(publicKey)
	if err != nil {
		return fmt.Errorf("parse peer public key: %w", err)
	}

	peerCfg := wg.PeerConfig{
		PublicKey:         key,
		ReplaceAllowedIPs: true,
	}

	if len(presharedKey) == 32 {
		psk, err := wg.NewKey(presharedKey)
		if err == nil {
			peerCfg.PresharedKey = &psk
		}
	}

	for _, cidr := range allowedIPs {
		_, ipNet, err := net.ParseCIDR(strings.TrimSpace(cidr))
		if err != nil {
			return fmt.Errorf("parse allowed IP %q: %w", cidr, err)
		}
		peerCfg.AllowedIPs = append(peerCfg.AllowedIPs, *ipNet)
	}

	resolvedEndpointHost := extractPeerEndpoint(endpointHost)
	if resolvedEndpointHost != "" {
		addr, err := net.ResolveUDPAddr("udp", resolvedEndpointHost)
		if err != nil {
			return fmt.Errorf("resolve endpoint %q: %w", resolvedEndpointHost, err)
		}
		peerCfg.Endpoint = addr
	}

	if persistentKeepalive > 0 {
		interval := time.Duration(persistentKeepalive) * time.Second
		peerCfg.PersistentKeepaliveInterval = &interval
	}

	return iface.Configure(wg.Config{Peers: []wg.PeerConfig{peerCfg}})
}

// ListPeers implements grpcserver.PeerManager.
func (c *ClientRunner) ListPeers() []grpcserver.PeerInfo {
	if c == nil {
		return nil
	}

	c.platformState.mu.Lock()
	linksSnapshot := append([]*transportLink(nil), c.platformState.links...)
	c.platformState.mu.Unlock()

	result := make([]grpcserver.PeerInfo, 0)
	for _, link := range linksSnapshot {
		if link == nil || link.iface == nil {
			continue
		}
		dev, err := link.iface.GetDevice()
		if err != nil {
			continue
		}
		for _, peer := range dev.Peers {
			endpoint := ""
			if peer.Endpoint != nil {
				endpoint = peer.Endpoint.String()
			}
			allowedIPs := make([]string, 0, len(peer.AllowedIPs))
			for _, allowedIP := range peer.AllowedIPs {
				allowedIPs = append(allowedIPs, allowedIP.String())
			}
			result = append(result, grpcserver.PeerInfo{
				PublicKey:     append([]byte(nil), peer.PublicKey[:]...),
				Endpoint:      endpoint,
				AllowedIPs:    allowedIPs,
				LastHandshake: peer.LastHandshakeTime.Unix(),
				TxBytes:       peer.TransmitBytes,
				RxBytes:       peer.ReceiveBytes,
			})
		}
	}

	return result
}

// RemovePeer implements grpcserver.PeerManager.
func (c *ClientRunner) RemovePeer(publicKey []byte) error {
	if c == nil {
		return nil
	}
	if len(publicKey) == 0 {
		return fmt.Errorf("public key is required")
	}

	pubkeyHex := hex.EncodeToString(publicKey)

	c.platformState.mu.Lock()
	link, exists := c.platformState.byKey[pubkeyHex]
	if !exists {
		c.platformState.mu.Unlock()
		return nil
	}

	nextLinks := make([]*transportLink, 0, len(c.platformState.links)-1)
	for _, currentLink := range c.platformState.links {
		if currentLink != link {
			nextLinks = append(nextLinks, currentLink)
		}
	}

	nextByKey := make(map[string]*transportLink, len(c.platformState.byKey)-1)
	for key, value := range c.platformState.byKey {
		if key != pubkeyHex {
			nextByKey[key] = value
		}
	}

	c.platformState.links = nextLinks
	c.platformState.byKey = nextByKey
	linksSnapshot := append([]*transportLink(nil), nextLinks...)
	c.platformState.mu.Unlock()

	if err := c.updateDefaultRoute(linksSnapshot); err != nil {
		c.node.logger.Warn().Err(err).Msg("update default route after peer removal failed")
	}

	return link.iface.Close()
}

// ConfigureTransport implements grpcserver.TransportConfigurator.
func (c *ClientRunner) ConfigureTransport(pubkeyHex, localIP, peerIP string) error {
	if c == nil || c.node == nil {
		return fmt.Errorf("client runner node is required")
	}

	trimmedPubkeyHex := strings.TrimSpace(pubkeyHex)
	if trimmedPubkeyHex == "" {
		return fmt.Errorf("pubkey hex is required")
	}

	trimmedLocalIP := strings.TrimSpace(localIP)
	if net.ParseIP(trimmedLocalIP) == nil {
		return fmt.Errorf("local transport IP %q is invalid", localIP)
	}

	trimmedPeerIP := strings.TrimSpace(peerIP)
	if net.ParseIP(trimmedPeerIP) == nil {
		return fmt.Errorf("peer transport IP %q is invalid", peerIP)
	}

	c.platformState.mu.Lock()
	link, exists := c.platformState.byKey[trimmedPubkeyHex]
	if !exists {
		c.platformState.mu.Unlock()
		return fmt.Errorf("no interface found for peer %q", trimmedPubkeyHex)
	}

	updatedLink := &transportLink{
		iface:            link.iface,
		pubkeyHex:        link.pubkeyHex,
		localTransportIP: trimmedLocalIP,
		peerTransportIP:  trimmedPeerIP,
		balancerIP:       link.balancerIP,
		healthy:          link.healthy,
	}

	nextLinks := make([]*transportLink, 0, len(c.platformState.links))
	for _, currentLink := range c.platformState.links {
		if currentLink == link {
			nextLinks = append(nextLinks, updatedLink)
			continue
		}
		nextLinks = append(nextLinks, currentLink)
	}

	nextByKey := make(map[string]*transportLink, len(c.platformState.byKey))
	for key, value := range c.platformState.byKey {
		nextByKey[key] = value
	}
	nextByKey[trimmedPubkeyHex] = updatedLink

	c.platformState.links = nextLinks
	c.platformState.byKey = nextByKey
	c.platformState.mu.Unlock()

	if err := ensureInterfaceAddress(updatedLink.iface.Name(), trimmedLocalIP); err != nil {
		return fmt.Errorf("assign transport IP %s/30 on %s: %w", trimmedLocalIP, updatedLink.iface.Name(), err)
	}

	if updatedLink.balancerIP != "" {
		if err := c.rebuildECMP(updatedLink.balancerIP); err != nil {
			return err
		}
	} else {
		linksSnapshot := append([]*transportLink(nil), nextLinks...)
		if err := c.updateDefaultRoute(linksSnapshot); err != nil {
			return err
		}
	}

	peerLabel := trimmedPubkeyHex
	if len(peerLabel) > 8 {
		peerLabel = peerLabel[:8] + "..."
	}
	c.node.logger.Info().
		Str("interface", updatedLink.iface.Name()).
		Str("peer", peerLabel).
		Str("local_ip", trimmedLocalIP).
		Str("peer_ip", trimmedPeerIP).
		Msg("configured client transport link")

	return nil
}

func (c *ClientRunner) reconcileFromTransportState() error {
	if c == nil || c.node == nil {
		return fmt.Errorf("client runner node is required")
	}

	state, err := loadNodeTransportState(c.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("load node transport state: %w", err)
	}
	if len(state.Tunnels) == 0 {
		return nil
	}

	reconciled := 0
	for _, tunnel := range state.Tunnels {
		if strings.TrimSpace(tunnel.PeerPublicKey) == "" || strings.TrimSpace(tunnel.TransportIP) == "" || strings.TrimSpace(tunnel.PeerTransportIP) == "" {
			continue
		}

		peerPublicKey, err := hex.DecodeString(strings.TrimSpace(tunnel.PeerPublicKey))
		if err != nil {
			c.node.logger.Warn().
				Str("tunnel", tunnel.Name).
				Err(err).
				Msg("reconcile peer: decode key failed")
			continue
		}

		if err := c.AddPeer(peerPublicKey, nil, []string{"0.0.0.0/0"}, strings.TrimSpace(tunnel.PeerEndpoint), 25); err != nil {
			c.node.logger.Warn().
				Str("tunnel", tunnel.Name).
				Err(err).
				Msg("reconcile peer failed")
			continue
		}

		pubkeyHex := hex.EncodeToString(peerPublicKey)
		if balancerIP := strings.TrimSpace(tunnel.BalancerIP); balancerIP != "" {
			c.SetBalancerIP(pubkeyHex, balancerIP)
		}
		if err := c.ConfigureTransport(pubkeyHex, strings.TrimSpace(tunnel.TransportIP), strings.TrimSpace(tunnel.PeerTransportIP)); err != nil {
			c.node.logger.Warn().
				Str("tunnel", tunnel.Name).
				Err(err).
				Msg("reconcile transport failed")
			continue
		}

		reconciled++
	}

	c.node.logger.Info().
		Int("tunnels", reconciled).
		Msg("reconciled client transport from saved state")

	return nil
}

func (c *ClientRunner) closeInterfaces() error {
	if c == nil {
		return nil
	}

	c.platformState.mu.Lock()
	linksSnapshot := append([]*transportLink(nil), c.platformState.links...)
	c.platformState.links = nil
	c.platformState.byKey = nil
	c.platformState.mu.Unlock()

	closeErrors := make([]string, 0, len(linksSnapshot))
	for _, link := range linksSnapshot {
		if link == nil || link.iface == nil {
			continue
		}
		if err := link.iface.Close(); err != nil {
			closeErrors = append(closeErrors, err.Error())
		}
	}

	if len(closeErrors) == 0 {
		return nil
	}
	return fmt.Errorf("close client interfaces: %s", strings.Join(closeErrors, "; "))
}

func ensureInterfaceAddress(interfaceName, address string) error {
	trimmedAddress := strings.TrimSpace(address)
	parsedIP := net.ParseIP(trimmedAddress)
	if parsedIP == nil {
		return fmt.Errorf("invalid transport IP %q", address)
	}

	normalizedCIDR := parsedIP.String() + "/30"
	showOutput, err := exec.Command("ip", "addr", "show", "dev", interfaceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip addr show dev %s: %w: %s", interfaceName, err, strings.TrimSpace(string(showOutput)))
	}
	if strings.Contains(string(showOutput), normalizedCIDR) {
		return nil
	}

	if err := addInterfaceAddress(interfaceName, parsedIP.String()); err != nil {
		if strings.Contains(err.Error(), "File exists") {
			return nil
		}
		return err
	}
	return nil
}

func (c *ClientRunner) updateDefaultRoute(links []*transportLink) error {
	nexthops := buildClientNexthops(links)
	if len(nexthops) == 0 {
		if err := routing.RemoveECMPRoute("default"); err != nil {
			return fmt.Errorf("remove default ECMP route: %w", err)
		}
		return nil
	}

	if err := routing.SetECMPRoute("default", nexthops); err != nil {
		return fmt.Errorf("set default ECMP route: %w", err)
	}
	return nil
}

func buildClientNexthops(links []*transportLink) []routing.NextHop {
	nexthops := make([]routing.NextHop, 0, len(links))
	for _, link := range links {
		if link == nil || link.iface == nil || strings.TrimSpace(link.peerTransportIP) == "" {
			continue
		}
		nexthops = append(nexthops, routing.NextHop{
			Via:    link.peerTransportIP,
			Dev:    link.iface.Name(),
			Weight: 1,
		})
	}
	return nexthops
}

// startHealthCheck launches the healthcheck goroutine for client transport links.
func (c *ClientRunner) startHealthCheck(ctx context.Context) {
	hcCfg := HealthConfig{
		Interval:         defaultHealthInterval,
		Timeout:          defaultHealthTimeout,
		FailureThreshold: defaultHealthFailureThreshold,
	}
	hcLogger := c.node.logger.With().Str("component", "healthcheck").Logger()
	hc := NewHealthChecker(hcCfg, hcLogger)

	go hc.Run(ctx, c.healthTargets,
		func(name string) {
			var balancerIP string
			c.platformState.mu.Lock()
			link := c.platformState.byKey[name]
			if link != nil {
				link.healthy = false
				balancerIP = link.balancerIP
			}
			c.platformState.mu.Unlock()
			if balancerIP != "" {
				c.rebuildECMP(balancerIP) //nolint:errcheck // health callback: logged inside rebuildECMP
			}
		},
		func(name string) {
			var balancerIP string
			c.platformState.mu.Lock()
			link := c.platformState.byKey[name]
			if link != nil {
				link.healthy = true
				balancerIP = link.balancerIP
			}
			c.platformState.mu.Unlock()
			if balancerIP != "" {
				c.rebuildECMP(balancerIP) //nolint:errcheck // health callback: logged inside rebuildECMP
			}
		},
	)
}

// rebuildECMP builds a multipath route to balancerIP/32 using all healthy
// transport links that share the same balancer IP. Mirrors master's rebuildECMP.
// Returns an error if any routing or iptables operation fails.
func (c *ClientRunner) rebuildECMP(balancerIP string) error {
	if balancerIP == "" {
		return nil
	}

	cidr := balancerIP + "/32"
	c.platformState.mu.Lock()
	nexthops := make([]routing.NextHop, 0, len(c.platformState.links))
	for _, link := range c.platformState.links {
		if link.balancerIP == balancerIP && link.healthy && strings.TrimSpace(link.peerTransportIP) != "" {
			nexthops = append(nexthops, routing.NextHop{
				Via:    link.peerTransportIP,
				Dev:    link.iface.Name(),
				Weight: 1,
			})
		}
	}
	c.platformState.mu.Unlock()

	if len(nexthops) == 0 {
		if err := routing.DisableStickyECMP(cidr); err != nil {
			c.node.logger.Warn().Str("balancer_ip", balancerIP).Err(err).Msg("failed to disable sticky ECMP rules")
		}
		if err := routing.RemoveECMPRoute(cidr); err != nil {
			c.node.logger.Warn().Str("balancer_ip", balancerIP).Err(err).Msg("failed to remove client ECMP route")
			return fmt.Errorf("remove client ECMP route for %s: %w", balancerIP, err)
		}
		c.node.logger.Info().Str("balancer_ip", balancerIP).Msg("client ECMP route removed")
		return nil
	}

	if err := routing.SetECMPRoute(cidr, nexthops); err != nil {
		c.node.logger.Warn().Str("balancer_ip", balancerIP).Err(err).Msg("failed to set client ECMP route")
		return fmt.Errorf("set client ECMP route for %s: %w", balancerIP, err)
	}

	if err := routing.EnableStickyECMP(cidr); err != nil {
		c.node.logger.Warn().Str("balancer_ip", balancerIP).Err(err).Msg("failed to enable sticky ECMP rules")
		return fmt.Errorf("enable sticky ECMP for %s: %w", balancerIP, err)
	}

	if err := routing.EnableL4Hash(); err != nil {
		c.node.logger.Warn().Str("balancer_ip", balancerIP).Err(err).Msg("failed to enable L4 hash")
		return fmt.Errorf("enable L4 hash: %w", err)
	}

	c.node.logger.Info().Str("balancer_ip", balancerIP).Int("nexthops", len(nexthops)).Msg("client ECMP route updated")
	return nil
}

// SetBalancerIP sets the balancer IP for a specific peer link.
func (c *ClientRunner) SetBalancerIP(pubkeyHex, balancerIP string) {
	c.platformState.mu.Lock()
	link, exists := c.platformState.byKey[strings.TrimSpace(pubkeyHex)]
	if exists && link != nil {
		link.balancerIP = strings.TrimSpace(balancerIP)
	}
	c.platformState.mu.Unlock()
}

// healthTargets returns health check targets from all transport links.
func (c *ClientRunner) healthTargets() []HealthTarget {
	c.platformState.mu.Lock()
	defer c.platformState.mu.Unlock()

	targets := make([]HealthTarget, 0, len(c.platformState.links))
	for _, link := range c.platformState.links {
		if link == nil || link.peerTransportIP == "" {
			continue
		}
		targets = append(targets, HealthTarget{
			Name:     link.pubkeyHex,
			PingAddr: link.peerTransportIP,
			Healthy:  link.healthy,
		})
	}
	return targets
}

func extractPeerEndpoint(endpointHost string) string {
	trimmedEndpointHost := strings.TrimSpace(endpointHost)
	if trimmedEndpointHost == "" {
		return ""
	}

	if strings.Contains(trimmedEndpointHost, "|") {
		parts := strings.SplitN(trimmedEndpointHost, "|", 2)
		if len(parts) == 2 {
			return strings.TrimSpace(parts[1])
		}
	}

	return trimmedEndpointHost
}
