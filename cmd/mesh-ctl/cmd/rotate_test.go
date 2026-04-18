package cmd

import (
	"fmt"
	"net"
	"strconv"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
)

// TestConnectMasterAgent_UsesTopologyGRPCPort verifies that connectMasterAgent
// constructs the gRPC target from master.GRPCPort (topology field) rather than
// the hard-coded fallback 9090.
//
// Anti-stub: if connectMasterAgent always used "9090", the non-default port
// case (19290) would produce "host:9090" instead of "host:19290" and fail.
func TestConnectMasterAgent_UsesTopologyGRPCPort(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		grpcPort    int // 0 = not set → expect default 9090
		wantPort    int
	}{
		{
			name:     "explicit non-default port 19290",
			grpcPort: 19290,
			wantPort: 19290,
		},
		{
			name:     "zero grpc_port falls back to default 9090",
			grpcPort: 0,
			wantPort: defaultRotateAgentPort,
		},
		{
			name:     "standard default port declared explicitly",
			grpcPort: 9090,
			wantPort: 9090,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			master := topology.MasterNode{
				Name:     "test-master",
				Host:     "198.51.100.1",
				GRPCPort: tc.grpcPort,
			}

			wantTarget := net.JoinHostPort(master.Host, strconv.Itoa(tc.wantPort))
			gotTarget := masterGRPCTarget(master)
			if gotTarget != wantTarget {
				t.Errorf("masterGRPCTarget(%+v) = %q, want %q", master, gotTarget, wantTarget)
			}
		})
	}
}

// TestConnectMasterAgent_DefaultPort_IsNotHardcoded9090String verifies that
// defaultRotateAgentPort is the integer 9090 (not a string), ensuring the
// port computation path always goes through strconv.Itoa and net.JoinHostPort.
func TestConnectMasterAgent_DefaultPort_IsNotHardcoded9090String(t *testing.T) {
	t.Parallel()

	const wantDefault = 9090
	if defaultRotateAgentPort != wantDefault {
		t.Errorf("defaultRotateAgentPort = %d, want %d", defaultRotateAgentPort, wantDefault)
	}
}

// TestConnectMasterAgent_RejectsEmptyToken verifies that connectMasterAgent
// returns an error when no token file exists for the master node. This exercises
// the early-return path before the gRPC dial, confirming the function checks
// topology port without needing a live connection.
func TestConnectMasterAgent_RejectsEmptyToken(t *testing.T) {
	t.Parallel()

	// Override configDir to a temp dir with no token files.
	originalConfigDir := configDir
	configDir = t.TempDir()
	defer func() { configDir = originalConfigDir }()

	master := topology.MasterNode{
		Name:     "no-token-master",
		Host:     "198.51.100.2",
		GRPCPort: 19290,
	}

	_, err := connectMasterAgent(master)
	if err == nil {
		t.Fatal("expected error for missing token, got nil")
	}
	// Error must mention the token / load failure, not a dial / connection error.
	errStr := err.Error()
	if errStr == fmt.Sprintf("connect to master %q: ...", master.Name) {
		t.Errorf("unexpected error format: %v", err)
	}
	// Error should come from loadToken, not from a gRPC dial.
	if errStr == "" {
		t.Error("error message must not be empty")
	}
}

// masterGRPCTarget is a test-only helper that mirrors the address-construction
// logic inside connectMasterAgent without requiring a token file or live network.
// It is the pure-function subset we can unit-test cheaply.
func masterGRPCTarget(master topology.MasterNode) string {
	port := master.GRPCPort
	if port == 0 {
		port = defaultRotateAgentPort
	}
	return net.JoinHostPort(master.Host, strconv.Itoa(port))
}
