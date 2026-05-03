package balancer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
)

type Runtime struct {
	mu            sync.Mutex
	cfg           Config
	plan          Plan
	engine        *Engine
	metricsServer *http.Server
	logger        zerolog.Logger
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
	engine, err := NewEngine(normalized, &logger)
	if err != nil {
		return nil, err
	}
	return &Runtime{
		cfg:    normalized,
		plan:   plan,
		engine: engine,
		logger: logger,
	}, nil
}

func (r *Runtime) Plan() Plan {
	if r == nil {
		return Plan{}
	}
	return r.plan
}

func (r *Runtime) Engine() *Engine {
	if r == nil {
		return nil
	}
	return r.engine
}

func (r *Runtime) Metrics() *Metrics {
	if r == nil || r.engine == nil {
		return nil
	}
	return r.engine.Metrics()
}

func (r *Runtime) Run(ctx context.Context) error {
	if r == nil {
		return errors.New("balancer runtime is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if r.cfg.MetricsAddress != "" {
		r.metricsServer = &http.Server{
			Addr:              r.cfg.MetricsAddress,
			Handler:           promhttp.HandlerFor(r.engine.Metrics().Registry(), promhttp.HandlerOpts{}),
			ReadHeaderTimeout: 5 * time.Second,
		}
	}
	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
	r.logger.Info().
		Str("event", EventRuntimeStarted).
		Str("node", r.cfg.Name).
		Str("mode", string(r.cfg.Mode)).
		Int("egresses", len(r.cfg.Egresses)).
		Msg(EventRuntimeStarted)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 1)
	go r.engine.Health().Run(runCtx, r.engine.Registry(), r.engine.Metrics(), r.cfg.HealthProbeInterval)
	if r.metricsServer != nil {
		go func() { errCh <- serveHTTP(r.metricsServer) }()
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

func serveHTTP(server *http.Server) error {
	if server == nil {
		return fmt.Errorf("balancer metrics server is nil")
	}
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
