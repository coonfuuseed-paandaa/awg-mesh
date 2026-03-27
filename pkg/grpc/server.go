package grpcserver

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"

	"github.com/rs/zerolog"
	pkgtls "github.com/thebtf/awg-mesh/pkg/tls"
	proto "github.com/thebtf/awg-mesh/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ServerConfig holds configuration for the gRPC server.
type ServerConfig struct {
	ListenAddr    string
	CACertPath    string
	CertPath      string // directory containing node.crt + node.key
	TokenHashPath string // directory containing mesh.token hash file
}

// Server wraps a gRPC server with dual auth (mTLS + bearer token).
type Server struct {
	grpcServer *grpc.Server
	listener   net.Listener
	config     ServerConfig
	logger     zerolog.Logger
}

// NewServer constructs a Server with TLS transport and dual-auth interceptor.
func NewServer(cfg ServerConfig, handler proto.AwgAgentServer, logger zerolog.Logger) (*Server, error) {
	cert, err := pkgtls.LoadCertKey(cfg.CertPath)
	if err != nil {
		return nil, fmt.Errorf("grpc server: load cert/key: %w", err)
	}

	pool, err := pkgtls.LoadCACert(cfg.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("grpc server: load CA cert: %w", err)
	}

	tlsConfig := &tls.Config{
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}

	tokenHash, err := pkgtls.LoadTokenHash(cfg.TokenHashPath)
	if err != nil {
		return nil, fmt.Errorf("grpc server: load token hash: %w", err)
	}

	gs := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.ChainUnaryInterceptor(makeUnaryAuthInterceptor(tokenHash, logger)),
	)

	proto.RegisterAwgAgentServer(gs, handler)

	return &Server{
		grpcServer: gs,
		config:     cfg,
		logger:     logger,
	}, nil
}

// NewInsecureServer constructs a Server without transport TLS. Authentication
// is performed by bearer token via the unary interceptor.
func NewInsecureServer(cfg ServerConfig, handler proto.AwgAgentServer, logger zerolog.Logger) (*Server, error) {
	tokenHash, err := pkgtls.LoadTokenHash(cfg.TokenHashPath)
	if err != nil {
		return nil, fmt.Errorf("grpc server: load token hash: %w", err)
	}

	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(makeUnaryAuthInterceptor(tokenHash, logger)),
	)

	proto.RegisterAwgAgentServer(gs, handler)

	return &Server{
		grpcServer: gs,
		config:     cfg,
		logger:     logger,
	}, nil
}

// Start begins listening on ListenAddr and serves gRPC requests (blocking).
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("grpc server: listen %s: %w", s.config.ListenAddr, err)
	}
	s.listener = ln
	s.logger.Info().Str("addr", s.config.ListenAddr).Msg("gRPC server listening")
	return s.grpcServer.Serve(ln)
}

// Stop initiates a graceful shutdown of the gRPC server.
func (s *Server) Stop() {
	s.grpcServer.GracefulStop()
}

// makeUnaryAuthInterceptor returns a unary server interceptor that enforces
// dual-auth: mTLS certificate verification takes priority; bearer token is
// the fallback when no client certificate is presented.
func makeUnaryAuthInterceptor(tokenHash string, logger zerolog.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		// Primary: mTLS — check whether the client presented a verified cert chain.
		if p, ok := peer.FromContext(ctx); ok {
			if tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo); ok {
				if len(tlsInfo.State.VerifiedChains) > 0 {
					return handler(ctx, req)
				}
			}
		}

		// Fallback: bearer token in "authorization" metadata.
		md, ok := metadata.FromIncomingContext(ctx)
		if ok {
			if vals := md.Get("authorization"); len(vals) > 0 {
				raw := vals[0]
				if strings.HasPrefix(raw, "Bearer ") {
					token := strings.TrimPrefix(raw, "Bearer ")
					if pkgtls.VerifyToken(token, tokenHash) {
						return handler(ctx, req)
					}
					logger.Warn().Str("method", info.FullMethod).Msg("invalid bearer token")
				}
			}
		}

		return nil, status.Error(codes.Unauthenticated, "authentication required")
	}
}
