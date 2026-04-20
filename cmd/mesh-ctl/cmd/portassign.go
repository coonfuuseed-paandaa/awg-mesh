package cmd

import (
	"fmt"
	"sort"

	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"github.com/rs/zerolog"
)

// portOffset returns the 0-based alphabetical index of targetMaster within
// allMasterNames. Index 0 corresponds to the base listen port (no shift).
// When targetMaster is not found (should not happen if caller validates input),
// 0 is returned as a safe default.
func portOffset(allMasterNames []string, targetMaster string) int {
	sorted := append([]string(nil), allMasterNames...)
	sort.Strings(sorted)
	for i, name := range sorted {
		if name == targetMaster {
			return i
		}
	}
	return 0
}

// computePeerEndpoint builds the "<host>:<port>" string to use as EndpointHost
// in AddTunnel. It prefers the port returned by the endpoint's AddPeer response
// (resp.GetEndpointListenPort()). When that field is 0 (pre-v1.12.3 endpoint or
// error), it falls back to basePort + portOffset(allMasters, thisMaster) and
// logs a warning so the operator knows the fallback fired.
func computePeerEndpoint(
	host string,
	basePort int,
	resp *proto.AddPeerResponse,
	allMasters []string,
	thisMaster string,
	logger zerolog.Logger,
) string {
	port := int(resp.GetEndpointListenPort())
	if port == 0 {
		port = basePort + portOffset(allMasters, thisMaster)
		logger.Warn().
			Str("master", thisMaster).
			Int("fallback_port", port).
			Msg("endpoint did not return listen_port; using topology port + offset fallback")
	}
	return fmt.Sprintf("%s:%d", host, port)
}
