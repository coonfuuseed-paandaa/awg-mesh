package node

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	meshnft "github.com/coonfuuseed-paandaa/awg-mesh/pkg/nftables"
)

type fakeEgressInstaller struct {
	calls []meshnft.MasqueradeConfig
	err   error
}

func (i *fakeEgressInstaller) Apply(ctx context.Context, cfg meshnft.MasqueradeConfig) (meshnft.MasqueradePlan, error) {
	if err := ctx.Err(); err != nil {
		return meshnft.MasqueradePlan{}, err
	}
	i.calls = append(i.calls, cfg)
	if i.err != nil {
		return meshnft.MasqueradePlan{}, i.err
	}
	return meshnft.Plan(cfg)
}

func TestNewEgressValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     EgressConfig
		wantErr string
	}{
		{name: "missing name", cfg: EgressConfig{OverlayIP: "172.21.92.20", InternetInterface: "eth0"}, wantErr: "name is required"},
		{name: "missing overlay", cfg: EgressConfig{Name: "egress-01", InternetInterface: "eth0"}, wantErr: "overlay IP is required"},
		{name: "bad overlay", cfg: EgressConfig{Name: "egress-01", OverlayIP: "not-ip", InternetInterface: "eth0"}, wantErr: "parse egress overlay IP"},
		{name: "missing internet iface", cfg: EgressConfig{Name: "egress-01", OverlayIP: "172.21.92.20"}, wantErr: "internet interface is required"},
		{name: "mesh iface", cfg: EgressConfig{Name: "egress-01", OverlayIP: "172.21.92.20", InternetInterface: "awg-mesh0"}, wantErr: "mesh interface"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewEgress(tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestEgressRunAppliesMasqueradeAndRunsAgent(t *testing.T) {
	t.Parallel()

	installer := &fakeEgressInstaller{}
	agentCalled := false
	egress, err := NewEgress(EgressConfig{
		Name:                "egress-01",
		OverlayIP:           "172.21.92.20",
		InternetInterface:   "eth0",
		MasqueradeInstaller: installer,
		AgentRunner: func(context.Context) error {
			agentCalled = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewEgress: %v", err)
	}
	if err := egress.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(installer.calls) != 1 || installer.calls[0].InternetInterface != "eth0" {
		t.Fatalf("installer calls = %#v, want eth0", installer.calls)
	}
	if !agentCalled {
		t.Fatal("agent runner was not called after NAT apply")
	}
	status := egress.Status()
	if !status.Started || status.Masquerade.InternetInterface != "eth0" {
		t.Fatalf("unexpected status after Run: %+v", status)
	}
}

func TestEgressRunPropagatesInstallerError(t *testing.T) {
	t.Parallel()

	installer := &fakeEgressInstaller{err: errors.New("nft apply failed")}
	egress, err := NewEgress(EgressConfig{
		Name:                "egress-01",
		OverlayIP:           "172.21.92.20",
		InternetInterface:   "eth0",
		MasqueradeInstaller: installer,
		AgentRunner: func(context.Context) error {
			t.Fatal("agent runner must not start after NAT failure")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewEgress: %v", err)
	}
	err = egress.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nft apply failed") {
		t.Fatalf("expected installer error, got %v", err)
	}
	if egress.Status().Started {
		t.Fatal("egress should not be marked started after NAT failure")
	}
}

func TestEgressRunWithoutAgentBlocksUntilCancel(t *testing.T) {
	t.Parallel()

	egress, err := NewEgress(EgressConfig{
		Name:                "egress-01",
		OverlayIP:           "172.21.92.20",
		InternetInterface:   "eth0",
		MasqueradeInstaller: &fakeEgressInstaller{},
	})
	if err != nil {
		t.Fatalf("NewEgress: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- egress.Run(ctx)
	}()

	waitFor(t, func() bool { return egress.Status().Started })
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error after cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("egress Run did not return after cancellation")
	}
}

func TestEgressCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	egress, err := NewEgress(EgressConfig{Name: "egress-01", OverlayIP: "172.21.92.20", InternetInterface: "eth0"})
	if err != nil {
		t.Fatalf("NewEgress: %v", err)
	}
	if err := egress.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := egress.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
