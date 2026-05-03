package balancer

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

type FlowKey struct {
	Source          string
	Destination     string
	Protocol        string
	SourcePort      int
	DestinationPort int
}

func (k FlowKey) String() string {
	parts := []string{
		strings.TrimSpace(k.Source),
		strings.TrimSpace(k.Destination),
		strings.TrimSpace(k.Protocol),
		strconv.Itoa(k.SourcePort),
		strconv.Itoa(k.DestinationPort),
	}
	return strings.Join(parts, "|")
}

func (k FlowKey) empty() bool {
	return strings.TrimSpace(k.Source) == "" &&
		strings.TrimSpace(k.Destination) == "" &&
		strings.TrimSpace(k.Protocol) == "" &&
		k.SourcePort == 0 &&
		k.DestinationPort == 0
}

type DecisionRequest struct {
	FlowKey FlowKey
	DSCP    int
	FWMark  int
}

type Decision struct {
	Egress         EgressTarget
	Mode           Mode
	FlowKey        FlowKey
	Sticky         bool
	FallbackReason string
	ExpiresAt      time.Time
}

type flowEntry struct {
	egressID  string
	expiresAt time.Time
}

type Engine struct {
	mu          sync.Mutex
	cfg         Config
	registry    *Registry
	health      *HealthTracker
	metrics     *Metrics
	logger      zerolog.Logger
	labelMap    map[string]string
	flowIdle    time.Duration
	rrCursor    int
	flowEntries map[string]flowEntry
}

func NewEngine(cfg Config, logger *zerolog.Logger) (*Engine, error) {
	normalized, err := NormalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	registry, err := NewRegistry(normalized.Egresses)
	if err != nil {
		return nil, err
	}
	metrics, err := NewMetrics(nil)
	if err != nil {
		return nil, fmt.Errorf("balancer metrics: %w", err)
	}
	actualLogger := zerolog.Nop()
	if logger != nil {
		actualLogger = *logger
	}
	labelMap := make(map[string]string, len(normalized.Labels))
	for _, label := range normalized.Labels {
		labelMap[labelKey(label.Type, label.Value)] = label.EgressID
	}
	return &Engine{
		cfg:         normalized,
		registry:    registry,
		health:      NewHealthTracker(nil),
		metrics:     metrics,
		logger:      actualLogger,
		labelMap:    labelMap,
		flowIdle:    normalized.FlowIdleTimeout,
		flowEntries: make(map[string]flowEntry),
	}, nil
}

func (e *Engine) Registry() *Registry {
	if e == nil {
		return nil
	}
	return e.registry
}

func (e *Engine) Health() *HealthTracker {
	if e == nil {
		return nil
	}
	return e.health
}

func (e *Engine) Metrics() *Metrics {
	if e == nil {
		return nil
	}
	return e.metrics
}

func (e *Engine) Select(req DecisionRequest, now time.Time) (Decision, error) {
	if e == nil {
		return Decision{}, errors.New("balancer engine is nil")
	}
	if now.IsZero() {
		now = time.Now()
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	snapshot := e.registry.Snapshot()
	if snapshot == nil {
		return Decision{}, errors.New("balancer has no egress snapshot")
	}
	e.expireFlows(now)
	flowID := ""
	if !req.FlowKey.empty() {
		flowID = req.FlowKey.String()
		if entry, ok := e.flowEntries[flowID]; ok && now.Before(entry.expiresAt) {
			if egress, found := snapshot.Lookup(entry.egressID); found && e.health.IsHealthy(egress) {
				decision := e.recordDecisionLocked(req, egress, "", true, now, flowID)
				return decision, nil
			}
			delete(e.flowEntries, flowID)
		}
	}

	var fallbackReason string
	if e.cfg.Mode == ModeLabeled {
		if mapped, ok := e.mappedEgress(req); ok {
			egress, found := snapshot.Lookup(mapped)
			if !found {
				fallbackReason = "labeled-target-missing"
			} else if e.health.IsHealthy(egress) {
				decision := e.recordDecisionLocked(req, egress, "", false, now, flowID)
				return decision, nil
			} else {
				fallbackReason = "labeled-target-unhealthy"
			}
		}
	}
	egress, err := e.nextHealthyDumb(snapshot)
	if err != nil {
		return Decision{}, err
	}
	decision := e.recordDecisionLocked(req, egress, fallbackReason, false, now, flowID)
	return decision, nil
}

func (e *Engine) mappedEgress(req DecisionRequest) (string, bool) {
	if req.DSCP > 0 {
		if id, ok := e.labelMap[labelKey(LabelDSCP, req.DSCP)]; ok {
			return id, true
		}
	}
	if req.FWMark > 0 {
		if id, ok := e.labelMap[labelKey(LabelFWMark, req.FWMark)]; ok {
			return id, true
		}
	}
	return "", false
}

func (e *Engine) nextHealthyDumb(snapshot *Snapshot) (EgressTarget, error) {
	weighted := snapshot.Weighted()
	if len(weighted) == 0 {
		return EgressTarget{}, errors.New("balancer has no weighted egress candidates")
	}
	for i := 0; i < len(weighted); i++ {
		index := e.rrCursor % len(weighted)
		e.rrCursor++
		candidate := weighted[index]
		if e.health.IsHealthy(candidate) {
			return candidate, nil
		}
	}
	return EgressTarget{}, errors.New("balancer has no healthy egress targets")
}

func (e *Engine) recordDecisionLocked(req DecisionRequest, egress EgressTarget, fallbackReason string, sticky bool, now time.Time, flowID string) Decision {
	expiresAt := now.Add(e.flowIdle)
	if flowID != "" {
		e.flowEntries[flowID] = flowEntry{egressID: egress.ID, expiresAt: expiresAt}
	}
	decision := Decision{
		Egress:         egress,
		Mode:           e.cfg.Mode,
		FlowKey:        req.FlowKey,
		Sticky:         sticky,
		FallbackReason: fallbackReason,
		ExpiresAt:      expiresAt,
	}
	if e.metrics != nil {
		e.metrics.RecordDecision(e.cfg.Mode, egress, fallbackReason != "", fallbackReason)
		e.metrics.SetActiveFlows(len(e.flowEntries))
	}
	logDecisionEvent(e.logger, e.cfg.Name, decision)
	return decision
}

func (e *Engine) expireFlows(now time.Time) {
	for key, entry := range e.flowEntries {
		if !now.Before(entry.expiresAt) {
			delete(e.flowEntries, key)
		}
	}
}
