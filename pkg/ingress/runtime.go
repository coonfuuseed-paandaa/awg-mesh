package ingress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/quic-go/quic-go/http3"
	"github.com/rs/zerolog"
	"golang.org/x/crypto/acme/autocert"
)

// Runtime owns ingress listeners and background health/metrics loops.
type Runtime struct {
	mu            sync.Mutex
	cfg           Config
	plan          Plan
	registry      *Registry
	health        *HealthTracker
	metrics       *Metrics
	logger        zerolog.Logger
	httpServer    *http.Server
	http3Server   *http3.Server
	metricsServer *http.Server
	started       bool
}

func NewRuntime(cfg Config, logger zerolog.Logger) (*Runtime, error) {
	normalized, err := NormalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	plan, err := PlanConfig(normalized)
	if err != nil {
		return nil, err
	}
	registry, err := NewRegistry(normalized.Routes)
	if err != nil {
		return nil, err
	}
	metrics, err := NewMetrics(nil)
	if err != nil {
		return nil, fmt.Errorf("ingress metrics: %w", err)
	}
	return &Runtime{
		cfg:      normalized,
		plan:     plan,
		registry: registry,
		health:   NewHealthTracker(nil),
		metrics:  metrics,
		logger:   logger,
	}, nil
}

func (r *Runtime) Plan() Plan {
	if r == nil {
		return Plan{}
	}
	return r.plan
}

func (r *Runtime) Registry() *Registry {
	if r == nil {
		return nil
	}
	return r.registry
}

func (r *Runtime) Metrics() *Metrics {
	if r == nil {
		return nil
	}
	return r.metrics
}

func (r *Runtime) Run(ctx context.Context) error {
	if r == nil {
		return errors.New("ingress runtime is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	handler := NewHTTPProxy(r.registry, r.health, r.metrics, r.logger)
	mux := http.NewServeMux()
	mux.Handle("/", handler)
	mux.Handle("/.awg-mesh/ws/", NewWebSocketProxy(r.registry, r.health, r.metrics, r.logger))

	tlsConfig, err := r.tlsConfig()
	if err != nil {
		return err
	}
	r.httpServer = &http.Server{
		Addr:              r.cfg.PublicAddress,
		Handler:           mux,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if r.plan.HTTP3Enabled && tlsConfig != nil {
		r.http3Server = &http3.Server{
			Addr:      r.cfg.PublicAddress,
			Handler:   mux,
			TLSConfig: tlsConfig.Clone(),
		}
	}
	if r.cfg.MetricsAddress != "" {
		r.metricsServer = &http.Server{
			Addr:              r.cfg.MetricsAddress,
			Handler:           promhttp.HandlerFor(r.metrics.Registry(), promhttp.HandlerOpts{}),
			ReadHeaderTimeout: 5 * time.Second,
		}
	}

	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
	r.logger.Info().
		Str("event", EventRuntimeStarted).
		Str("node", r.cfg.Name).
		Str("public_addr", r.cfg.PublicAddress).
		Int("routes", len(r.cfg.Routes)).
		Msg(EventRuntimeStarted)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 3)
	go r.health.Run(runCtx, r.registry, r.cfg.HealthProbeInterval)
	go func() { errCh <- serveHTTP(r.httpServer, tlsConfig != nil) }()
	if r.http3Server != nil {
		go func() { errCh <- r.http3Server.ListenAndServe() }()
	}
	if r.metricsServer != nil {
		go func() { errCh <- serveHTTP(r.metricsServer, false) }()
	}

	select {
	case <-ctx.Done():
		return r.Close()
	case err := <-errCh:
		if err != nil {
			cancel()
			return errors.Join(err, r.Close())
		}
		return nil
	}
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var errs []error
	if r.httpServer != nil {
		if err := r.httpServer.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if r.http3Server != nil {
		if err := r.http3Server.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if r.metricsServer != nil {
		if err := r.metricsServer.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	r.mu.Lock()
	r.started = false
	r.mu.Unlock()
	r.logger.Info().Str("event", EventRuntimeStopped).Str("node", r.cfg.Name).Msg(EventRuntimeStopped)
	return errors.Join(errs...)
}

func (r *Runtime) Started() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}

func (r *Runtime) tlsConfig() (*tls.Config, error) {
	if r.cfg.ACMECacheDir == "" {
		return nil, nil
	}
	hostPolicy := autocert.HostWhitelist(routeHostnames(r.cfg.Routes)...)
	manager := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: hostPolicy,
		Cache:      autocert.DirCache(r.cfg.ACMECacheDir),
		Email:      r.cfg.ACMEEmail,
	}
	cfg := manager.TLSConfig()
	cfg.MinVersion = tls.VersionTLS12
	cfg.NextProtos = []string{"h2", "http/1.1", "h3"}
	return cfg, nil
}

func routeHostnames(routes []Route) []string {
	out := make([]string, 0, len(routes))
	for _, route := range routes {
		out = append(out, route.Hostname)
	}
	return out
}

func serveHTTP(server *http.Server, tlsEnabled bool) error {
	var err error
	if tlsEnabled {
		err = server.ListenAndServeTLS("", "")
	} else {
		err = server.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
