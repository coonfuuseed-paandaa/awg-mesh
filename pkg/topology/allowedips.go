package topology

import (
	"fmt"
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
// Returns an error if masterOverlayIP or transportSubnet are empty.
func BuildAllowedIPsForEndpoint(topo *Topology, masterOverlayIP, transportSubnet string) ([]string, error) {
	if masterOverlayIP == "" {
		return nil, fmt.Errorf("master overlay IP is required")
	}
	if transportSubnet == "" {
		return nil, fmt.Errorf("transport subnet is required")
	}

	out := make([]string, 0, 4+len(topo.Overlay.Ranges))
	out = append(out, transportSubnet)       // e.g. 10.255.0.24/30
	out = append(out, masterOverlayIP+"/32") // e.g. 172.20.70.2/32

	for _, r := range topo.Overlay.Ranges {
		if r.CIDR != "" {
			out = append(out, r.CIDR) // 172.20.70.0/27, etc.
		}
	}

	return dedup(out), nil
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
