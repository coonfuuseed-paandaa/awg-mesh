package grpcserver

import (
	"context"
	"crypto/tls"
	"fmt"

	pkgtls "github.com/thebtf/awg-mesh/pkg/tls"
	proto "github.com/thebtf/awg-mesh/proto"
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
}

// Client wraps a gRPC client connection and the typed AwgAgent client.
type Client struct {
	conn  *grpc.ClientConn
	agent proto.AwgAgentClient
}

// NewClient creates a gRPC client using mTLS (if CertPath is set) or bearer
// token (if Token is set). One of the two must be provided.
func NewClient(cfg ClientConfig) (*Client, error) {
	pool, err := pkgtls.LoadCACert(cfg.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("grpc client: load CA cert: %w", err)
	}

	var conn *grpc.ClientConn

	switch {
	case cfg.CertPath != "":
		cert, err := pkgtls.LoadCertKey(cfg.CertPath)
		if err != nil {
			return nil, fmt.Errorf("grpc client: load cert/key: %w", err)
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

	case cfg.Token != "":
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
	token string
}

func (tc *tokenCredentials) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + tc.token,
	}, nil
}

func (tc *tokenCredentials) RequireTransportSecurity() bool {
	return true
}
