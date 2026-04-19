package topology

import (
	"fmt"
	"net"
)

// BuildAllowedIPsForEndpoint returns the allowed_ips list that a master
// should send to an endpoint via AddPeer/UpdateTunnelPeer. The list contains:
//  1. The per-tunnel transport /30 (transport_subnet).
//  2. The master's own overlay IP as /32 (so the endpoint knows it can reach
//     the master over this tunnel).
//  3. Every CIDR from topology.overlay.ranges[*].cidr.
//
// Called from both `master init`, `endpoint init`, and `reconcile` paths to ensure
// identical AllowedIPs semantics across all code paths.
//
// Returns an error if masterOverlayIP or transportSubnet are empty or malformed.
func BuildAllowedIPsForEndpoint(topo *Topology, masterOverlayIP, transportSubnet string) ([]string, error) {
	if masterOverlayIP == "" {
		return nil, fmt.Errorf("master overlay IP is required")
	}
	if transportSubnet == "" {
		return nil, fmt.Errorf("transport subnet is required")
	}

	// Validate masterOverlayIP is a plain IP (not a CIDR).
	if net.ParseIP(masterOverlayIP) == nil {
		return nil, fmt.Errorf("invalid master overlay IP %q: not a valid IP address", masterOverlayIP)
	}

	// Validate transportSubnet is a valid CIDR.
	if _, _, err := net.ParseCIDR(transportSubnet); err != nil {
		return nil, fmt.Errorf("invalid transport subnet %q: %w", transportSubnet, err)
	}

	out := make([]string, 0, 4+len(topo.Overlay.Ranges))
	out = append(out, transportSubnet)       // e.g. 10.255.0.24/30
	out = append(out, masterOverlayIP+"/32") // e.g. 172.20.70.2/32

	for _, r := range topo.Overlay.Ranges {
		if r.CIDR == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(r.CIDR); err != nil {
			return nil, fmt.Errorf("invalid overlay range CIDR %q in range %q: %w", r.CIDR, r.Name, err)
		}
		out = append(out, r.CIDR) // 172.20.70.0/27, etc.
	}

	return dedup(out), nil
}

// BuildMinimalAllowedIPsForEndpointPeer returns the minimal AllowedIPs for one
// endpoint-side WG peer (a master): [transport_subnet, master_overlay_ip/32].
// This is the correct set for the per-master-iface (Pattern X) model in v1.12.2.
// Do NOT use BuildAllowedIPsForEndpoint for endpoint-side — that produces the full
// overlapping list appropriate only for the master side (which uses one iface per peer).
//
// Order: transport subnet first, then overlay /32 — matches master-side
// computeMasterPeerAllowedIPs convention.
//
// IPv4 only — consistent with BuildAllowedIPsForEndpoint scope.
//
// Returns an error if masterOverlayIP or transportSubnet are empty or malformed.
func BuildMinimalAllowedIPsForEndpointPeer(masterOverlayIP, transportSubnet string) ([]string, error) {
	if masterOverlayIP == "" {
		return nil, fmt.Errorf("master overlay IP is required")
	}
	if transportSubnet == "" {
		return nil, fmt.Errorf("transport subnet is required")
	}

	// Validate masterOverlayIP is a plain IP (not a CIDR).
	if net.ParseIP(masterOverlayIP) == nil {
		return nil, fmt.Errorf("invalid master overlay IP %q: not a valid IP address", masterOverlayIP)
	}

	// Validate transportSubnet is a valid CIDR.
	if _, _, err := net.ParseCIDR(transportSubnet); err != nil {
		return nil, fmt.Errorf("invalid transport subnet %q: %w", transportSubnet, err)
	}

	return []string{transportSubnet, masterOverlayIP + "/32"}, nil
}

// dedup returns a new slice with duplicate strings removed, preserving order.
func dedup(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
