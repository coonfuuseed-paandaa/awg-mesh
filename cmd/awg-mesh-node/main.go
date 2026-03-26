package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/thebtf/awg-mesh/pkg/node"
)

var version = "dev"

const (
	modeMaster   = "master"
	modeEndpoint = "endpoint"
	modeClient   = "client"
)

func main() {
	mode := flag.String("mode", modeMaster, "Node mode: master|endpoint|client")
	name := flag.String("name", "", "Node name")
	overlayIP := flag.String("overlay-ip", "", "Node overlay IP address")
	listenPort := flag.Int("listen-port", 51820, "AWG listen port")
	configDir := flag.String("config-dir", "/config", "Node config directory")
	topologyPath := flag.String("topology", "", "Path to topology YAML")
	flag.Parse()

	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()

	if !isValidMode(*mode) {
		logger.Fatal().
			Str("mode", *mode).
			Str("valid_modes", modeMaster+","+modeEndpoint+","+modeClient).
			Msg("invalid node mode")
	}

	cfg := node.NodeConfig{
		Name:         *name,
		Mode:         *mode,
		OverlayIP:    *overlayIP,
		ListenPort:   *listenPort,
		ConfigDir:    *configDir,
		TopologyPath: *topologyPath,
	}

	meshNode, err := node.NewNode(cfg)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create node")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info().
		Str("version", version).
		Str("mode", *mode).
		Str("name", *name).
		Msg("awg-mesh-node starting")

	if err := meshNode.Run(ctx); err != nil {
		logger.Fatal().Err(err).Msg("node run failed")
	}

	if ctx.Err() != nil {
		meshNode.Shutdown()
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
