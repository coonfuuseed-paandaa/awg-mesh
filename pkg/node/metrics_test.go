package node

import (
	"context"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func TestRegisterMetricsIsIdempotent(t *testing.T) {
	t.Parallel()

	RegisterMetrics()
	RegisterMetrics()
}

func TestUpdateTunnelMetrics(t *testing.T) {
	t.Parallel()

	UpdateTunnelMetrics([]MasterTunnel{
		{Name: "a", Healthy: true},
		{Name: "b", Healthy: false},
		{Name: "c", Healthy: true},
	})

	total, err := readGaugeValue(tunnelsTotal)
	if err != nil {
		t.Fatalf("read tunnelsTotal gauge: %v", err)
	}
	healthy, err := readGaugeValue(tunnelsHealthy)
	if err != nil {
		t.Fatalf("read tunnelsHealthy gauge: %v", err)
	}

	if total != 3 {
		t.Fatalf("expected tunnelsTotal=3, got %v", total)
	}
	if healthy != 2 {
		t.Fatalf("expected tunnelsHealthy=2, got %v", healthy)
	}
}

func TestStartMetricsServer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		addr         string
		wantErr      bool
		errContains  string
		expectServer bool
	}{
		{
			name:        "returns error for empty address",
			addr:        "   ",
			wantErr:     true,
			errContains: "metrics address is required",
		},
		{
			name:         "starts server for valid address",
			addr:         "127.0.0.1:0",
			expectServer: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server, err := StartMetricsServer(tt.addr)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected StartMetricsServer error")
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("expected error containing %q, got %v", tt.errContains, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("StartMetricsServer returned error: %v", err)
			}
			if tt.expectServer && server == nil {
				t.Fatal("expected non-nil server")
			}
			if server != nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if shutdownErr := server.Shutdown(ctx); shutdownErr != nil {
					t.Fatalf("metrics server shutdown failed: %v", shutdownErr)
				}
			}
		})
	}
}

func readGaugeValue(gauge interface{ Write(*dto.Metric) error }) (float64, error) {
	metric := &dto.Metric{}
	if err := gauge.Write(metric); err != nil {
		return 0, err
	}
	if metric.Gauge == nil {
		return 0, nil
	}
	return metric.GetGauge().GetValue(), nil
}
