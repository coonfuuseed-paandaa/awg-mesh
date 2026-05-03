package ingress

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestRuntimeStartsAndStops(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()

	publicAddress := freeTCPAddress(t)
	runtime, err := NewRuntime(Config{
		Name:          "ingress-01",
		OverlayIP:     "172.21.92.30",
		PublicAddress: publicAddress,
		Routes: []Route{{
			Hostname: "media.example.com",
			Target:   strings.TrimPrefix(target.URL, "http://"),
			Protocol: ProtocolHTTP,
		}},
		HealthProbeInterval: 10 * time.Millisecond,
	}, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- runtime.Run(ctx)
	}()
	waitForRuntime(t, runtime)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runtime did not stop after context cancellation")
	}
	if runtime.Started() {
		t.Fatal("runtime still reports started after cancellation")
	}
}

func freeTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free TCP port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close free TCP listener: %v", err)
	}
	return addr
}

func waitForRuntime(t *testing.T, runtime *Runtime) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runtime.Started() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("runtime did not report started")
}
