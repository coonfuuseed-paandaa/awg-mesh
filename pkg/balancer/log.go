package balancer

import "github.com/rs/zerolog"

const (
	EventDecision       = "balancer.decision"
	EventRuntimeStarted = "balancer.runtime.started"
	EventRuntimeStopped = "balancer.runtime.stopped"
)

func logDecisionEvent(logger zerolog.Logger, node string, decision Decision) {
	event := logger.Info().
		Str("event", EventDecision).
		Str("node", node).
		Str("policy_mode", string(decision.Mode)).
		Str("flow_key", decision.FlowKey.String()).
		Str("selected_egress", decision.Egress.ID).
		Str("target", decision.Egress.Target).
		Bool("sticky", decision.Sticky).
		Str("decision", "select")
	if decision.FallbackReason != "" {
		event = event.Str("fallback_reason", decision.FallbackReason)
	}
	event.Msg(EventDecision)
}
