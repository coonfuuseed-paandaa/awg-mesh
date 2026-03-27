package grpcserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"

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

// NewDynamicServer constructs a Server that refreshes TLS materials on each
// connection. It starts with ephemeral certificate fallback until real certs are
// written to disk and rotates by reading files from disk on demand.
func NewDynamicServer(cfg ServerConfig, handler proto.AwgAgentServer, logger zerolog.Logger) (*Server, error) {
	tokenHash, err := pkgtls.LoadTokenHash(cfg.TokenHashPath)
	if err != nil {
		return nil, fmt.Errorf("grpc server: load token hash: %w", err)
	}

	provider := newDynamicCertificateProvider(cfg.CertPath, cfg.CACertPath, logger)
	certificate := provider.getServerCertificate
	clientConfig := provider.getClientConfig

	tlsConfig := &tls.Config{
		GetCertificate:     certificate,
		GetConfigForClient: clientConfig,
		MinVersion:         tls.VersionTLS13,
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

type dynamicCertificateProvider struct {
	certPath     string
	caPath       string
	logger       zerolog.Logger
	fallback     *tls.Certificate
	fallbackLock sync.Mutex
}

func newDynamicCertificateProvider(certPath, caPath string, logger zerolog.Logger) *dynamicCertificateProvider {
	return &dynamicCertificateProvider{
		certPath: certPath,
		caPath:   caPath,
		logger:   logger,
	}
}

func (p *dynamicCertificateProvider) getServerCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert, err := pkgtls.LoadCertKey(p.certPath)
	if err == nil {
		return &cert, nil
	}
	p.logger.Warn().Err(err).Str("cert_path", p.certPath).Msg("using fallback ephemeral certificate")

	fallbackCert, fallbackErr := p.getFallbackCertificate()
	if fallbackErr != nil {
		return nil, fmt.Errorf("grpc server: generate fallback certificate: %w", fallbackErr)
	}
	return fallbackCert, nil
}

func (p *dynamicCertificateProvider) getClientConfig(_ *tls.ClientHelloInfo) (*tls.Config, error) {
	pool, err := pkgtls.LoadCACert(p.caPath)
	if err != nil {
		return &tls.Config{
			ClientAuth: tls.NoClientCert,
			MinVersion: tls.VersionTLS13,
		}, nil
	}

	return &tls.Config{
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  pool,
		MinVersion: tls.VersionTLS13,
	}, nil
}

func (p *dynamicCertificateProvider) getFallbackCertificate() (*tls.Certificate, error) {
	p.fallbackLock.Lock()
	defer p.fallbackLock.Unlock()
	if p.fallback != nil {
		return p.fallback, nil
	}

	fallback, err := generateSelfSignedServerCert()
	if err != nil {
		return nil, err
	}
	p.fallback = fallback
	return p.fallback, nil
}

func generateSelfSignedServerCert() (*tls.Certificate, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate private key: %w", err)
	}

	now := time.Now()
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 62)
	serialNumber, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "awg-mesh-node",
		},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(12 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(
		rand.Reader,
		&template,
		&template,
		&privateKey.PublicKey,
		privateKey,
	)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: derBytes,
	})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: keyBytes,
	})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("pair cert and key: %w", err)
	}
	return &cert, nil
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
