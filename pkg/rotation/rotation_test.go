package rotation

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/awggen"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"google.golang.org/grpc"
)

type mockAwgAgentClient struct {
	mu           sync.Mutex
	rotateCalls  int
	healthCalls  int
	rotateReqs   []*proto.RotateParamsRequest
	rotateErr    error
	healthErr    error
	healthResp   *proto.HealthResponse
	rotateResp   *proto.RotateParamsResponse
}

func (m *mockAwgAgentClient) Init(_ context.Context, _ *proto.InitRequest, _ ...grpc.CallOption) (*proto.InitResponse, error) {
	return &proto.InitResponse{Success: true}, nil
}

func (m *mockAwgAgentClient) RotateToken(_ context.Context, _ *proto.RotateTokenRequest, _ ...grpc.CallOption) (*proto.RotateTokenResponse, error) {
	return &proto.RotateTokenResponse{Success: true}, nil
}

func (m *mockAwgAgentClient) AddTunnel(_ context.Context, _ *proto.AddTunnelRequest, _ ...grpc.CallOption) (*proto.AddTunnelResponse, error) {
	return &proto.AddTunnelResponse{Success: true}, nil
}

func (m *mockAwgAgentClient) RemoveTunnel(_ context.Context, _ *proto.RemoveTunnelRequest, _ ...grpc.CallOption) (*proto.RemoveTunnelResponse, error) {
	return &proto.RemoveTunnelResponse{Success: true}, nil
}

func (m *mockAwgAgentClient) ListTunnels(_ context.Context, _ *proto.Empty, _ ...grpc.CallOption) (*proto.TunnelList, error) {
	return &proto.TunnelList{}, nil
}

func (m *mockAwgAgentClient) AddPeer(_ context.Context, _ *proto.AddPeerRequest, _ ...grpc.CallOption) (*proto.AddPeerResponse, error) {
	return &proto.AddPeerResponse{Success: true}, nil
}

func (m *mockAwgAgentClient) RemovePeer(_ context.Context, _ *proto.RemovePeerRequest, _ ...grpc.CallOption) (*proto.RemovePeerResponse, error) {
	return &proto.RemovePeerResponse{Success: true}, nil
}

func (m *mockAwgAgentClient) ListPeers(_ context.Context, _ *proto.Empty, _ ...grpc.CallOption) (*proto.PeerList, error) {
	return &proto.PeerList{}, nil
}

func (m *mockAwgAgentClient) GetParams(_ context.Context, _ *proto.GetParamsRequest, _ ...grpc.CallOption) (*proto.AwgParams, error) {
	return &proto.AwgParams{}, nil
}

func (m *mockAwgAgentClient) CaptureRefresh(_ context.Context, _ *proto.CaptureRequest, _ ...grpc.CallOption) (*proto.CaptureResponse, error) {
	return &proto.CaptureResponse{Success: true}, nil
}

func (m *mockAwgAgentClient) GetStatus(_ context.Context, _ *proto.Empty, _ ...grpc.CallOption) (*proto.NodeStatus, error) {
	return &proto.NodeStatus{}, nil
}

func (m *mockAwgAgentClient) GetRoutes(_ context.Context, _ *proto.Empty, _ ...grpc.CallOption) (*proto.RouteTable, error) {
	return &proto.RouteTable{}, nil
}

func (m *mockAwgAgentClient) RotateParams(_ context.Context, req *proto.RotateParamsRequest, _ ...grpc.CallOption) (*proto.RotateParamsResponse, error) {
	m.mu.Lock()
	m.rotateCalls++
	m.rotateReqs = append(m.rotateReqs, req)
	m.mu.Unlock()

	if m.rotateErr != nil {
		return nil, m.rotateErr
	}
	if m.rotateResp == nil {
		return &proto.RotateParamsResponse{Success: true}, nil
	}
	return m.rotateResp, nil
}

func (m *mockAwgAgentClient) GetHealth(_ context.Context, _ *proto.Empty, _ ...grpc.CallOption) (*proto.HealthResponse, error) {
	m.mu.Lock()
	m.healthCalls++
	m.mu.Unlock()

	if m.healthErr != nil {
		return nil, m.healthErr
	}
	return m.healthResp, nil
}

func TestTier1ExecuteValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ctx      context.Context
		client   proto.AwgAgentClient
		tunnel   string
		params   *awggen.Params
		contains string
	}{
		{
			name:     "nil context",
			client:   &mockAwgAgentClient{},
			tunnel:   "tunnel-a",
			params:   &awggen.Params{},
			contains: "context is required",
		},
		{
			name:     "nil client",
			ctx:      context.Background(),
			tunnel:   "tunnel-a",
			params:   &awggen.Params{},
			contains: "client is required",
		},
		{
			name:     "empty tunnel",
			ctx:      context.Background(),
			client:   &mockAwgAgentClient{},
			params:   &awggen.Params{},
			contains: "tunnel name is required",
		},
		{
			name:     "nil params",
			ctx:      context.Background(),
			client:   &mockAwgAgentClient{},
			tunnel:   "tunnel-a",
			contains: "params are required",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := NewTier1Rotation().Execute(tc.ctx, tc.client, tc.tunnel, tc.params)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected error containing %q, got %v", tc.contains, err)
			}
		})
	}
}

func TestTier1ExecuteBuildsExpectedRotateRequest(t *testing.T) {
	t.Parallel()

	client := &mockAwgAgentClient{
		rotateResp: &proto.RotateParamsResponse{
			Success: true,
		},
	}
	params := &awggen.Params{
		Jc:   11,
		Jmin: 12,
		Jmax: 13,
		I1:   "i1",
		I2:   "i2",
		I3:   "i3",
		I4:   "i4",
		I5:   "i5",
		S1:   100,
		S2:   200,
	}

	err := NewTier1Rotation().Execute(context.Background(), client, "tier1-tunnel", params)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if client.rotateCalls != 1 {
		t.Fatalf("expected 1 rotate call, got %d", client.rotateCalls)
	}

	req := client.rotateReqs[0]
	if req.GetTier() != 1 {
		t.Fatalf("expected tier 1, got %d", req.GetTier())
	}
	if req.GetTunnelName() != "tier1-tunnel" {
		t.Fatalf("unexpected tunnel name %q", req.GetTunnelName())
	}
	if req.GetNewParams().GetJc() != 11 || req.GetNewParams().GetJmin() != 12 || req.GetNewParams().GetJmax() != 13 {
		t.Fatalf("unexpected J parameters in request: %+v", req.GetNewParams())
	}
	if req.GetNewParams().GetS1() != 0 || req.GetNewParams().GetH1() != 0 {
		t.Fatalf("expected tier1 request only J and I fields, got %+v", req.GetNewParams())
	}
	if req.GetNewParams().GetI1() != "i1" || req.GetNewParams().GetI5() != "i5" {
		t.Fatalf("unexpected i-values in request: %+v", req.GetNewParams())
	}
}

func TestTier2PreflightFailsWhenAnyClientUnhealthy(t *testing.T) {
	t.Parallel()

	unhealthyClient := &mockAwgAgentClient{
		healthResp: &proto.HealthResponse{Healthy: false},
	}
	healthyClient := &mockAwgAgentClient{
		healthResp: &proto.HealthResponse{Healthy: true},
	}
	clients := map[string]proto.AwgAgentClient{
		"healthy":   healthyClient,
		"unhealthy": unhealthyClient,
	}

	err := NewTier2Rotation().Execute(context.Background(), clients, "tier2-tunnel", &awggen.Params{
		S1: 1,
	})
	if err == nil {
		t.Fatal("expected preflight failure")
	}
	if !strings.Contains(err.Error(), "preflight failed") {
		t.Fatalf("unexpected preflight error: %v", err)
	}
	if unhealthyClient.rotateCalls != 0 || healthyClient.rotateCalls != 0 {
		t.Fatalf("expected no rotate calls when preflight fails, healthy=%d unhealthy=%d", healthyClient.rotateCalls, unhealthyClient.rotateCalls)
	}
	if unhealthyClient.healthCalls != 1 || healthyClient.healthCalls != 1 {
		t.Fatalf("expected one health check per client, healthy=%d unhealthy=%d", healthyClient.healthCalls, unhealthyClient.healthCalls)
	}
}

func TestTier2ExecuteBuildsExpectedRotateRequest(t *testing.T) {
	t.Parallel()

	client := &mockAwgAgentClient{
		healthResp: &proto.HealthResponse{Healthy: true},
		rotateResp: &proto.RotateParamsResponse{Success: true},
	}
	params := &awggen.Params{
		Jc: 7,
		S1: 21,
		S2: 22,
		S3: 23,
		S4: 24,
		H1: 31,
		H2: 32,
		H3: 33,
		H4: 34,
	}

	err := NewTier2Rotation().Execute(context.Background(), map[string]proto.AwgAgentClient{"master": client}, "tier2-tunnel", params)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if client.rotateCalls != 1 {
		t.Fatalf("expected 1 rotate call, got %d", client.rotateCalls)
	}
	if client.rotateReqs[0].GetTier() != 2 {
		t.Fatalf("expected tier 2, got %d", client.rotateReqs[0].GetTier())
	}
	if client.rotateReqs[0].GetNewParams().GetS1() != 21 || client.rotateReqs[0].GetNewParams().GetS4() != 24 {
		t.Fatalf("unexpected S values: %+v", client.rotateReqs[0].GetNewParams())
	}
	if client.rotateReqs[0].GetNewParams().GetH1() != 31 || client.rotateReqs[0].GetNewParams().GetH4() != 34 {
		t.Fatalf("unexpected H values: %+v", client.rotateReqs[0].GetNewParams())
	}
	if client.rotateReqs[0].GetNewParams().GetJc() != 0 {
		t.Fatalf("expected Jc to remain default when tier 2, got %d", client.rotateReqs[0].GetNewParams().GetJc())
	}
}

func TestTier3ValidationZeroPublicKey(t *testing.T) {
	t.Parallel()

	err := NewTier3Rotation().Execute(context.Background(), &mockAwgAgentClient{}, "tier3-tunnel", &awggen.Params{}, wg.Key{})
	if err == nil {
		t.Fatal("expected error for zero public key")
	}
	if !strings.Contains(err.Error(), "new public key must not be zero") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTier3ExecuteBuildsExpectedRotateRequest(t *testing.T) {
	t.Parallel()

	client := &mockAwgAgentClient{
		rotateResp: &proto.RotateParamsResponse{Success: true},
	}
	params := &awggen.Params{
		Jc:   101,
		Jmin: 102,
		Jmax: 103,
		S1:   11,
		S2:   12,
		S3:   13,
		S4:   14,
		H1:   15,
		H2:   16,
		H3:   17,
		H4:   18,
		I1:   "one",
		I2:   "two",
		I3:   "three",
		I4:   "four",
		I5:   "five",
	}
	var key wg.Key
	for i := range key {
		key[i] = byte(i + 1)
	}

	err := NewTier3Rotation().Execute(context.Background(), client, "tier3-tunnel", params, key)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if client.rotateCalls != 1 {
		t.Fatalf("expected 1 rotate call, got %d", client.rotateCalls)
	}

	req := client.rotateReqs[0]
	if req.GetTier() != 3 {
		t.Fatalf("expected tier 3, got %d", req.GetTier())
	}
	if req.GetTunnelName() != "tier3-tunnel" {
		t.Fatalf("unexpected tunnel name %q", req.GetTunnelName())
	}
	if !bytes.Equal(req.GetNewPublicKey(), key[:]) {
		t.Fatalf("expected key bytes %v, got %v", []byte(key[:]), req.GetNewPublicKey())
	}
	expectedProto := params.ToProto()
	gotProto := req.GetNewParams()
	if gotProto == nil {
		t.Fatal("expected new params in request")
	}
	if !equalAwgParams(t, gotProto, expectedProto) {
		t.Fatalf("unexpected tier3 params: %+v", gotProto)
	}
}

func TestTier3ValidationNilParams(t *testing.T) {
	t.Parallel()

	client := &mockAwgAgentClient{}
	var key wg.Key
	for i := range key {
		key[i] = 1
	}

	err := NewTier3Rotation().Execute(context.Background(), client, "tier3-tunnel", nil, key)
	if err == nil {
		t.Fatal("expected error for nil params")
	}
	if !strings.Contains(err.Error(), "params are required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func equalAwgParams(t *testing.T, got, want *proto.AwgParams) bool {
	t.Helper()

	return got.GetJc() == want.GetJc() &&
		got.GetJmin() == want.GetJmin() &&
		got.GetJmax() == want.GetJmax() &&
		got.GetS1() == want.GetS1() &&
		got.GetS2() == want.GetS2() &&
		got.GetS3() == want.GetS3() &&
		got.GetS4() == want.GetS4() &&
		got.GetH1() == want.GetH1() &&
		got.GetH2() == want.GetH2() &&
		got.GetH3() == want.GetH3() &&
		got.GetH4() == want.GetH4() &&
		got.GetI1() == want.GetI1() &&
		got.GetI2() == want.GetI2() &&
		got.GetI3() == want.GetI3() &&
		got.GetI4() == want.GetI4() &&
		got.GetI5() == want.GetI5()
}

func TestNilContextPreventsExecution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		execution  func() error
		contains   string
	}{
		{
			name: "tier2 nil context",
			execution: func() error {
				//nolint:staticcheck // SA1012: intentionally passing nil to test nil-context guard
				return NewTier2Rotation().Execute(nil, map[string]proto.AwgAgentClient{"a": &mockAwgAgentClient{}}, "tunnel", &awggen.Params{})
			},
			contains: "context is required",
		},
		{
			name: "tier3 nil context",
			execution: func() error {
				var key wg.Key
				for i := range key {
					key[i] = 1
				}
				//nolint:staticcheck // SA1012: intentionally passing nil to test nil-context guard
				return NewTier3Rotation().Execute(nil, &mockAwgAgentClient{}, "tunnel", &awggen.Params{}, key)
			},
			contains: "context is required",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.execution()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected %q in error, got %v", tc.contains, err)
			}
			if !strings.Contains(err.Error(), "execute") {
				t.Fatalf("expected validation error prefix include execute, got %v", err)
			}
		})
	}
}
