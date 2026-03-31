package node

import "github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"

const (
	// MinMTU is the minimum allowed MTU (IPv6 minimum).
	MinMTU = 1280
	// defaultPhysicalMTU is used when topology doesn't specify physical_mtu.
	defaultPhysicalMTU = 1500
	// defaultAWGOverhead is used when topology doesn't specify awg_overhead.
	defaultAWGOverhead = 80
)

// CalculateMTU computes the effective overlay MTU given the physical MTU,
// AWG tunnel overhead per hop, and the number of hops (1 = direct, 2 = relayed).
// The result is clamped to a minimum of MinMTU.
func CalculateMTU(physicalMTU int, awgOverhead int, hops int) int {
	result := physicalMTU - (awgOverhead * hops)
	if result < MinMTU {
		return MinMTU
	}
	return result
}

// calculateMTUFromTopology reads physical_mtu and awg_overhead from topology,
// falling back to defaults (1500/80) if not set or topology is nil.
func calculateMTUFromTopology(topo *topology.Topology, hops int) int {
	physicalMTU := defaultPhysicalMTU
	awgOverhead := defaultAWGOverhead
	if topo != nil {
		if topo.Overlay.PhysicalMTU > 0 {
			physicalMTU = topo.Overlay.PhysicalMTU
		}
		if topo.Overlay.AWGOverhead > 0 {
			awgOverhead = topo.Overlay.AWGOverhead
		}
	}
	return CalculateMTU(physicalMTU, awgOverhead, hops)
}
