package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/logging"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/node"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	"github.com/rs/zerolog"
	"gopkg.in/natefinch/lumberjack.v2"
)

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

// newClientLogRotator returns a lumberjack rotating file writer for client-mode
// log output. The log file is placed in configDir so it co-locates with other
// persistent node state (keys, token). Parameters:
//   - MaxSize 10 MB — keeps logs well within the ~64 MB RouterOS storage budget
//   - MaxBackups 3 — three generations of rotated files before oldest is removed
//   - MaxAge 0 — no age-based deletion; size is the only eviction trigger
//   - LocalTime true — timestamps in log file names use the host clock timezone
//   - Compress false — uncompressed for immediate human-readable access
func newClientLogRotator(configDir string) *lumberjack.Logger {
	return &lumberjack.Logger{
		Filename:   filepath.Join(configDir, "awg-mesh-client.log"),
		MaxSize:    10,
		MaxBackups: 3,
		MaxAge:     0,
		LocalTime:  true,
		Compress:   false,
	}
}

func main() {
	options := parseNodeOptions()

	logging.SetGlobalLevel(options.logLevel)

	var logger zerolog.Logger
	if options.mode == modeClient {
		rotator := newClientLogRotator(options.configDir)
		logger = zerolog.New(rotator).
			With().
			Timestamp().
			Str("component", "awg-mesh-node").
			Logger()
	} else {
		logger = logging.NewLogger("awg-mesh-node")
	}

	if !isValidMode(options.mode) {
		logger.Fatal().
			Str("mode", options.mode).
			Str("valid_modes", modeMaster+","+modeEndpoint+","+modeClient).
			Msg("invalid node mode")
	}

	if err := bootstrapTokenHash(options.configDir, logger); err != nil {
		logger.Fatal().Err(err).Msg("token bootstrap failed")
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
	metricsServer, metricsErr := node.StartMetricsServer(options.metricsAddr, options.configDir)
	if metricsErr != nil {
		logger.Warn().
			Err(metricsErr).
			Str("metrics_addr", options.metricsAddr).
			Msg("failed to start metrics server")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info().
		Str("version", version()).
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

// parseNodeOptions reads CLI flags with MESH_* env var fallback for flags that
// were not explicitly set on the command line. Flags win over env vars — this
// matches 12-factor container conventions where env vars are the usual config
// transport and flags are reserved for debug/override.
func parseNodeOptions() nodeOptions {
	mode := flag.String("mode", modeMaster, "Node mode: master|endpoint|client (env: MESH_MODE)")
	name := flag.String("name", "", "Node name (env: MESH_NAME)")
	overlayIP := flag.String("overlay-ip", "", "Node overlay IP address (env: MESH_OVERLAY_IP)")
	listenPort := flag.Int("listen-port", 51820, "AWG listen port (env: MESH_LISTEN_PORT)")
	configDir := flag.String("config-dir", "/config", "Node config directory (env: MESH_CONFIG_DIR)")
	topologyPath := flag.String("topology", "", "Path to topology YAML (env: MESH_TOPOLOGY)")
	logLevel := flag.String("log-level", "info", "Log level: debug|info|warn|error (env: MESH_LOG_LEVEL)")
	metricsAddr := flag.String("metrics-addr", "127.0.0.1:9091", "Prometheus metrics listen address (env: MESH_METRICS_ADDR)")
	flag.Parse()

	setFlags := explicitFlags()

	return nodeOptions{
		mode:        envFallbackString(setFlags, "mode", *mode, "MESH_MODE"),
		name:        envFallbackString(setFlags, "name", *name, "MESH_NAME"),
		overlayIP:   envFallbackString(setFlags, "overlay-ip", *overlayIP, "MESH_OVERLAY_IP"),
		listenPort:  envFallbackInt(setFlags, "listen-port", *listenPort, "MESH_LISTEN_PORT"),
		configDir:   envFallbackString(setFlags, "config-dir", *configDir, "MESH_CONFIG_DIR"),
		topology:    envFallbackString(setFlags, "topology", *topologyPath, "MESH_TOPOLOGY"),
		logLevel:    envFallbackString(setFlags, "log-level", *logLevel, "MESH_LOG_LEVEL"),
		metricsAddr: envFallbackString(setFlags, "metrics-addr", *metricsAddr, "MESH_METRICS_ADDR"),
	}
}

// explicitFlags returns the names of flags that were explicitly set on the
// command line. These take precedence over MESH_* env vars.
func explicitFlags() map[string]bool {
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) {
		set[f.Name] = true
	})
	return set
}

func envFallbackString(setFlags map[string]bool, flagName, flagValue, envName string) string {
	if setFlags[flagName] {
		return flagValue
	}
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		return v
	}
	return flagValue
}

func envFallbackInt(setFlags map[string]bool, flagName string, flagValue int, envName string) int {
	if setFlags[flagName] {
		return flagValue
	}
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		return flagValue
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		// Logger is not yet constructed here; emit the diagnostic directly
		// so an operator with a typo in MESH_LISTEN_PORT (e.g. "51820x")
		// sees the fall-back happen instead of silently getting the default.
		fmt.Fprintf(os.Stderr,
			"warning: %s=%q is not a valid integer, using flag default %d (%v)\n",
			envName, raw, flagValue, err)
		return flagValue
	}
	return parsed
}

// bootstrapTokenHash copies the MESH_TOKEN_HASH env var into <configDir>/mesh.token
// on first boot. The value must be a v2 argon2id hash (prefix "mesh1."); bcrypt and
// plaintext values are rejected with a structured error so the node refuses to start
// with an invalid token, keeping auth state on the node correct.
// Subsequent boots see an existing token file and leave the env var untouched.
func bootstrapTokenHash(configDir string, logger zerolog.Logger) error {
	rawVal, set := os.LookupEnv("MESH_TOKEN_HASH")
	if !set {
		return nil // env var absent entirely — no-op on subsequent boots
	}
	hash := strings.TrimSpace(rawVal)
	if hash == "" {
		logger.Error().
			Str("event", "token_hash_invalid").
			Str("format", "unknown").
			Msg("MESH_TOKEN_HASH must be v2 format")
		return errors.New("MESH_TOKEN_HASH is set but empty — must be a v2 argon2id hash")
	}

	if _, _, _, _, _, _, _, err := pkgtls.ParseV2(hash); err != nil {
		logger.Error().
			Str("event", "token_hash_invalid").
			Str("format", "unknown").
			Msg("MESH_TOKEN_HASH must be v2 format")
		return fmt.Errorf("MESH_TOKEN_HASH is not a valid v2 token hash: %w", err)
	}

	cleanDir := strings.TrimSpace(configDir)
	if cleanDir == "" {
		return errors.New("config directory is empty — cannot bootstrap token hash")
	}

	tokenPath := filepath.Join(cleanDir, "mesh.token")
	if _, err := os.Stat(tokenPath); err == nil {
		return nil // token already present; env var ignored on subsequent boots
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat token file %q: %w", tokenPath, err)
	}

	if err := os.MkdirAll(cleanDir, 0o700); err != nil {
		return fmt.Errorf("create config dir %q: %w", cleanDir, err)
	}

	tmpPath := tokenPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(hash), 0o600); err != nil {
		return fmt.Errorf("write token temp file: %w", err)
	}
	if err := os.Rename(tmpPath, tokenPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename token file: %w", err)
	}

	logger.Warn().
		Str("path", tokenPath).
		Msg("token hash bootstrapped from MESH_TOKEN_HASH env var — mount a real file for production-grade secret handling")
	return nil
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

// versionFromBuild is injected at build time via
//
//	go build -ldflags "-X main.versionFromBuild=<ref>"
//
// Empty when not injected — falls through to debug.ReadBuildInfo().
var versionFromBuild = ""

func version() string {
	if versionFromBuild != "" {
		return versionFromBuild
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}

	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}

	var revision string
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			revision = s.Value
			break
		}
	}

	if revision != "" {
		if len(revision) > 8 {
			revision = revision[:8]
		}
		return revision
	}

	return "dev"
}
