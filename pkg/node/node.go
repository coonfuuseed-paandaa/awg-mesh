package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog"
	"github.com/thebtf/awg-mesh/pkg/topology"
)

const defaultConfigDir = "/config"

// NodeConfig contains startup configuration for a node instance.
type NodeConfig struct {
	Name         string
	Mode         string
	OverlayIP    string
	ListenPort   int
	ConfigDir    string
	TopologyPath string
}

// Node is the core runtime abstraction for an awg-mesh node.
type Node struct {
	config   NodeConfig
	topology *topology.Topology
	logger   zerolog.Logger
}

// NewNode validates config, optionally loads topology, and constructs a node.
func NewNode(cfg NodeConfig) (*Node, error) {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		return nil, fmt.Errorf("node name is required")
	}

	mode := strings.TrimSpace(cfg.Mode)
	if mode == "" {
		return nil, fmt.Errorf("node mode is required")
	}

	configDir := strings.TrimSpace(cfg.ConfigDir)
	if configDir == "" {
		configDir = defaultConfigDir
	}

	topologyPath := strings.TrimSpace(cfg.TopologyPath)
	normalizedConfig := NodeConfig{
		Name:         name,
		Mode:         mode,
		OverlayIP:    strings.TrimSpace(cfg.OverlayIP),
		ListenPort:   cfg.ListenPort,
		ConfigDir:    configDir,
		TopologyPath: topologyPath,
	}

	var loadedTopology *topology.Topology
	if topologyPath != "" {
		topologyConfig, err := topology.LoadTopology(topologyPath)
		if err != nil {
			return nil, fmt.Errorf("load topology: %w", err)
		}
		loadedTopology = topologyConfig
	}

	logger := zerolog.New(os.Stderr).
		With().
		Timestamp().
		Str("component", "node").
		Str("name", normalizedConfig.Name).
		Str("mode", normalizedConfig.Mode).
		Logger()

	return &Node{
		config:   normalizedConfig,
		topology: loadedTopology,
		logger:   logger,
	}, nil
}

// Run starts the mode-specific runner and waits until completion.
func (n *Node) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}

	var err error
	switch n.config.Mode {
	case "endpoint":
		err = n.runEndpoint(ctx)
	case "client":
		err = n.runClient(ctx)
	case "master":
		return fmt.Errorf("master mode not yet implemented")
	default:
		return fmt.Errorf("unsupported node mode %q", n.config.Mode)
	}

	if err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		return err
	}

	return nil
}

// Shutdown emits a shutdown log entry.
func (n *Node) Shutdown() {
	n.logger.Info().Msg("node shutting down")
}

func (n *Node) runEndpoint(ctx context.Context) error {
	runner := NewEndpointRunner(n)
	return runner.Run(ctx)
}

func (n *Node) runClient(ctx context.Context) error {
	runner := NewClientRunner(n)
	return runner.Run(ctx)
}
