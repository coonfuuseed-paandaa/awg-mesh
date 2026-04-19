//go:build linux

package node

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/routing"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/rs/zerolog"
)

// overlayRouterFn is the test seam for the LinkRouter factory used by
// rebuildAllOverlayRoutes. Unit tests replace this to capture route calls
// without kernel access.
var overlayRouterFn = func() LinkRouter {
	return routing.NewNetlinkRouter()
}

// LinkRouter is the subset of routing.Router required by the overlay route
// functions in this file. The production implementation is routing.NetlinkRouter;
// unit tests substitute a lightweight mock.
type LinkRouter interface {
	// RouteReplaceLink installs (or replaces) a scope=link route to dest via dev.
	// Idempotent: running it twice produces the same kernel state.
	RouteReplaceLink(dest *net.IPNet, dev string) error
	// RouteDelete removes the route to dest. May return an error if the route
	// does not exist; callers should treat that as non-fatal.
	RouteDelete(dest *net.IPNet) error
}

// overlayIfaceName returns the kernel interface name for a given master name.
// Mirrors the truncation logic in createMasterInterface.
func overlayIfaceName(masterName string) string {
	part := masterName
	if len(part) > 12 {
		part = part[:12]
	}
	return "wg-" + part
}

// peersForMaster returns the names of endpoints bound to masterName (other than self),
// along with their OverlayIP strings. Result is sorted by endpoint name for
// determinism. Endpoints whose OverlayIP is empty are silently skipped.
func peersForMaster(topo *topology.Topology, selfEndpointName, masterName string) []topology.EndpointNode {
	master := topo.FindMaster(masterName)
	if master == nil {
		return nil
	}

	// Build a quick-lookup set of endpoints bound to this master.
	boundSet := make(map[string]struct{}, len(master.Endpoints))
	for _, ep := range master.Endpoints {
		boundSet[ep] = struct{}{}
	}

	result := make([]topology.EndpointNode, 0)
	for _, ep := range topo.Endpoints {
		if ep.Name == selfEndpointName {
			continue
		}
		if _, bound := boundSet[ep.Name]; !bound {
			continue
		}
		if strings.TrimSpace(ep.OverlayIP) == "" {
			continue
		}
		result = append(result, ep)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

// chosenMasterForPeer returns the first master name (alphabetically) that binds
// both selfEndpointName and peerEndpointName. Returns "" when no shared master exists.
// v1.12.2 uses a single master per pair (no ECMP); callers must skip if "".
func chosenMasterForPeer(topo *topology.Topology, selfEndpointName, peerEndpointName string) string {
	candidates := make([]string, 0)
	for _, master := range topo.Masters {
		selfBound := false
		peerBound := false
		for _, ep := range master.Endpoints {
			if ep == selfEndpointName {
				selfBound = true
			}
			if ep == peerEndpointName {
				peerBound = true
			}
		}
		if selfBound && peerBound {
			candidates = append(candidates, master.Name)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Strings(candidates)
	return candidates[0]
}

// parseOverlayHostIP parses overlayIP (which may be "A.B.C.D" or "A.B.C.D/N")
// and returns just the host IP as a /32 network, suitable for a kernel route.
func parseOverlayHostIP(overlayIP string) (*net.IPNet, error) {
	trimmed := strings.TrimSpace(overlayIP)
	if trimmed == "" {
		return nil, fmt.Errorf("overlay IP is empty")
	}
	// Strip any existing prefix to normalise to host address.
	if strings.Contains(trimmed, "/") {
		ip, _, err := net.ParseCIDR(trimmed)
		if err != nil {
			return nil, fmt.Errorf("parse overlay IP CIDR %q: %w", overlayIP, err)
		}
		trimmed = ip.String()
	}
	_, ipNet, err := net.ParseCIDR(trimmed + "/32")
	if err != nil {
		return nil, fmt.Errorf("build /32 for overlay IP %q: %w", overlayIP, err)
	}
	return ipNet, nil
}

// installOverlayRoutesForMaster installs kernel routes that let this endpoint
// reach other endpoints via the wg-<masterName> interface. Called after each
// wg-<masterName> iface is created.
//
// Strategy: for each endpoint EB in the topology that shares masterName with
// self (EA), and for which masterName is the first-alphabetically shared master,
// install: ip route add <EB.OverlayIP>/32 dev wg-<masterName>
// Uses RouteReplaceLink for idempotency — running twice is safe.
func installOverlayRoutesForMaster(
	topo *topology.Topology,
	selfEndpointName string,
	masterName string,
	ifaceName string,
	router LinkRouter,
	logger zerolog.Logger,
) error {
	if topo == nil {
		return fmt.Errorf("topology is required")
	}
	peers := peersForMaster(topo, selfEndpointName, masterName)
	var firstErr error
	for _, peer := range peers {
		// Only install the route here if this master is the chosen (alphabetically
		// first) master for the self↔peer pair. This prevents installing the same
		// /32 route on multiple interfaces.
		chosen := chosenMasterForPeer(topo, selfEndpointName, peer.Name)
		if chosen != masterName {
			continue
		}

		dest, err := parseOverlayHostIP(peer.OverlayIP)
		if err != nil {
			logger.Warn().
				Err(err).
				Str("peer", peer.Name).
				Str("master", masterName).
				Msg("endpoint overlay route: skip peer with unparseable overlay IP")
			continue
		}

		if err := router.RouteReplaceLink(dest, ifaceName); err != nil {
			logger.Warn().
				Err(err).
				Str("dest", dest.String()).
				Str("dev", ifaceName).
				Str("peer", peer.Name).
				Str("master", masterName).
				Msg("endpoint overlay route: install failed")
			if firstErr == nil {
				firstErr = fmt.Errorf("install route %s dev %s: %w", dest, ifaceName, err)
			}
			continue
		}

		logger.Debug().
			Str("dest", dest.String()).
			Str("dev", ifaceName).
			Str("peer", peer.Name).
			Str("master", masterName).
			Msg("endpoint overlay route installed")
	}
	return firstErr
}

// removeOverlayRoutesForMaster removes kernel routes installed by
// installOverlayRoutesForMaster when a master iface is torn down.
// Errors are collected and the first is returned; removal continues past
// individual failures so all routes are attempted.
func removeOverlayRoutesForMaster(
	topo *topology.Topology,
	selfEndpointName string,
	masterName string,
	router LinkRouter,
	logger zerolog.Logger,
) error {
	if topo == nil {
		return fmt.Errorf("topology is required")
	}
	peers := peersForMaster(topo, selfEndpointName, masterName)
	var firstErr error
	for _, peer := range peers {
		chosen := chosenMasterForPeer(topo, selfEndpointName, peer.Name)
		if chosen != masterName {
			continue
		}

		dest, err := parseOverlayHostIP(peer.OverlayIP)
		if err != nil {
			logger.Warn().
				Err(err).
				Str("peer", peer.Name).
				Str("master", masterName).
				Msg("endpoint overlay route: skip remove for peer with unparseable overlay IP")
			continue
		}

		if err := router.RouteDelete(dest); err != nil {
			logger.Warn().
				Err(err).
				Str("dest", dest.String()).
				Str("peer", peer.Name).
				Str("master", masterName).
				Msg("endpoint overlay route: remove failed (may already be gone)")
			if firstErr == nil {
				firstErr = fmt.Errorf("remove route %s (master %s): %w", dest, masterName, err)
			}
		} else {
			logger.Debug().
				Str("dest", dest.String()).
				Str("peer", peer.Name).
				Str("master", masterName).
				Msg("endpoint overlay route removed")
		}
	}
	return firstErr
}

// rebuildAllOverlayRoutes reconciles all overlay routes for all currently active
// per-master ifaces. Called after the full startup reconcile loop completes.
// Idempotent: uses RouteReplaceLink internally.
func rebuildAllOverlayRoutes(e *EndpointRunner, topo *topology.Topology) error {
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}
	if topo == nil {
		return fmt.Errorf("topology is required")
	}

	selfName := e.node.config.Name
	router := routing.NewNetlinkRouter()
	logger := e.node.logger

	masterNames := e.listIfaces()
	var firstErr error
	for _, masterName := range masterNames {
		ifaceName := overlayIfaceName(masterName)
		if err := installOverlayRoutesForMaster(topo, selfName, masterName, ifaceName, router, logger); err != nil {
			logger.Warn().
				Err(err).
				Str("master", masterName).
				Msg("rebuildAllOverlayRoutes: partial failure")
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
