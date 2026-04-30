package topology

import (
	"fmt"
	"net"
	"strings"
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
// endpoint-side WG peer (a master): [transport_subnet, master_overlay_ip/32]
// plus the topology "clients" overlay range when topo carries one. This is the
// correct set for the per-master-iface (Pattern X) model in v1.12.2.
// Do NOT use BuildAllowedIPsForEndpoint for endpoint-side — that produces the full
// overlapping list appropriate only for the master side (which uses one iface per peer).
//
// Order: transport subnet first, then master overlay /32, then clients range —
// matches master-side computeMasterPeerAllowedIPs convention.
//
// The clients range is appended whenever topology.overlay.ranges[*].name == "clients"
// is present (case-insensitive). Without it, packets travelling endpoint→master→client
// are dropped on the endpoint's WireGuard INBOUND filter (src=client_overlay not in
// AllowedIPs) and the endpoint's reply path has no kernel route. Mirrors the existing
// endpoints-range pattern fixed for cross-endpoint forwarding in v1.12.7 (issue #147).
//
// topo may be nil — in that case the clients range is omitted (callers using nil
// topology must explicitly accept the trade-off; production callers always pass topo).
//
// IPv4 only — consistent with BuildAllowedIPsForEndpoint scope.
//
// Returns an error if masterOverlayIP or transportSubnet are empty or malformed.
func BuildMinimalAllowedIPsForEndpointPeer(topo *Topology, masterOverlayIP, transportSubnet string) ([]string, error) {
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

	out := []string{transportSubnet, masterOverlayIP + "/32"}

	// Append the topology "clients" range so endpoint master-peer AllowedIPs
	// permit client→endpoint INBOUND and endpoint→client OUTBOUND traffic.
	// Identified by name (case-insensitive) consistent with the "endpoints"
	// range lookup in BuildAllowedIPsForMasterPeer.
	if topo != nil {
		for _, r := range topo.Overlay.Ranges {
			if !strings.EqualFold(strings.TrimSpace(r.Name), "clients") {
				continue
			}
			cidr := strings.TrimSpace(r.CIDR)
			if cidr == "" {
				continue
			}
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return nil, fmt.Errorf("invalid clients overlay range CIDR %q in range %q: %w", r.CIDR, r.Name, err)
			}
			out = append(out, cidr)
		}
	}

	return out, nil
}

// BuildAllowedIPsForMasterPeer returns the AllowedIPs a master should install for
// one endpoint peer: transport /30, the endpoint overlay /32, and either the
// topology "endpoints" range or explicit overlay /32s for the other endpoints.
//
// The topology currently identifies the endpoints range by name ("endpoints"),
// not by a dedicated kind field.
func BuildAllowedIPsForMasterPeer(topo *Topology, endpointName, endpointOverlayIP, transportSubnet string) ([]string, error) {
	if endpointOverlayIP == "" {
		return nil, fmt.Errorf("endpoint overlay IP is required")
	}
	if transportSubnet == "" {
		return nil, fmt.Errorf("transport subnet is required")
	}

	_, transport, err := net.ParseCIDR(strings.TrimSpace(transportSubnet))
	if err != nil {
		return nil, fmt.Errorf("invalid transport subnet %q: %w", transportSubnet, err)
	}

	// Normalize endpointOverlayIP to a host /32 CIDR. When the caller passes a
	// CIDR-form value (e.g. "172.20.70.34/27"), net.ParseCIDR would widen it to
	// the network prefix ("172.20.70.32/27"), which would install an overly broad
	// AllowedIPs entry. Extract the host address and force /32 instead.
	overlayCIDR := strings.TrimSpace(endpointOverlayIP)
	if strings.Contains(overlayCIDR, "/") {
		hostIP, _, parseErr := net.ParseCIDR(overlayCIDR)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid endpoint overlay IP %q: %w", endpointOverlayIP, parseErr)
		}
		overlayCIDR = hostIP.String() + "/32"
	} else {
		overlayCIDR += "/32"
	}
	_, overlay, err := net.ParseCIDR(overlayCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint overlay IP %q: %w", endpointOverlayIP, err)
	}

	out := []string{transport.String(), overlay.String()}
	if topo == nil {
		return out, nil
	}

	for _, namedRange := range topo.Overlay.Ranges {
		if !strings.EqualFold(strings.TrimSpace(namedRange.Name), "endpoints") {
			continue
		}
		if strings.TrimSpace(namedRange.CIDR) == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(namedRange.CIDR); err != nil {
			return nil, fmt.Errorf("invalid endpoints range CIDR %q: %w", namedRange.CIDR, err)
		}
		return dedup(append(out, namedRange.CIDR)), nil
	}

	selfName := strings.TrimSpace(endpointName)
	selfOverlay := overlay.String()
	for _, endpoint := range topo.Endpoints {
		if selfName != "" && endpoint.Name == selfName {
			continue
		}
		peerOverlayCIDR := strings.TrimSpace(endpoint.OverlayIP)
		if peerOverlayCIDR == "" {
			continue
		}
		// Same host-extraction normalization as above: CIDR-form inputs must be
		// reduced to a host /32 to avoid broadening to the network prefix.
		if strings.Contains(peerOverlayCIDR, "/") {
			hostIP, _, parseErr := net.ParseCIDR(peerOverlayCIDR)
			if parseErr != nil {
				return nil, fmt.Errorf("invalid endpoint overlay IP %q for endpoint %q: %w", endpoint.OverlayIP, endpoint.Name, parseErr)
			}
			peerOverlayCIDR = hostIP.String() + "/32"
		} else {
			peerOverlayCIDR += "/32"
		}
		_, peerOverlay, err := net.ParseCIDR(peerOverlayCIDR)
		if err != nil {
			return nil, fmt.Errorf("invalid endpoint overlay IP %q for endpoint %q: %w", endpoint.OverlayIP, endpoint.Name, err)
		}
		if peerOverlay.String() == selfOverlay {
			continue
		}
		out = append(out, peerOverlay.String())
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
