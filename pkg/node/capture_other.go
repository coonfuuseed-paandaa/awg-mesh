//go:build !linux

package node

import grpcserver "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"

func newCaptureFunc() grpcserver.CaptureFunc {
	return nil
}
