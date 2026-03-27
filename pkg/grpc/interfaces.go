package grpcserver

import (
	"time"

	"github.com/thebtf/awg-mesh/pkg/wg"
)

// TunnelManager handles tunnel lifecycle operations.
type TunnelManager interface {
	AddTunnel(name, endpointHost, overlayIP, balancerIP string, weight int, peerPublicKey wg.Key) error
	RemoveTunnel(name string) error
}

// ParamApplier applies AWG runtime parameter updates to an interface/tunnel.
type ParamApplier interface {
	ApplyParams(tunnelName string, cfg wg.Config) error
}

// CaptureFunc performs capture using a network interface and returns how many
// packets were captured.
type CaptureFunc func(interfaceName string, domains []string, countPerDomain int, timeout time.Duration) (int, error)
