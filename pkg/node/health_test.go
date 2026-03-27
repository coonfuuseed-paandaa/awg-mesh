package node

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestNewHealthChecker(t *testing.T) {
	t.Parallel()

	cfg := HealthConfig{
		Interval:         2 * time.Second,
		Timeout:          500 * time.Millisecond,
		FailureThreshold: 5,
	}

	checker := NewHealthChecker(cfg, zerolog.Nop())
	if checker == nil {
		t.Fatal("expected checker instance")
	}
	if checker.cfg != cfg {
		t.Fatalf("unexpected checker config: got %#v want %#v", checker.cfg, cfg)
	}
}

func TestPingOverlayInvalidIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ip   string
	}{
		{name: "empty", ip: ""},
		{name: "hostname", ip: "example.com"},
		{name: "invalid octets", ip: "999.1.2.3"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if PingOverlay(tt.ip, 200*time.Millisecond) {
				t.Fatalf("expected PingOverlay(%q) to return false", tt.ip)
			}
		})
	}
}
