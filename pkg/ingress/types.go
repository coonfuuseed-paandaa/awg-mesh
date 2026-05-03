package ingress

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultTenant              = "default"
	DefaultHealthProbeInterval = 5 * time.Second
	DefaultUDPIdleTimeout      = 30 * time.Second
)

type TLSMode string

const (
	TLSModeSNIPassthrough TLSMode = "sni_passthrough"
	TLSModeTLSTerminate   TLSMode = "tls_terminate"
	TLSModeTCPForward     TLSMode = "tcp_forward"
	TLSModeUDPForward     TLSMode = "udp_forward"
)

type Protocol string

const (
	ProtocolHTTP      Protocol = "http"
	ProtocolWebSocket Protocol = "websocket"
	ProtocolTCP       Protocol = "tcp"
	ProtocolUDP       Protocol = "udp"
)

// Route maps one public hostname to one overlay service endpoint.
type Route struct {
	Tenant   string
	Hostname string
	Target   string
	Mode     TLSMode
	Protocol Protocol
	HTTP3    bool
}

// Config describes the ingress role runtime.
type Config struct {
	Name                string
	OverlayIP           string
	PublicAddress       string
	Routes              []Route
	HealthProbeInterval time.Duration
	UDPIdleTimeout      time.Duration
	MetricsAddress      string
	ACMECacheDir        string
	ACMEEmail           string
	EnableHTTP3         bool
}

// Plan is the dry-run-safe observable ingress startup plan.
type Plan struct {
	Name                string
	OverlayIP           string
	PublicAddress       string
	RouteCount          int
	Routes              []Route
	HealthProbeInterval time.Duration
	UDPIdleTimeout      time.Duration
	MetricsAddress      string
	ACMEEnabled         bool
	HTTP3Enabled        bool
}

// NormalizeConfig returns a validated copy of cfg with defaults applied.
func NormalizeConfig(cfg Config) (Config, error) {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		return Config{}, errors.New("ingress name is required")
	}
	overlay := strings.TrimSpace(cfg.OverlayIP)
	if overlay == "" {
		return Config{}, errors.New("ingress overlay IP is required")
	}
	if _, err := netip.ParseAddr(overlay); err != nil {
		return Config{}, fmt.Errorf("parse ingress overlay IP %q: %w", overlay, err)
	}
	publicAddress := strings.TrimSpace(cfg.PublicAddress)
	if err := validateListenAddress(publicAddress); err != nil {
		return Config{}, err
	}
	if len(cfg.Routes) == 0 {
		return Config{}, errors.New("ingress requires at least one route")
	}
	healthInterval := cfg.HealthProbeInterval
	if healthInterval <= 0 {
		healthInterval = DefaultHealthProbeInterval
	}
	udpIdle := cfg.UDPIdleTimeout
	if udpIdle <= 0 {
		udpIdle = DefaultUDPIdleTimeout
	}
	routes := make([]Route, 0, len(cfg.Routes))
	for i, route := range cfg.Routes {
		normalized, err := NormalizeRoute(route)
		if err != nil {
			return Config{}, fmt.Errorf("ingress route %d: %w", i, err)
		}
		routes = append(routes, normalized)
	}
	if _, err := NewSnapshot(routes); err != nil {
		return Config{}, err
	}
	return Config{
		Name:                name,
		OverlayIP:           overlay,
		PublicAddress:       publicAddress,
		Routes:              routes,
		HealthProbeInterval: healthInterval,
		UDPIdleTimeout:      udpIdle,
		MetricsAddress:      strings.TrimSpace(cfg.MetricsAddress),
		ACMECacheDir:        strings.TrimSpace(cfg.ACMECacheDir),
		ACMEEmail:           strings.TrimSpace(cfg.ACMEEmail),
		EnableHTTP3:         cfg.EnableHTTP3,
	}, nil
}

// NormalizeRoute returns a validated copy of route with canonical fields.
func NormalizeRoute(route Route) (Route, error) {
	hostname, err := normalizeHostname(route.Hostname)
	if err != nil {
		return Route{}, err
	}
	target := strings.TrimSpace(route.Target)
	if err := validateTarget(target); err != nil {
		return Route{}, err
	}
	mode := route.Mode
	if mode == "" {
		mode = TLSModeTLSTerminate
	}
	if err := validateTLSMode(mode); err != nil {
		return Route{}, err
	}
	protocol := route.Protocol
	if protocol == "" {
		protocol = protocolForMode(mode)
	}
	if err := validateProtocol(protocol); err != nil {
		return Route{}, err
	}
	tenant := strings.TrimSpace(route.Tenant)
	if tenant == "" {
		tenant = DefaultTenant
	}
	return Route{
		Tenant:   tenant,
		Hostname: hostname,
		Target:   target,
		Mode:     mode,
		Protocol: protocol,
		HTTP3:    route.HTTP3,
	}, nil
}

func PlanConfig(cfg Config) (Plan, error) {
	normalized, err := NormalizeConfig(cfg)
	if err != nil {
		return Plan{}, err
	}
	return Plan{
		Name:                normalized.Name,
		OverlayIP:           normalized.OverlayIP,
		PublicAddress:       normalized.PublicAddress,
		RouteCount:          len(normalized.Routes),
		Routes:              append([]Route(nil), normalized.Routes...),
		HealthProbeInterval: normalized.HealthProbeInterval,
		UDPIdleTimeout:      normalized.UDPIdleTimeout,
		MetricsAddress:      normalized.MetricsAddress,
		ACMEEnabled:         normalized.ACMECacheDir != "",
		HTTP3Enabled:        normalized.ACMECacheDir != "" && (normalized.EnableHTTP3 || anyRouteHTTP3(normalized.Routes)),
	}, nil
}

func anyRouteHTTP3(routes []Route) bool {
	for _, route := range routes {
		if route.HTTP3 {
			return true
		}
	}
	return false
}

func validateListenAddress(addr string) error {
	if strings.TrimSpace(addr) == "" {
		return errors.New("ingress public bind address is required")
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("ingress public bind address %q must be host:port: %w", addr, err)
	}
	if host != "" && net.ParseIP(host) == nil && strings.ContainsAny(host, "/\\") {
		return fmt.Errorf("ingress public bind host %q is invalid", host)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("ingress public bind port %q is invalid", portText)
	}
	return nil
}

func normalizeHostname(hostname string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(hostname))
	if host == "" {
		return "", errors.New("hostname is required")
	}
	if len(host) > 253 {
		return "", fmt.Errorf("hostname %q is too long", hostname)
	}
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return "", fmt.Errorf("hostname %q must not start or end with dot", hostname)
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return "", fmt.Errorf("hostname %q has invalid label length", hostname)
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", fmt.Errorf("hostname %q label %q must not start or end with hyphen", hostname, label)
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return "", fmt.Errorf("hostname %q contains invalid character %q", hostname, r)
		}
	}
	return host, nil
}

func validateTarget(target string) error {
	if target == "" {
		return errors.New("target endpoint is required")
	}
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("target endpoint %q must be host:port: %w", target, err)
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return fmt.Errorf("target endpoint host %q must be an overlay IP: %w", host, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("target endpoint port %q is invalid", portText)
	}
	return nil
}

func validateTLSMode(mode TLSMode) error {
	switch mode {
	case TLSModeSNIPassthrough, TLSModeTLSTerminate, TLSModeTCPForward, TLSModeUDPForward:
		return nil
	default:
		return fmt.Errorf("unsupported ingress TLS mode %q", mode)
	}
}

func validateProtocol(protocol Protocol) error {
	switch protocol {
	case ProtocolHTTP, ProtocolWebSocket, ProtocolTCP, ProtocolUDP:
		return nil
	default:
		return fmt.Errorf("unsupported ingress protocol %q", protocol)
	}
}

func protocolForMode(mode TLSMode) Protocol {
	switch mode {
	case TLSModeUDPForward:
		return ProtocolUDP
	case TLSModeSNIPassthrough, TLSModeTCPForward:
		return ProtocolTCP
	default:
		return ProtocolHTTP
	}
}
