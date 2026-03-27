//go:build !linux

package node

import grpcserver "github.com/thebtf/awg-mesh/pkg/grpc"

func newCaptureFunc() grpcserver.CaptureFunc {
	return nil
}
