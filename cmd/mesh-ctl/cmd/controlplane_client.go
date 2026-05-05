package cmd

import (
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/tls"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	controlPlaneAdminCertDir        = "admin"
	controlPlaneAdminCertCommonName = "mesh-ctl-admin"
)

func newControlPlaneAdminConn(controlPlane, configDir string) (*grpc.ClientConn, error) {
	transportCredentials, err := controlPlaneAdminTransportCredentials(controlPlane, configDir)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(controlPlane, grpc.WithTransportCredentials(transportCredentials))
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func controlPlaneAdminTransportCredentials(controlPlane, configDir string) (credentials.TransportCredentials, error) {
	cert, err := loadOrCreateControlPlaneAdminCert(configDir)
	if err != nil {
		return nil, err
	}
	rootCAs, err := pkgtls.LoadCACert(filepath.Join(configDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("load mesh CA cert: %w", err)
	}
	return credentials.NewTLS(&tls.Config{
		RootCAs:      rootCAs,
		Certificates: []tls.Certificate{cert},
		ServerName:   controlPlaneServerName(controlPlane),
		MinVersion:   tls.VersionTLS13,
	}), nil
}

func loadOrCreateControlPlaneAdminCert(configDir string) (tls.Certificate, error) {
	if strings.TrimSpace(configDir) == "" {
		return tls.Certificate{}, fmt.Errorf("config dir is required for control-plane mTLS")
	}
	adminDir := filepath.Join(configDir, controlPlaneAdminCertDir)
	certPath := filepath.Join(adminDir, "node.crt")
	keyPath := filepath.Join(adminDir, "node.key")
	certMissing := fileMissing(certPath)
	keyMissing := fileMissing(keyPath)
	switch {
	case certMissing && keyMissing:
		caCert, caKey, err := pkgtls.LoadCA(configDir)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("load mesh CA for admin client cert: %w", err)
		}
		certPEM, keyPEM, err := pkgtls.IssueCert(caCert, caKey, controlPlaneAdminCertCommonName, []string{controlPlaneAdminCertCommonName})
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("issue admin client cert: %w", err)
		}
		if err := pkgtls.SaveCertKey(adminDir, certPEM, keyPEM); err != nil {
			return tls.Certificate{}, fmt.Errorf("save admin client cert: %w", err)
		}
	case certMissing != keyMissing:
		return tls.Certificate{}, fmt.Errorf("admin client certificate is incomplete: both %s and %s are required", certPath, keyPath)
	}
	cert, err := pkgtls.LoadCertKey(adminDir)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load admin client cert/key: %w", err)
	}
	return cert, nil
}

func controlPlaneServerName(target string) string {
	host, _, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		return "localhost"
	}
	return strings.Trim(host, "[]")
}
