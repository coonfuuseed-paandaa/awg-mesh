package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	controlplane "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/control_plane"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/rotation"
	controlpb "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto/control_plane"
)

func TestRunRotateCommandMeshWideSendsRequestAndPrintsRows(t *testing.T) {
	configDir := writeControlPlaneMTLSConfig(t)
	addr, client, teardown := startMeshWideRotateServer(t, configDir)
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	registerMeshWideRotateNode(t, client, ctx, "client-01", role.RoleClient, "172.21.92.130")
	registerMeshWideRotateNode(t, client, ctx, "master-01", role.RoleMaster, "172.21.92.2")
	registerMeshWideRotateNode(t, client, ctx, "egress-01", role.RoleEgress, "172.21.92.34")

	var out bytes.Buffer
	err := runRotateCommand(rotateOptions{
		tier:         1,
		preset:       defaultRotatePreset,
		meshWide:     true,
		controlPlane: addr,
		configDir:    configDir,
		applyDelay:   rotation.DefaultApplyLeadTime,
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runRotateCommand: %v", err)
	}

	text := out.String()
	if !strings.Contains(text, "NODE") || !strings.Contains(text, "ACK") || !strings.Contains(text, "ERROR") {
		t.Fatalf("missing ACK table header: %q", text)
	}
	if !strings.Contains(text, "egress-01") || !strings.Contains(text, "master-01") {
		t.Fatalf("missing mesh-internal ACK rows: %q", text)
	}
	if strings.Contains(text, "client-01") {
		t.Fatalf("client-only node must not receive mesh-wide AWG rotation: %q", text)
	}
}

func TestValidateRotateOptionsMeshWideBoundaries(t *testing.T) {
	tests := []struct {
		name string
		opts rotateOptions
		want string
	}{
		{
			name: "mesh-wide requires control plane",
			opts: rotateOptions{tier: 1, preset: defaultRotatePreset, meshWide: true, applyDelay: time.Second},
			want: "--control-plane is required",
		},
		{
			name: "mesh-wide rejects endpoint",
			opts: rotateOptions{tier: 1, endpoint: "endpoint-01", preset: defaultRotatePreset, meshWide: true, controlPlane: "127.0.0.1:1", applyDelay: time.Second},
			want: "--endpoint cannot be used",
		},
		{
			name: "mesh-wide requires positive apply delay",
			opts: rotateOptions{tier: 1, preset: defaultRotatePreset, meshWide: true, controlPlane: "127.0.0.1:1"},
			want: "--apply-delay",
		},
		{
			name: "legacy rejects control plane",
			opts: rotateOptions{tier: 1, endpoint: "endpoint-01", preset: defaultRotatePreset, controlPlane: "127.0.0.1:1"},
			want: "--control-plane requires --mesh-wide",
		},
		{
			name: "legacy still requires endpoint",
			opts: rotateOptions{tier: 1, preset: defaultRotatePreset},
			want: "--endpoint is required",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateRotateOptions(tt.opts)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want contains %q", err, tt.want)
			}
		})
	}

	legacy, err := validateRotateOptions(rotateOptions{tier: 1, endpoint: "endpoint-01", preset: defaultRotatePreset})
	if err != nil {
		t.Fatalf("legacy validate: %v", err)
	}
	if legacy.endpoint != "endpoint-01" || legacy.meshWide {
		t.Fatalf("unexpected legacy options: %+v", legacy)
	}

	meshWide, err := validateRotateOptions(rotateOptions{
		tier:         1,
		preset:       defaultRotatePreset,
		meshWide:     true,
		controlPlane: "127.0.0.1:9090",
		applyDelay:   rotation.DefaultApplyLeadTime,
	})
	if err != nil {
		t.Fatalf("mesh-wide validate: %v", err)
	}
	if !meshWide.meshWide || meshWide.controlPlane != "127.0.0.1:9090" {
		t.Fatalf("unexpected mesh-wide options: %+v", meshWide)
	}
}

func TestRotateCommandTimeoutMeshWideIncludesApplyDelay(t *testing.T) {
	legacy := rotateCommandTimeout(rotateOptions{})
	if legacy != rotateTimeout {
		t.Fatalf("legacy timeout = %s, want %s", legacy, rotateTimeout)
	}

	meshWide := rotateCommandTimeout(rotateOptions{meshWide: true, applyDelay: 45 * time.Second})
	want := rotateTimeout + 45*time.Second
	if meshWide != want {
		t.Fatalf("mesh-wide timeout = %s, want %s", meshWide, want)
	}
}

func startMeshWideRotateServer(t *testing.T, configDir string) (string, controlpb.ControlPlaneClient, func()) {
	t.Helper()
	srv := controlplane.NewServer(controlplane.NewRegistry(), controlplane.NewLedger(), controlplane.NewAuditLog(64))
	return startMTLSControlPlaneTestServer(t, configDir, srv)
}

func registerMeshWideRotateNode(t *testing.T, client controlpb.ControlPlaneClient, ctx context.Context, name string, nodeRole role.Role, overlayIP string) {
	t.Helper()
	resp, err := client.RegisterNode(ctx, &controlpb.RegisterNodeRequest{
		NodeName:    name,
		Roles:       []string{string(nodeRole)},
		NodeCertPem: []byte("-----BEGIN CERTIFICATE-----\nMIIfake\n-----END CERTIFICATE-----\n"),
		OverlayIp:   overlayIP,
	})
	if err != nil {
		t.Fatalf("RegisterNode %s: %v", name, err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("RegisterNode %s rejected: %s", name, resp.GetRejectReason())
	}
}
