//go:build linux

package node

import (
	"fmt"
	"time"

	"github.com/thebtf/awg-mesh/pkg/awggen"
	grpcserver "github.com/thebtf/awg-mesh/pkg/grpc"
)

const captureInterfaceName = "any"

func newCaptureFunc() grpcserver.CaptureFunc {
	return func(interfaceName string, domains []string, countPerDomain int, timeout time.Duration) (int, error) {
		if countPerDomain <= 0 {
			countPerDomain = 3
		}
		if timeout <= 0 {
			timeout = 15 * time.Second
		}

		cfg := awggen.CaptureConfig{
			Interface:      normalizeCaptureInterface(interfaceName),
			Domains:        append([]string(nil), domains...),
			CountPerDomain: countPerDomain,
			Timeout:        timeout,
		}

		result, err := awggen.Capture(cfg)
		if err != nil {
			return 0, fmt.Errorf("capture: %w", err)
		}
		return len(result), nil
	}
}

func normalizeCaptureInterface(interfaceName string) string {
	if interfaceName == "" {
		return captureInterfaceName
	}
	return interfaceName
}
