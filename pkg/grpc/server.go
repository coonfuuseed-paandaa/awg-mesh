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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
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

	if _, err := pkgtls.LoadTokenHash(cfg.TokenHashPath); err != nil {
		return nil, fmt.Errorf("grpc server: load token hash: %w", err)
	}

	gs := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.ChainUnaryInterceptor(makeUnaryAuthInterceptor(newTokenHashProvider(cfg.TokenHashPath), logger)),
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
	if _, err := pkgtls.LoadTokenHash(cfg.TokenHashPath); err != nil {
		return nil, fmt.Errorf("grpc server: load token hash: %w", err)
	}

	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(makeUnaryAuthInterceptor(newTokenHashProvider(cfg.TokenHashPath), logger)),
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
	if _, err := pkgtls.LoadTokenHash(cfg.TokenHashPath); err != nil {
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
		grpc.ChainUnaryInterceptor(makeUnaryAuthInterceptor(newTokenHashProvider(cfg.TokenHashPath), logger)),
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

	certCache         *tls.Certificate
	certCacheCrtMtime time.Time
	certCacheKeyMtime time.Time
	certCacheLock     sync.Mutex
}

func newDynamicCertificateProvider(certPath, caPath string, logger zerolog.Logger) *dynamicCertificateProvider {
	return &dynamicCertificateProvider{
		certPath: certPath,
		caPath:   caPath,
		logger:   logger,
	}
}

func (p *dynamicCertificateProvider) getServerCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	certFile := filepath.Join(p.certPath, "node.crt")
	keyFile := filepath.Join(p.certPath, "node.key")

	// Check mtime-based cache to avoid disk read + PEM parse on every handshake.
	// Both node.crt and node.key mtimes must match; if either Stat fails we treat
	// the cached certificate as last-known-good rather than falling back to a
	// self-signed ephemeral cert.
	p.certCacheLock.Lock()
	if p.certCache != nil {
		certInfo, certErr := os.Stat(certFile)
		keyInfo, keyErr := os.Stat(keyFile)
		if certErr != nil || keyErr != nil {
			// Temporary FS error — return the cached cert as last-known-good.
			cached := p.certCache
			p.certCacheLock.Unlock()
			return cached, nil
		}
		if certInfo.ModTime().Equal(p.certCacheCrtMtime) && keyInfo.ModTime().Equal(p.certCacheKeyMtime) {
			cached := p.certCache
			p.certCacheLock.Unlock()
			return cached, nil
		}
	}
	p.certCacheLock.Unlock()

	cert, err := pkgtls.LoadCertKey(p.certPath)
	if err == nil {
		p.certCacheLock.Lock()
		p.certCache = &cert
		if certInfo, statErr := os.Stat(certFile); statErr == nil {
			p.certCacheCrtMtime = certInfo.ModTime()
		}
		if keyInfo, statErr := os.Stat(keyFile); statErr == nil {
			p.certCacheKeyMtime = keyInfo.ModTime()
		}
		p.certCacheLock.Unlock()
		return &cert, nil
	}

	// Load failed — return last-known-good cached cert before falling back to ephemeral.
	p.certCacheLock.Lock()
	if p.certCache != nil {
		cached := p.certCache
		p.certCacheLock.Unlock()
		return cached, nil
	}
	p.certCacheLock.Unlock()

	p.logger.Warn().Err(err).Str("cert_path", p.certPath).Msg("using fallback ephemeral certificate")

	fallbackCert, fallbackErr := p.getFallbackCertificate()
	if fallbackErr != nil {
		return nil, fmt.Errorf("grpc server: generate fallback certificate: %w", fallbackErr)
	}
	return fallbackCert, nil
}

func (p *dynamicCertificateProvider) getClientConfig(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	// GetConfigForClient replaces the entire TLS config for this connection.
	// We must include GetCertificate so the server cert is still available.
	pool, err := pkgtls.LoadCACert(p.caPath)
	if err != nil {
		return &tls.Config{
			GetCertificate: p.getServerCertificate,
			ClientAuth:     tls.NoClientCert,
			MinVersion:     tls.VersionTLS13,
		}, nil
	}

	return &tls.Config{
		GetCertificate: p.getServerCertificate,
		ClientAuth:     tls.VerifyClientCertIfGiven,
		ClientCAs:      pool,
		MinVersion:     tls.VersionTLS13,
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
// tokenHashProvider loads the current token hash from disk, enabling hot
// rotation without server restart. The provider caches the hash and reloads
// only when the file modification time changes.
func newTokenHashProvider(hashDir string) func() string {
	var (
		mu          sync.Mutex
		cached      string
		cachedMtime time.Time
	)

	path := filepath.Join(hashDir, "mesh.token")
	// Initial load.
	if h, err := pkgtls.LoadTokenHash(hashDir); err == nil {
		cached = h
	}
	if info, err := os.Stat(path); err == nil {
		cachedMtime = info.ModTime()
	}

	return func() string {
		mu.Lock()
		defer mu.Unlock()

		info, err := os.Stat(path)
		if err != nil {
			return cached
		}
		if info.ModTime().Equal(cachedMtime) {
			return cached
		}
		if h, err := pkgtls.LoadTokenHash(hashDir); err == nil {
			cached = h
			cachedMtime = info.ModTime()
		}
		return cached
	}
}

func makeUnaryAuthInterceptor(tokenProvider func() string, logger zerolog.Logger) grpc.UnaryServerInterceptor {
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
					if pkgtls.VerifyToken(token, tokenProvider()) {
						return handler(ctx, req)
					}
					logger.Warn().Str("method", info.FullMethod).Msg("invalid bearer token")
				}
			}
		}

		return nil, status.Error(codes.Unauthenticated, "authentication required")
	}
}
