package ingress

import "github.com/rs/zerolog"

const (
	EventRequestAccepted = "ingress.request.accepted"
	EventRequestRejected = "ingress.request.rejected"
	EventProxyError      = "ingress.proxy.error"
	EventTargetHealth    = "ingress.target.health"
	EventRuntimeStarted  = "ingress.runtime.started"
	EventRuntimeStopped  = "ingress.runtime.stopped"
)

func logRouteEvent(logger zerolog.Logger, event string, route Route, protocol Protocol, decision string) {
	logger.Info().
		Str("event", event).
		Str("tenant", route.Tenant).
		Str("hostname", route.Hostname).
		Str("target", route.Target).
		Str("protocol", string(protocol)).
		Str("decision", decision).
		Msg(event)
}

func logRejectEvent(logger zerolog.Logger, event string, hostname string, protocol Protocol, reason string) {
	logger.Warn().
		Str("event", event).
		Str("hostname", hostname).
		Str("protocol", string(protocol)).
		Str("decision", "reject").
		Str("reason", reason).
		Msg(event)
}
