package nftables

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeNATFirewall struct {
	interfaces []string
	err        error
}

func (f *fakeNATFirewall) SetupNAT(iface string) error {
	f.interfaces = append(f.interfaces, iface)
	return f.err
}

func TestPlanValidatesInternetInterface(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		iface   string
		wantErr string
	}{
		{name: "missing", iface: "", wantErr: "required"},
		{name: "whitespace", iface: "   ", wantErr: "required"},
		{name: "too long", iface: "this-interface-is-too-long", wantErr: "IFNAMSIZ"},
		{name: "contains slash", iface: "eth0/1", wantErr: "invalid characters"},
		{name: "wireguard", iface: "wg-mesh", wantErr: "mesh interface"},
		{name: "amneziawg", iface: "awg-mesh0", wantErr: "mesh interface"},
		{name: "tun", iface: "tun0", wantErr: "mesh interface"},
		{name: "valid ethernet", iface: "eth0", wantErr: ""},
		{name: "valid predictable name", iface: "ens18", wantErr: ""},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan, err := Plan(MasqueradeConfig{InternetInterface: tt.iface})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if plan.InternetInterface != tt.iface {
				t.Fatalf("plan interface = %q, want %q", plan.InternetInterface, tt.iface)
			}
			if !strings.Contains(plan.Operation, tt.iface) || !strings.Contains(plan.Operation, "masquerade") {
				t.Fatalf("plan operation does not describe egress masquerade: %+v", plan)
			}
		})
	}
}

func TestMasqueradeInstallerApplyUsesConfiguredInterface(t *testing.T) {
	t.Parallel()

	firewall := &fakeNATFirewall{}
	installer, err := NewMasqueradeInstaller(firewall)
	if err != nil {
		t.Fatalf("NewMasqueradeInstaller: %v", err)
	}

	plan, err := installer.Apply(context.Background(), MasqueradeConfig{InternetInterface: "eth0"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if plan.InternetInterface != "eth0" {
		t.Fatalf("plan interface = %q, want eth0", plan.InternetInterface)
	}
	if len(firewall.interfaces) != 1 || firewall.interfaces[0] != "eth0" {
		t.Fatalf("SetupNAT calls = %#v, want [eth0]", firewall.interfaces)
	}
}

func TestMasqueradeInstallerApplyIsDeterministicForSameInterface(t *testing.T) {
	t.Parallel()

	firewall := &fakeNATFirewall{}
	installer, err := NewMasqueradeInstaller(firewall)
	if err != nil {
		t.Fatalf("NewMasqueradeInstaller: %v", err)
	}

	first, err := installer.Apply(context.Background(), MasqueradeConfig{InternetInterface: "ens18"})
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	second, err := installer.Apply(context.Background(), MasqueradeConfig{InternetInterface: "ens18"})
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if first != second {
		t.Fatalf("plans differ across repeated apply: first=%+v second=%+v", first, second)
	}
	if got := strings.Join(firewall.interfaces, ","); got != "ens18,ens18" {
		t.Fatalf("SetupNAT calls = %s, want ens18,ens18", got)
	}
}

func TestMasqueradeInstallerApplyPropagatesFirewallError(t *testing.T) {
	t.Parallel()

	firewall := &fakeNATFirewall{err: errors.New("kernel rejected rule")}
	installer, err := NewMasqueradeInstaller(firewall)
	if err != nil {
		t.Fatalf("NewMasqueradeInstaller: %v", err)
	}

	_, err = installer.Apply(context.Background(), MasqueradeConfig{InternetInterface: "eth0"})
	if err == nil || !strings.Contains(err.Error(), "kernel rejected rule") {
		t.Fatalf("expected firewall error, got %v", err)
	}
}

func TestMasqueradeInstallerApplyHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	firewall := &fakeNATFirewall{}
	installer, err := NewMasqueradeInstaller(firewall)
	if err != nil {
		t.Fatalf("NewMasqueradeInstaller: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = installer.Apply(ctx, MasqueradeConfig{InternetInterface: "eth0"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(firewall.interfaces) != 0 {
		t.Fatalf("SetupNAT called after context cancellation: %#v", firewall.interfaces)
	}
}
