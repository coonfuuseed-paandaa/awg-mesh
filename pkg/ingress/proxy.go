package ingress

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/rs/zerolog"
)

// HTTPProxy routes public HTTP requests to overlay targets.
type HTTPProxy struct {
	registry *Registry
	health   *HealthTracker
	metrics  *Metrics
	logger   zerolog.Logger
}

func NewHTTPProxy(registry *Registry, health *HealthTracker, metrics *Metrics, logger zerolog.Logger) *HTTPProxy {
	return &HTTPProxy{registry: registry, health: health, metrics: metrics, logger: logger}
}

func (p *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hostname := stripPort(r.Host)
	route, ok := p.registry.Lookup(hostname)
	if !ok {
		if p.metrics != nil {
			p.metrics.RecordRejection(ProtocolHTTP, hostname, "unknown-host")
		}
		logRejectEvent(p.logger, EventRequestRejected, hostname, ProtocolHTTP, "unknown-host")
		http.Error(w, "unknown ingress hostname", http.StatusMisdirectedRequest)
		return
	}
	if route.Protocol != ProtocolHTTP && route.Protocol != ProtocolWebSocket && route.Protocol != ProtocolTCP {
		if p.metrics != nil {
			p.metrics.RecordRejection(route.Protocol, route.Hostname, "protocol-mismatch")
		}
		logRejectEvent(p.logger, EventRequestRejected, hostname, route.Protocol, "protocol-mismatch")
		http.Error(w, "ingress protocol mismatch", http.StatusBadGateway)
		return
	}
	if p.health != nil && !p.health.IsHealthy(route) {
		if p.metrics != nil {
			p.metrics.RecordProxyError(route, "target-unhealthy")
		}
		logRouteEvent(p.logger, EventProxyError, route, route.Protocol, "target-unhealthy")
		http.Error(w, "target unhealthy", http.StatusBadGateway)
		return
	}
	targetURL := &url.URL{Scheme: "http", Host: route.Target}
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	originalDirector := proxy.Director
	proxy.Director = func(out *http.Request) {
		originalDirector(out)
		out.Host = r.Host
		out.URL.Host = route.Target
		out.URL.Scheme = "http"
		setForwardedHeaders(out, r)
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		if p.metrics != nil {
			p.metrics.RecordProxyError(route, "proxy-error")
		}
		logRouteEvent(p.logger, EventProxyError, route, route.Protocol, "proxy-error")
		http.Error(w, "proxy error", http.StatusBadGateway)
	}
	if p.metrics != nil {
		p.metrics.RecordRequest(route)
	}
	logRouteEvent(p.logger, EventRequestAccepted, route, route.Protocol, "accept")
	proxy.ServeHTTP(w, r)
}

func setForwardedHeaders(out, in *http.Request) {
	out.Header.Set("X-Forwarded-Host", in.Host)
	proto := "http"
	if in.TLS != nil {
		proto = "https"
	}
	if forwardedProto := strings.TrimSpace(in.Header.Get("X-Forwarded-Proto")); forwardedProto != "" {
		proto = forwardedProto
	}
	out.Header.Set("X-Forwarded-Proto", proto)
}
