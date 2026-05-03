package control_plane

import (
	"crypto"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"

	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	pb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
)

const (
	defaultCertRotationDays   = 90
	defaultCertRenewBefore    = 7 * 24 * time.Hour
	defaultCertPollInterval   = time.Hour
	defaultCertPromotionGrace = time.Hour
)

// CertIssuer creates replacement node credentials for certificate lifecycle updates.
type CertIssuer interface {
	IssueNodeCert(node RegisteredNode, hosts []string, validFor time.Duration) ([]byte, []byte, *x509.Certificate, error)
}

// CAIssuer issues node credentials from an in-memory mesh CA.
type CAIssuer struct {
	CACert *x509.Certificate
	CAKey  crypto.PrivateKey
}

func (i CAIssuer) IssueNodeCert(node RegisteredNode, hosts []string, validFor time.Duration) ([]byte, []byte, *x509.Certificate, error) {
	if i.CACert == nil || i.CAKey == nil {
		return nil, nil, nil, errors.New("cert issuer CA material is required")
	}
	certPEM, keyPEM, err := pkgtls.IssueCertWithValidity(i.CACert, i.CAKey, node.Name, hosts, validFor)
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
	if hasActivePendingCert(node, now) {
		return nil, current.NotAfter, false, nil
	}
	if now.Add(c.renewBefore).Before(current.NotAfter) {
		return nil, current.NotAfter, false, nil
	}
	promotionDeadline := current.NotAfter
	if minDeadline := now.Add(defaultCertPromotionGrace); promotionDeadline.Before(minDeadline) {
		promotionDeadline = minDeadline
	}
	certPEM, keyPEM, cert, err := c.issuer.IssueNodeCert(node, certHostsForNode(node, current), c.validFor)
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("issue replacement node certificate for %q: %w", node.Name, err)
	}
	return &pb.CertUpdate{
		CertPem:        certPEM,
		KeyPem:         keyPEM,
		ValidFromUnix:  cert.NotBefore.Unix(),
		ValidUntilUnix: cert.NotAfter.Unix(),
	}, promotionDeadline, true, nil
}

func (c *CertLifecycle) DelayUntilDue(node RegisteredNode) time.Duration {
	current, err := pkgtls.ParseCertPEM(node.NodeCertPEM)
	if err != nil {
		return 0
	}
	now := c.now().UTC()
	if hasActivePendingCert(node, now) {
		return c.pollInterval
	}
	delay := current.NotAfter.Add(-c.renewBefore).Sub(now)
	if delay < 0 {
		return 0
	}
	if delay > c.pollInterval {
		return c.pollInterval
	}
	return delay
}

func hasActivePendingCert(node RegisteredNode, now time.Time) bool {
	if len(node.PendingCertPEM) == 0 {
		return false
	}
	return !node.CertOverlapUntil.IsZero() && node.CertOverlapUntil.After(now.UTC())
}

func certHostsForNode(node RegisteredNode, current *x509.Certificate) []string {
	hosts := make([]string, 0)
	seen := make(map[string]struct{})
	if current != nil {
		addUniqueHost(&hosts, seen, current.Subject.CommonName)
		for _, dnsName := range current.DNSNames {
			addUniqueHost(&hosts, seen, dnsName)
		}
		for _, ip := range current.IPAddresses {
			addUniqueHost(&hosts, seen, ip.String())
		}
	}
	addUniqueHost(&hosts, seen, node.Name)
	if node.OverlayIP != "" {
		addUniqueHost(&hosts, seen, node.OverlayIP)
	}
	return hosts
}

func addUniqueHost(hosts *[]string, seen map[string]struct{}, host string) {
	if host == "" {
		return
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	if _, ok := seen[host]; ok {
		return
	}
	seen[host] = struct{}{}
	*hosts = append(*hosts, host)
}
