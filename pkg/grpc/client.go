package grpcserver

import (
	"context"
	"crypto/tls"
	"fmt"

	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// ClientConfig holds configuration for the gRPC client.
// Exactly one of CertPath (mTLS) or Token (bearer) must be set.
type ClientConfig struct {
	Target     string
	CACertPath string
	CertPath   string // directory containing node.crt + node.key; enables mTLS when non-empty
	Token      string // bearer token; used when CertPath is empty
	Insecure   bool   // skip TLS cert verification (for pre-Init bootstrap)
}

// Client wraps a gRPC client connection and the typed AwgAgent client.
type Client struct {
	conn  *grpc.ClientConn
	agent proto.AwgAgentClient
}

// NewClient creates a gRPC client using mTLS (if CertPath is set) or bearer
// token (if Token is set). One of the two must be provided.
func NewClient(cfg ClientConfig) (*Client, error) {
	pool, caErr := pkgtls.LoadCACert(cfg.CACertPath)

	var conn *grpc.ClientConn
	var err error

	switch {
	case cfg.Insecure && cfg.Token != "":
		// Pre-Init bootstrap: skip TLS verification, use token auth only.
		tlsCfg := &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // pre-Init bootstrap, token authenticates
			MinVersion:         tls.VersionTLS13,
		}
		conn, err = grpc.NewClient(
			cfg.Target,
			grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
			grpc.WithPerRPCCredentials(&tokenCredentials{token: cfg.Token, insecure: true}),
		)
		if err != nil {
			return nil, fmt.Errorf("grpc client: dial (insecure+token) %s: %w", cfg.Target, err)
		}

	case cfg.CertPath != "" && caErr == nil:
		cert, certErr := pkgtls.LoadCertKey(cfg.CertPath)
		if certErr != nil {
			return nil, fmt.Errorf("grpc client: load cert/key: %w", certErr)
		}
		tlsCfg := &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS13,
		}
		conn, err = grpc.NewClient(cfg.Target, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
		if err != nil {
			return nil, fmt.Errorf("grpc client: dial (mTLS) %s: %w", cfg.Target, err)
		}

	case cfg.Token != "" && caErr == nil:
		tlsCfg := &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS13,
		}
		conn, err = grpc.NewClient(
			cfg.Target,
			grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
			grpc.WithPerRPCCredentials(&tokenCredentials{token: cfg.Token}),
		)
		if err != nil {
			return nil, fmt.Errorf("grpc client: dial (token) %s: %w", cfg.Target, err)
		}

	case cfg.Token != "":
		// Pre-Init: CA cert not available yet. Connect with TLS but skip server cert verification.
		// Server has ephemeral self-signed cert. Token provides authentication.
		tlsCfg := &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // pre-Init bootstrap, token authenticates
			MinVersion:         tls.VersionTLS13,
		}
		conn, err = grpc.NewClient(
			cfg.Target,
			grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
			grpc.WithPerRPCCredentials(&tokenCredentials{token: cfg.Token, insecure: true}),
		)
		if err != nil {
			return nil, fmt.Errorf("grpc client: dial (token, insecure) %s: %w", cfg.Target, err)
		}

	default:
		return nil, fmt.Errorf("grpc client: must provide either CertPath (mTLS) or Token")
	}

	return &Client{
		conn:  conn,
		agent: proto.NewAwgAgentClient(conn),
	}, nil
}

// Agent returns the typed AwgAgentClient for making RPC calls.
func (c *Client) Agent() proto.AwgAgentClient {
	return c.agent
}

// Close releases the underlying connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// tokenCredentials implements credentials.PerRPCCredentials for bearer token auth.
type tokenCredentials struct {
	token    string
	insecure bool // true during pre-Init bootstrap (InsecureSkipVerify)
}

func (tc *tokenCredentials) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + tc.token,
	}, nil
}

func (tc *tokenCredentials) RequireTransportSecurity() bool {
	return !tc.insecure
}
