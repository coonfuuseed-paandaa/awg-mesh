package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
	grpcserver "github.com/thebtf/awg-mesh/pkg/grpc"
)

const defaultGRPCListenAddr = ":9090"
const grpcStartupGracePeriod = 200 * time.Millisecond

// startGRPCServer starts a background gRPC server using either mTLS+token auth
// or token-only auth if TLS materials are not available yet.
func startGRPCServer(
	ctx context.Context,
	configDir string,
	logger zerolog.Logger,
	tunnelMgr grpcserver.TunnelManager,
	paramApplier grpcserver.ParamApplier,
) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}

	trimmedConfigDir := strings.TrimSpace(configDir)
	if trimmedConfigDir == "" {
		return fmt.Errorf("config dir is required")
	}

	handler := grpcserver.NewAgentHandlerFull(trimmedConfigDir, logger, tunnelMgr, paramApplier)
	serverConfig := grpcserver.ServerConfig{
		ListenAddr:    defaultGRPCListenAddr,
		TokenHashPath: trimmedConfigDir,
	}

	tlsDir := filepath.Join(trimmedConfigDir, "tls")
	hasTLSMaterials, err := hasTLSCredentials(tlsDir)
	if err != nil {
		return err
	}

	var server *grpcserver.Server
	if hasTLSMaterials {
		logger.Info().
			Str("addr", defaultGRPCListenAddr).
			Msg("starting gRPC with mTLS")

		serverConfig.CACertPath = filepath.Join(tlsDir, "ca.crt")
		serverConfig.CertPath = tlsDir

		server, err = grpcserver.NewServer(serverConfig, handler, logger)
		if err != nil {
			return fmt.Errorf("create mTLS gRPC server: %w", err)
		}
	} else {
		logger.Info().
			Str("addr", defaultGRPCListenAddr).
			Msg("starting gRPC with token-only auth (no TLS certs yet)")

		server, err = grpcserver.NewInsecureServer(serverConfig, handler, logger)
		if err != nil {
			return fmt.Errorf("create token-only gRPC server: %w", err)
		}
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
		if serveErr := <-serveErrCh; serveErr != nil && ctx.Err() == nil {
			logger.Error().Err(serveErr).Msg("gRPC server exited with error")
		}
	}()

	go func() {
		<-ctx.Done()
		server.Stop()
	}()

	return nil
}

func hasTLSCredentials(tlsDir string) (bool, error) {
	requiredFiles := []string{"ca.crt", "node.crt", "node.key"}
	for _, filename := range requiredFiles {
		path := filepath.Join(tlsDir, filename)
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, fmt.Errorf("stat tls file %q: %w", path, err)
		}
	}

	return true, nil
}
