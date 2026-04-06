package node

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
	grpcserver "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
)

const defaultGRPCListenAddr = ":9090"
const grpcStartupGracePeriod = 200 * time.Millisecond

// startGRPCServer starts a background gRPC server with token fallback auth and
// dynamic TLS certificate reloading.
func startGRPCServer(
	ctx context.Context,
	configDir string,
	logger zerolog.Logger,
	tunnelMgr grpcserver.TunnelManager,
	paramApplier grpcserver.ParamApplier,
	peerMgr grpcserver.PeerManager,
	stateProvider grpcserver.NodeStateProvider,
	scheduler grpcserver.CaptureScheduler,
	keyProvider grpcserver.KeyProvider,
) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}

	trimmedConfigDir := strings.TrimSpace(configDir)
	if trimmedConfigDir == "" {
		return fmt.Errorf("config dir is required")
	}
	tlsDir := filepath.Join(trimmedConfigDir, "tls")

	handler := grpcserver.NewAgentHandlerFull(
		trimmedConfigDir,
		logger,
		tunnelMgr,
		paramApplier,
		newCaptureFunc(),
		peerMgr,
		stateProvider,
		scheduler,
		keyProvider,
	)
	serverConfig := grpcserver.ServerConfig{
		ListenAddr:    defaultGRPCListenAddr,
		TokenHashPath: trimmedConfigDir,
		CACertPath:    filepath.Join(tlsDir, "ca.crt"),
		CertPath:      tlsDir,
	}

	logger.Info().
		Str("addr", defaultGRPCListenAddr).
		Msg("starting gRPC with dynamic TLS (hot-reload enabled)")

	server, err := grpcserver.NewDynamicServer(serverConfig, handler, logger)
	if err != nil {
		return fmt.Errorf("create gRPC server: %w", err)
	}

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- server.Start()
	}()

	select {
	case serveErr := <-serveErrCh:
		if serveErr != nil {
			return fmt.Errorf("start gRPC server: %w", serveErr)
		}
		return fmt.Errorf("gRPC server stopped unexpectedly")
	case <-time.After(grpcStartupGracePeriod):
	}

	go func() {
		select {
		case serveErr := <-serveErrCh:
			if serveErr != nil && ctx.Err() == nil {
				logger.Error().Err(serveErr).Msg("gRPC server exited with error")
			}
		case <-ctx.Done():
		}
	}()

	go func() {
		<-ctx.Done()
		server.Stop()
	}()

	return nil
}
