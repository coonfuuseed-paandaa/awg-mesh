package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/thebtf/awg-mesh/pkg/logging"
	"github.com/thebtf/awg-mesh/pkg/node"
)

// version is set via ldflags: -X main.version=v0.1.0
// Falls back to module version from go install, then "dev".
var version = "dev"

func resolveVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

const (
	modeMaster   = "master"
	modeEndpoint = "endpoint"
	modeClient   = "client"
)

const metricsShutdownTimeout = 5 * time.Second

type nodeOptions struct {
	mode        string
	name        string
	overlayIP   string
	listenPort  int
	configDir   string
	topology    string
	logLevel    string
	metricsAddr string
}

func main() {
	options := parseNodeOptions()

	logging.SetGlobalLevel(options.logLevel)
	logger := logging.NewLogger("awg-mesh-node")

	if !isValidMode(options.mode) {
		logger.Fatal().
			Str("mode", options.mode).
			Str("valid_modes", modeMaster+","+modeEndpoint+","+modeClient).
			Msg("invalid node mode")
	}

	cfg := node.NodeConfig{
		Name:         options.name,
		Mode:         options.mode,
		OverlayIP:    options.overlayIP,
		ListenPort:   options.listenPort,
		ConfigDir:    options.configDir,
		TopologyPath: options.topology,
	}

	meshNode, err := node.NewNode(cfg)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create node")
	}

	node.RegisterMetrics()
	metricsServer, metricsErr := node.StartMetricsServer(options.metricsAddr)
	if metricsErr != nil {
		logger.Warn().
			Err(metricsErr).
			Str("metrics_addr", options.metricsAddr).
			Msg("failed to start metrics server")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info().
		Str("version", resolveVersion()).
		Str("mode", options.mode).
		Str("name", options.name).
		Msg("awg-mesh-node starting")

	if err := meshNode.Run(ctx); err != nil {
		logger.Fatal().Err(err).Msg("node run failed")
	}

	if ctx.Err() != nil {
		shutdownMetricsServer(metricsServer, logger)
		meshNode.Shutdown()
		logger.Info().Msg("node exited cleanly")
	}
}

func parseNodeOptions() nodeOptions {
	mode := flag.String("mode", modeMaster, "Node mode: master|endpoint|client")
	name := flag.String("name", "", "Node name")
	overlayIP := flag.String("overlay-ip", "", "Node overlay IP address")
	listenPort := flag.Int("listen-port", 51820, "AWG listen port")
	configDir := flag.String("config-dir", "/config", "Node config directory")
	topologyPath := flag.String("topology", "", "Path to topology YAML")
	logLevel := flag.String("log-level", "info", "Log level: debug|info|warn|error")
	metricsAddr := flag.String("metrics-addr", ":9091", "Prometheus metrics listen address")
	flag.Parse()

	return nodeOptions{
		mode:        *mode,
		name:        *name,
		overlayIP:   *overlayIP,
		listenPort:  *listenPort,
		configDir:   *configDir,
		topology:    *topologyPath,
		logLevel:    *logLevel,
		metricsAddr: *metricsAddr,
	}
}

func shutdownMetricsServer(server *http.Server, logger zerolog.Logger) {
	if server == nil {
		return
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), metricsShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn().Err(err).Msg("failed to shut down metrics server")
	}
}

func isValidMode(mode string) bool {
	switch mode {
	case modeMaster, modeEndpoint, modeClient:
		return true
	default:
		return false
	}
}
