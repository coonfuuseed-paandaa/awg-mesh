package rotation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
	controlpb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
)

const (
	// Mesh-wide rotation tiers use the control-plane string form so operator
	// logs and gRPC payloads share one vocabulary.
	Tier1 = "tier1"
	Tier2 = "tier2"
	Tier3 = "tier3"

	RotationStatusSucceeded     = "succeeded"
	RotationStatusPartialFailed = "partial_failed"

	DefaultApplyLeadTime = 30 * time.Second
)

var (
	ErrNoRotationTargets = errors.New("rotation: no mesh-internal targets")
	ErrPartialApply      = errors.New("rotation: partial apply failure")
)

// Target is one mesh-internal node that should receive a mesh-wide rotation.
type Target struct {
	Name      string
	Roles     []role.Role
	OverlayIP string
}

// Request is the operator/control-plane input for one mesh-wide rotation.
type Request struct {
	Tier       string
	Params     *controlpb.AWGParamsV2
	ApplyAt    time.Time
	RotationID string
	Reason     string
	Targets    []Target
}

// Plan is a validated, immutable rotation plan.
type Plan struct {
	Tier       string
	Params     *controlpb.AWGParamsV2
	ApplyAt    time.Time
	RotationID string
	Reason     string
	Targets    []Target
}

// Result is one target's final apply outcome.
type Result struct {
	Target Target
	Ack    bool
	Error  string
}

// Execution is the full outcome for a mesh-wide rotation.
type Execution struct {
	Plan    Plan
	Results []Result
	Status  string
}

// Record is an append-only history entry.
type Record struct {
	Plan      Plan
	Results   []Result
	Status    string
	CreatedAt time.Time
}

// Applier applies a validated rotation plan to one target.
type Applier interface {
	ApplyRotation(ctx context.Context, target Target, plan Plan) error
}

// Orchestrator validates, applies, and records mesh-wide rotation attempts.
type Orchestrator struct {
	applier Applier
	history *History
	clock   func() time.Time
	lead    time.Duration
}

// OrchestratorConfig configures deterministic test hooks for the orchestrator.
type OrchestratorConfig struct {
	Clock         func() time.Time
	ApplyLeadTime time.Duration
}

// NewOrchestrator constructs a mesh-wide rotation orchestrator.
func NewOrchestrator(applier Applier, cfg OrchestratorConfig) *Orchestrator {
	if applier == nil {
		applier = NewMemoryApplier()
	}
	clock := cfg.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	lead := cfg.ApplyLeadTime
	if lead <= 0 {
		lead = DefaultApplyLeadTime
	}
	return &Orchestrator{
		applier: applier,
		history: NewHistory(),
		clock:   clock,
		lead:    lead,
	}
}

// Plan validates and normalizes a mesh-wide rotation request.
func (o *Orchestrator) Plan(req Request) (Plan, error) {
	if o == nil {
		return Plan{}, errors.New("rotation: orchestrator is nil")
	}
	tier, err := NormalizeTier(req.Tier)
	if err != nil {
		return Plan{}, err
	}
	if req.Params == nil {
		return Plan{}, errors.New("rotation: params are required")
	}
	targets := MeshInternalTargets(req.Targets)
	if len(targets) == 0 {
		return Plan{}, ErrNoRotationTargets
	}
	now := o.clock().UTC()
	applyAt := req.ApplyAt
	if applyAt.IsZero() {
		applyAt = now.Add(o.lead)
	}
	applyAt = applyAt.UTC()
	if applyAt.Before(now) {
		return Plan{}, fmt.Errorf("rotation: apply_at %s is in the past", applyAt.Format(time.RFC3339Nano))
	}
	rotationID := strings.TrimSpace(req.RotationID)
	if rotationID == "" {
		rotationID = fmt.Sprintf("%s-%d", tier, now.UnixMicro())
	}
	return Plan{
		Tier:       tier,
		Params:     CloneAWGParamsV2(req.Params),
		ApplyAt:    applyAt,
		RotationID: rotationID,
		Reason:     strings.TrimSpace(req.Reason),
		Targets:    cloneTargets(targets),
	}, nil
}

// Execute applies a validated plan to every mesh-internal target.
func (o *Orchestrator) Execute(ctx context.Context, req Request) (Execution, error) {
	if ctx == nil {
		return Execution{}, errors.New("rotation: context is required")
	}
	plan, err := o.Plan(req)
	if err != nil {
		return Execution{}, err
	}
	results := make([]Result, 0, len(plan.Targets))
	status := RotationStatusSucceeded
	for _, target := range plan.Targets {
		if err := o.applier.ApplyRotation(ctx, target, clonePlan(plan)); err != nil {
			status = RotationStatusPartialFailed
			results = append(results, Result{Target: cloneTarget(target), Ack: false, Error: err.Error()})
			continue
		}
		results = append(results, Result{Target: cloneTarget(target), Ack: true})
	}
	exec := Execution{Plan: clonePlan(plan), Results: cloneResults(results), Status: status}
	o.history.Append(Record{
		Plan:      exec.Plan,
		Results:   exec.Results,
		Status:    status,
		CreatedAt: o.clock().UTC(),
	})
	if status == RotationStatusPartialFailed {
		return exec, fmt.Errorf("%w: %s", ErrPartialApply, plan.RotationID)
	}
	return exec, nil
}

// History returns the orchestrator's append-only history store.
func (o *Orchestrator) History() *History {
	if o == nil {
		return nil
	}
	return o.history
}

// NormalizeTier accepts CLI-friendly numeric aliases and canonical tier names.
func NormalizeTier(tier string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "1", Tier1:
		return Tier1, nil
	case "2", Tier2:
		return Tier2, nil
	case "3", Tier3:
		return Tier3, nil
	default:
		return "", fmt.Errorf("rotation: unsupported tier %q", tier)
	}
}

// MeshInternalTargets filters and sorts targets that participate in AWG mesh-wide rotation.
func MeshInternalTargets(nodes []Target) []Target {
	targets := make([]Target, 0, len(nodes))
	for _, node := range nodes {
		if isMeshInternal(node.Roles) {
			targets = append(targets, cloneTarget(node))
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return targets
}

func isMeshInternal(roles []role.Role) bool {
	for _, r := range roles {
		switch r {
		case role.RoleMaster, role.RoleEgress, role.RoleIngress, role.RoleBalancer:
			return true
		}
	}
	return false
}

// History stores rotation records with copy-on-write semantics.
type History struct {
	mu      sync.RWMutex
	records []Record
}

// NewHistory constructs an empty rotation history.
func NewHistory() *History {
	return &History{}
}

// Append stores one immutable copy of a rotation record.
func (h *History) Append(record Record) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, cloneRecord(record))
}

// Records returns immutable copies of stored records.
func (h *History) Records() []Record {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Record, 0, len(h.records))
	for _, record := range h.records {
		out = append(out, cloneRecord(record))
	}
	return out
}

// MemoryApplier records applied plans in memory. It is the control-plane's
// local state backend until node-side streaming apply lands in the release-gate CRs.
type MemoryApplier struct {
	mu       sync.Mutex
	applied  map[string]Plan
	failures map[string]error
}

// NewMemoryApplier constructs an in-memory applier.
func NewMemoryApplier() *MemoryApplier {
	return &MemoryApplier{
		applied:  make(map[string]Plan),
		failures: make(map[string]error),
	}
}

// SetFailure configures a deterministic target failure for tests and simulations.
func (m *MemoryApplier) SetFailure(nodeName string, err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := strings.TrimSpace(nodeName)
	if err == nil {
		delete(m.failures, key)
		return
	}
	m.failures[key] = err
}

// ApplyRotation records a target's latest applied plan.
func (m *MemoryApplier) ApplyRotation(ctx context.Context, target Target, plan Plan) error {
	if ctx == nil {
		return errors.New("rotation: context is required")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if m == nil {
		return errors.New("rotation: memory applier is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.failures[target.Name]; ok {
		return err
	}
	m.applied[target.Name] = clonePlan(plan)
	return nil
}

// Snapshot returns immutable copies of the latest plan per node.
func (m *MemoryApplier) Snapshot() map[string]Plan {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Plan, len(m.applied))
	for node, plan := range m.applied {
		out[node] = clonePlan(plan)
	}
	return out
}

// CloneAWGParamsV2 deep-copies control-plane AWG params.
func CloneAWGParamsV2(in *controlpb.AWGParamsV2) *controlpb.AWGParamsV2 {
	if in == nil {
		return nil
	}
	return &controlpb.AWGParamsV2{
		Jc:   in.GetJc(),
		Jmin: in.GetJmin(),
		Jmax: in.GetJmax(),
		S1:   in.GetS1(),
		S2:   in.GetS2(),
		H1:   in.GetH1(),
		H2:   in.GetH2(),
		H3:   in.GetH3(),
		H4:   in.GetH4(),
		I1:   append([]byte(nil), in.GetI1()...),
		I2:   append([]byte(nil), in.GetI2()...),
		I3:   append([]byte(nil), in.GetI3()...),
		I4:   append([]byte(nil), in.GetI4()...),
		I5:   append([]byte(nil), in.GetI5()...),
	}
}

func clonePlan(in Plan) Plan {
	return Plan{
		Tier:       in.Tier,
		Params:     CloneAWGParamsV2(in.Params),
		ApplyAt:    in.ApplyAt,
		RotationID: in.RotationID,
		Reason:     in.Reason,
		Targets:    cloneTargets(in.Targets),
	}
}

func cloneTargets(in []Target) []Target {
	if in == nil {
		return nil
	}
	out := make([]Target, 0, len(in))
	for _, target := range in {
		out = append(out, cloneTarget(target))
	}
	return out
}

func cloneTarget(in Target) Target {
	return Target{
		Name:      in.Name,
		Roles:     append([]role.Role(nil), in.Roles...),
		OverlayIP: in.OverlayIP,
	}
}

func cloneResults(in []Result) []Result {
	if in == nil {
		return nil
	}
	out := make([]Result, 0, len(in))
	for _, result := range in {
		out = append(out, Result{
			Target: cloneTarget(result.Target),
			Ack:    result.Ack,
			Error:  result.Error,
		})
	}
	return out
}

func cloneRecord(in Record) Record {
	return Record{
		Plan:      clonePlan(in.Plan),
		Results:   cloneResults(in.Results),
		Status:    in.Status,
		CreatedAt: in.CreatedAt,
	}
}
