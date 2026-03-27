package node

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	registerMetricsOnce sync.Once

	tunnelsTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tunnels_total",
		Help: "Total number of tunnels managed by the master node.",
	})
	tunnelsHealthy = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tunnels_healthy",
		Help: "Number of healthy tunnels managed by the master node.",
	})
	peersTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "peers_total",
		Help: "Total number of WireGuard peers.",
	})
	handshakeAgeSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "handshake_age_seconds",
		Help:    "Age of latest WireGuard handshakes in seconds.",
		Buckets: []float64{1, 5, 30, 60, 300, 900},
	})
	rxBytesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rx_bytes_total",
		Help: "Total received bytes across all tunnels.",
	})
	txBytesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tx_bytes_total",
		Help: "Total transmitted bytes across all tunnels.",
	})
	rotationTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rotation_total",
		Help: "Total AWG parameter rotations by tier.",
	}, []string{"tier"})
	healthcheckTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "healthcheck_total",
		Help: "Total health checks by result.",
	}, []string{"result"})
)

// RegisterMetrics registers all node-level Prometheus collectors.
func RegisterMetrics() {
	registerMetricsOnce.Do(func() {
		prometheus.MustRegister(
			tunnelsTotal,
			tunnelsHealthy,
			peersTotal,
			handshakeAgeSeconds,
			rxBytesTotal,
			txBytesTotal,
			rotationTotal,
			healthcheckTotal,
		)
	})
}

// StartMetricsServer starts a Prometheus metrics server and returns the running server.
func StartMetricsServer(addr string) (*http.Server, error) {
	metricsAddr := strings.TrimSpace(addr)
	if metricsAddr == "" {
		return nil, fmt.Errorf("metrics address is required")
	}

	listener, err := net.Listen("tcp", metricsAddr)
	if err != nil {
		return nil, fmt.Errorf("listen metrics server on %q: %w", metricsAddr, err)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	server := &http.Server{
		Addr:    metricsAddr,
		Handler: mux,
	}

	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "metrics server error: %v\n", serveErr)
		}
	}()

	return server, nil
}

// UpdateTunnelMetrics updates tunnel total and healthy gauges from current tunnel state.
func UpdateTunnelMetrics(tunnels []MasterTunnel) {
	total := len(tunnels)
	healthy := 0
	for _, tunnel := range tunnels {
		if tunnel.Healthy {
			healthy++
		}
	}

	tunnelsTotal.Set(float64(total))
	tunnelsHealthy.Set(float64(healthy))
}
