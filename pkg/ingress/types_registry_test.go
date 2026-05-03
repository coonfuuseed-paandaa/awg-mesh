package ingress

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeConfigRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "missing node",
			cfg: Config{
				OverlayIP:     "172.21.92.30",
				PublicAddress: ":8443",
				Routes:        []Route{{Hostname: "media.example.com", Target: "172.21.92.10:8096"}},
			},
			want: "ingress name is required",
		},
		{
			name: "invalid overlay",
			cfg: Config{
				Name:          "ingress-01",
				OverlayIP:     "not-ip",
				PublicAddress: ":8443",
				Routes:        []Route{{Hostname: "media.example.com", Target: "172.21.92.10:8096"}},
			},
			want: "parse ingress overlay IP",
		},
		{
			name: "missing public address",
			cfg: Config{
				Name:      "ingress-01",
				OverlayIP: "172.21.92.30",
				Routes:    []Route{{Hostname: "media.example.com", Target: "172.21.92.10:8096"}},
			},
			want: "public bind address is required",
		},
		{
			name: "invalid hostname",
			cfg: Config{
				Name:          "ingress-01",
				OverlayIP:     "172.21.92.30",
				PublicAddress: ":8443",
				Routes:        []Route{{Hostname: "bad_host.example.com", Target: "172.21.92.10:8096"}},
			},
			want: "invalid character",
		},
		{
			name: "invalid target",
			cfg: Config{
				Name:          "ingress-01",
				OverlayIP:     "172.21.92.30",
				PublicAddress: ":8443",
				Routes:        []Route{{Hostname: "media.example.com", Target: "service.local:8096"}},
			},
			want: "must be an overlay IP",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NormalizeConfig(tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NormalizeConfig error = %v, want contains %q", err, tt.want)
			}
		})
	}
}

func TestPlanConfigUsesRouteAndRuntimeFields(t *testing.T) {
	t.Parallel()

	plan, err := PlanConfig(Config{
		Name:                "ingress-01",
		OverlayIP:           "172.21.92.30",
		PublicAddress:       ":8443",
		HealthProbeInterval: 2 * time.Second,
		UDPIdleTimeout:      7 * time.Second,
		MetricsAddress:      ":9092",
		ACMECacheDir:        "/var/lib/awg-mesh/acme",
		EnableHTTP3:         true,
		Routes: []Route{{
			Tenant:   "tenant-a",
			Hostname: "Media.Example.Com",
			Target:   "172.21.92.10:8096",
			Mode:     TLSModeTLSTerminate,
			Protocol: ProtocolHTTP,
		}},
	})
	if err != nil {
		t.Fatalf("PlanConfig: %v", err)
	}
	if plan.Name != "ingress-01" || plan.OverlayIP != "172.21.92.30" || plan.PublicAddress != ":8443" {
		t.Fatalf("identity fields not preserved: %+v", plan)
	}
	if plan.RouteCount != 1 || plan.Routes[0].Hostname != "media.example.com" || plan.Routes[0].Target != "172.21.92.10:8096" {
		t.Fatalf("route fields not used in plan: %+v", plan)
	}
	if plan.HealthProbeInterval != 2*time.Second || plan.UDPIdleTimeout != 7*time.Second {
		t.Fatalf("timing fields not used in plan: %+v", plan)
	}
	if !plan.ACMEEnabled || !plan.HTTP3Enabled || plan.MetricsAddress != ":9092" {
		t.Fatalf("feature flags not used in plan: %+v", plan)
	}
}

func TestRegistryReplaceIsCopyOnWrite(t *testing.T) {
	t.Parallel()

	registry, err := NewRegistry([]Route{{Hostname: "media.example.com", Target: "172.21.92.10:8096"}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	oldSnapshot := registry.Snapshot()
	if _, err := registry.Replace([]Route{{Hostname: "api.example.com", Target: "172.21.92.11:8080"}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if route, ok := oldSnapshot.Lookup("media.example.com"); !ok || route.Target != "172.21.92.10:8096" {
		t.Fatalf("old snapshot was mutated: route=%+v ok=%v", route, ok)
	}
	if _, ok := oldSnapshot.Lookup("api.example.com"); ok {
		t.Fatal("old snapshot sees replacement route")
	}
	if route, ok := registry.Lookup("api.example.com"); !ok || route.Target != "172.21.92.11:8080" {
		t.Fatalf("current registry did not publish replacement: route=%+v ok=%v", route, ok)
	}
}

func TestRegistryRejectsDuplicateHostnameAcrossTenants(t *testing.T) {
	t.Parallel()

	_, err := NewRegistry([]Route{
		{Tenant: "tenant-a", Hostname: "media.example.com", Target: "172.21.92.10:8096"},
		{Tenant: "tenant-b", Hostname: "MEDIA.EXAMPLE.COM", Target: "172.21.92.11:8096"},
	})
	if err == nil || !strings.Contains(err.Error(), "already owned by tenant") {
		t.Fatalf("expected duplicate hostname rejection, got %v", err)
	}
}
