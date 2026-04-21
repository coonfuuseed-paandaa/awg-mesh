package node

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	grpcserver "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/rs/zerolog"
)

const defaultGRPCListenAddr = ":9090"

// grpcStartupGracePeriod is a time-based heuristic for detecting server bind failure.
// If the server fails to bind within this window, the error is returned to the caller.
// A deterministic readiness signal (via net.Listen + passing the listener) would be
// more robust, but would require refactoring the DynamicServer.Start() interface.
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
	statePersister grpcserver.NodeStatePersister,
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
		statePersister,
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
