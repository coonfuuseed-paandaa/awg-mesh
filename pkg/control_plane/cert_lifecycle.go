package control_plane

import (
	"crypto"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	pb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
)

const (
	defaultCertRotationDays = 90
	defaultCertRenewBefore  = 7 * 24 * time.Hour
	defaultCertPollInterval = time.Hour
)

// CertIssuer creates replacement node credentials for certificate lifecycle updates.
type CertIssuer interface {
	IssueNodeCert(node RegisteredNode, validFor time.Duration) ([]byte, []byte, *x509.Certificate, error)
}

// CAIssuer issues node credentials from an in-memory mesh CA.
type CAIssuer struct {
	CACert *x509.Certificate
	CAKey  crypto.PrivateKey
}

func (i CAIssuer) IssueNodeCert(node RegisteredNode, validFor time.Duration) ([]byte, []byte, *x509.Certificate, error) {
	if i.CACert == nil || i.CAKey == nil {
		return nil, nil, nil, errors.New("cert issuer CA material is required")
	}
	certPEM, keyPEM, err := pkgtls.IssueCertWithValidity(i.CACert, i.CAKey, node.Name, certHostsForNode(node), validFor)
	if err != nil {
		return nil, nil, nil, err
	}
	cert, err := pkgtls.ParseCertPEM(certPEM)
	if err != nil {
		return nil, nil, nil, err
	}
	return certPEM, keyPEM, cert, nil
}

// CertLifecycleConfig controls FR-16 renewal timing.
type CertLifecycleConfig struct {
	RotationDays int
	RenewBefore  time.Duration
	PollInterval time.Duration
	Now          func() time.Time
}

// CertLifecycle decides when a registered node needs replacement mTLS credentials.
type CertLifecycle struct {
	issuer       CertIssuer
	validFor     time.Duration
	renewBefore  time.Duration
	pollInterval time.Duration
	now          func() time.Time
}

func NewCertLifecycle(issuer CertIssuer, cfg CertLifecycleConfig) (*CertLifecycle, error) {
	if issuer == nil {
		return nil, errors.New("cert issuer is required")
	}
	rotationDays := cfg.RotationDays
	if rotationDays <= 0 {
		rotationDays = defaultCertRotationDays
	}
	renewBefore := cfg.RenewBefore
	if renewBefore <= 0 {
		renewBefore = defaultCertRenewBefore
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultCertPollInterval
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &CertLifecycle{
		issuer:       issuer,
		validFor:     time.Duration(rotationDays) * 24 * time.Hour,
		renewBefore:  renewBefore,
		pollInterval: pollInterval,
		now:          now,
	}, nil
}

func (c *CertLifecycle) NextDueUpdate(node RegisteredNode) (*pb.CertUpdate, time.Time, bool, error) {
	current, err := pkgtls.ParseCertPEM(node.NodeCertPEM)
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("parse current node certificate for %q: %w", node.Name, err)
	}
	if current.Subject.CommonName != node.Name {
		return nil, time.Time{}, false, fmt.Errorf("node certificate CN %q does not match node %q", current.Subject.CommonName, node.Name)
	}
	now := c.now().UTC()
	if now.Add(c.renewBefore).Before(current.NotAfter) {
		return nil, current.NotAfter, false, nil
	}
	certPEM, keyPEM, cert, err := c.issuer.IssueNodeCert(node, c.validFor)
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("issue replacement node certificate for %q: %w", node.Name, err)
	}
	return &pb.CertUpdate{
		CertPem:        certPEM,
		KeyPem:         keyPEM,
		ValidFromUnix:  cert.NotBefore.Unix(),
		ValidUntilUnix: cert.NotAfter.Unix(),
	}, current.NotAfter, true, nil
}

func (c *CertLifecycle) DelayUntilDue(node RegisteredNode) time.Duration {
	current, err := pkgtls.ParseCertPEM(node.NodeCertPEM)
	if err != nil {
		return 0
	}
	delay := current.NotAfter.Add(-c.renewBefore).Sub(c.now().UTC())
	if delay < 0 {
		return 0
	}
	if delay > c.pollInterval {
		return c.pollInterval
	}
	return delay
}

func certHostsForNode(node RegisteredNode) []string {
	hosts := []string{node.Name}
	if node.OverlayIP != "" {
		hosts = append(hosts, node.OverlayIP)
	}
	return hosts
}
