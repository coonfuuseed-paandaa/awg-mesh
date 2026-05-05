package clientd

import (
	"context"
	"errors"
	"fmt"
	"time"

	pb "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto/control_plane"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NATStatus describes whether control-plane signal relay is currently usable.
type NATStatus string

const (
	NATStatusAvailable   NATStatus = "available"
	NATStatusDisabled    NATStatus = "disabled"
	NATStatusUnavailable NATStatus = "unavailable"

	defaultNATProbeTimeout = 1 * time.Second
)

// NATResult reports the signal-relay capability status.
type NATResult struct {
	Status NATStatus
	Reason string
}

// NATTraverser opens the control-plane NAT traversal capability seam.
type NATTraverser interface {
	Probe(ctx context.Context) (NATResult, error)
}

// ControlPlaneNATTraverser probes SignalExchange on the control plane.
type ControlPlaneNATTraverser struct {
	Client pb.ControlPlaneClient
}

// Probe treats Unimplemented as a disabled non-fatal capability.
func (t ControlPlaneNATTraverser) Probe(ctx context.Context) (NATResult, error) {
	if t.Client == nil {
		return NATResult{}, errors.New("control-plane client is required")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultNATProbeTimeout)
		defer cancel()
	}
	stream, err := t.Client.SignalExchange(ctx)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return NATResult{Status: NATStatusDisabled, Reason: err.Error()}, nil
		}
		if status.Code(err) == codes.Unavailable {
			return NATResult{Status: NATStatusUnavailable, Reason: err.Error()}, nil
		}
		return NATResult{}, fmt.Errorf("open signal exchange: %w", err)
	}
	_ = stream.CloseSend()
	_, recvErr := stream.Recv()
	if recvErr != nil {
		if status.Code(recvErr) == codes.Unimplemented {
			return NATResult{Status: NATStatusDisabled, Reason: recvErr.Error()}, nil
		}
		if status.Code(recvErr) == codes.Unavailable {
			return NATResult{Status: NATStatusUnavailable, Reason: recvErr.Error()}, nil
		}
		return NATResult{}, fmt.Errorf("receive signal exchange probe: %w", recvErr)
	}
	return NATResult{Status: NATStatusAvailable}, nil
}
