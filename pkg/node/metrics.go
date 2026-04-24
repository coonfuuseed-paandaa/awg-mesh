package node

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

// StartMetricsServer starts a Prometheus metrics server. When the bind address
// is not localhost, bearer token authentication is enforced using the node's
// mesh.token from configDir. Prometheus scrape config supports this natively
// via authorization.credentials_file.
func StartMetricsServer(addr, configDir string) (*http.Server, error) {
	metricsAddr := strings.TrimSpace(addr)
	if metricsAddr == "" {
		return nil, fmt.Errorf("metrics address is required")
	}

	listener, err := net.Listen("tcp", metricsAddr)
	if err != nil {
		return nil, fmt.Errorf("listen metrics server on %q: %w", metricsAddr, err)
	}

	handler := promhttp.Handler()

	if !isLocalhostAddr(metricsAddr) {
		token, tokenErr := loadMetricsToken(configDir)
		if tokenErr != nil {
			_ = listener.Close()
			return nil, fmt.Errorf("metrics auth required for non-localhost bind %q but token unavailable: %w", metricsAddr, tokenErr)
		}
		handler = bearerAuthMiddleware(token, handler)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", handler)

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

// isLocalhostAddr returns true if the bind address is localhost (127.0.0.1 or ::1).
func isLocalhostAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimSpace(host)
	return host == "" || host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// loadMetricsToken reads the bearer token from <configDir>/mesh.token.
func loadMetricsToken(configDir string) (string, error) {
	dir := strings.TrimSpace(configDir)
	if dir == "" {
		return "", fmt.Errorf("config directory is empty")
	}
	data, err := os.ReadFile(filepath.Join(dir, "mesh.token"))
	if err != nil {
		return "", fmt.Errorf("read mesh.token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("mesh.token is empty")
	}
	return token, nil
}

// bearerAuthMiddleware wraps an http.Handler with Bearer token verification.
func bearerAuthMiddleware(expectedToken string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "authorization required", http.StatusUnauthorized)
			return
		}

		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			http.Error(w, "invalid authorization scheme", http.StatusUnauthorized)
			return
		}

		provided := strings.TrimSpace(auth[len(prefix):])
		if subtle.ConstantTimeCompare([]byte(provided), []byte(expectedToken)) != 1 {
			http.Error(w, "invalid token", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
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
