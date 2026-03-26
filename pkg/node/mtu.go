package node

// MinMTU is the minimum allowed MTU (IPv6 minimum).
const MinMTU = 1280

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
