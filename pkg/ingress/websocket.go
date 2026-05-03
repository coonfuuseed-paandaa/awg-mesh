package ingress

import (
	"context"
	"fmt"
	"net/http"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"
)

// WebSocketProxy bridges public WebSocket connections to overlay targets.
type WebSocketProxy struct {
	registry *Registry
	health   *HealthTracker
	metrics  *Metrics
	logger   zerolog.Logger
}

func NewWebSocketProxy(registry *Registry, health *HealthTracker, metrics *Metrics, logger zerolog.Logger) *WebSocketProxy {
	return &WebSocketProxy{registry: registry, health: health, metrics: metrics, logger: logger}
}

func (p *WebSocketProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hostname := stripPort(r.Host)
	route, ok := p.registry.Lookup(hostname)
	if !ok {
		if p.metrics != nil {
			p.metrics.RecordRejection(ProtocolWebSocket, hostname, "unknown-host")
		}
		logRejectEvent(p.logger, EventRequestRejected, hostname, ProtocolWebSocket, "unknown-host")
		http.Error(w, "unknown ingress hostname", http.StatusMisdirectedRequest)
		return
	}
	if route.Protocol != ProtocolWebSocket {
		if p.metrics != nil {
			p.metrics.RecordRejection(ProtocolWebSocket, route.Hostname, "protocol-mismatch")
		}
		logRejectEvent(p.logger, EventRequestRejected, hostname, ProtocolWebSocket, "protocol-mismatch")
		http.Error(w, "ingress protocol mismatch", http.StatusBadGateway)
		return
	}
	if p.health != nil && !p.health.IsHealthy(route) {
		if p.metrics != nil {
			p.metrics.RecordProxyError(route, "target-unhealthy")
		}
		logRouteEvent(p.logger, EventProxyError, route, ProtocolWebSocket, "target-unhealthy")
		http.Error(w, "target unhealthy", http.StatusBadGateway)
		return
	}
	clientConn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = clientConn.Close(websocket.StatusNormalClosure, "") }()

	targetURL := "ws://" + route.Target + r.URL.RequestURI()
	targetConn, _, err := websocket.Dial(r.Context(), targetURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Host": []string{r.Host}},
	})
	if err != nil {
		if p.metrics != nil {
			p.metrics.RecordProxyError(route, "target-dial")
		}
		logRouteEvent(p.logger, EventProxyError, route, ProtocolWebSocket, "target-dial")
		_ = clientConn.Close(websocket.StatusTryAgainLater, "target unavailable")
		return
	}
	defer func() { _ = targetConn.Close(websocket.StatusNormalClosure, "") }()

	if p.metrics != nil {
		p.metrics.RecordRequest(route)
	}
	logRouteEvent(p.logger, EventRequestAccepted, route, ProtocolWebSocket, "accept")
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	errCh := make(chan error, 2)
	go func() { errCh <- copyWebSocket(ctx, targetConn, clientConn) }()
	go func() { errCh <- copyWebSocket(ctx, clientConn, targetConn) }()
	if err := <-errCh; err != nil && p.metrics != nil {
		p.metrics.RecordProxyError(route, "copy-error")
	}
	cancel()
}

func copyWebSocket(ctx context.Context, dst, src *websocket.Conn) error {
	for {
		messageType, data, err := src.Read(ctx)
		if err != nil {
			return err
		}
		if err := dst.Write(ctx, messageType, data); err != nil {
			return fmt.Errorf("write websocket message: %w", err)
		}
	}
}
