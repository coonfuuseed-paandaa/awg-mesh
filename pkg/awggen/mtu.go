package awggen

import "fmt"

// ValidateMTU validates whether obfuscation overhead fits the physical MTU.
func ValidateMTU(params *Params, physicalMTU, awgOverhead int) error {
	if params == nil {
		return fmt.Errorf("params is required")
	}
	if physicalMTU <= 0 {
		return fmt.Errorf("physicalMTU must be > 0, got %d", physicalMTU)
	}
	if awgOverhead < 0 {
		return fmt.Errorf("awgOverhead must be >= 0, got %d", awgOverhead)
	}
	if params.S3 < 0 || params.S4 < 0 {
		return fmt.Errorf("S3 and S4 must be >= 0, got S3=%d S4=%d", params.S3, params.S4)
	}

	if params.S3+params.S4+awgOverhead > physicalMTU {
		return fmt.Errorf(
			"obfuscation overhead exceeds MTU: S3(%d)+S4(%d)+overhead(%d) > physicalMTU(%d)",
			params.S3,
			params.S4,
			awgOverhead,
			physicalMTU,
		)
	}

	if params.S3+64+awgOverhead > physicalMTU {
		return fmt.Errorf(
			"cookie reply exceeds MTU: S3(%d)+64+overhead(%d) > physicalMTU(%d)",
			params.S3,
			awgOverhead,
			physicalMTU,
		)
	}

	return nil
}

// EffectiveMTU calculates usable payload MTU after AWG overhead.
func EffectiveMTU(physicalMTU, awgOverhead int) int {
	return physicalMTU - awgOverhead
}

// MaxS4ForMTU returns the largest S4 value that keeps cookie reply packets within MTU.
func MaxS4ForMTU(physicalMTU, awgOverhead int) int {
	return EffectiveMTU(physicalMTU, awgOverhead) - 64
}
