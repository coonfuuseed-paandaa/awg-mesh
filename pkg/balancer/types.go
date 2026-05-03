package balancer

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
	DefaultHealthProbeInterval = 5 * time.Second
	DefaultFlowIdleTimeout     = 30 * time.Second
	maxFWMarkValue             = int64(1<<32 - 1)
)

type Mode string

const (
	ModeDumb    Mode = "dumb"
	ModeLabeled Mode = "labeled"
)

type LabelType string

const (
	LabelDSCP   LabelType = "dscp"
	LabelFWMark LabelType = "fwmark"
)

type EgressTarget struct {
	ID     string
	Target string
	Weight int
}

type LabelMapping struct {
	Type     LabelType
	Value    int
	EgressID string
}

type Config struct {
	Name                string
	OverlayIP           string
	Mode                Mode
	Egresses            []EgressTarget
	Labels              []LabelMapping
	HealthProbeInterval time.Duration
	FlowIdleTimeout     time.Duration
	MetricsAddress      string
}

type Plan struct {
	Name                string
	OverlayIP           string
	Mode                Mode
	EgressCount         int
	Egresses            []EgressTarget
	LabelCount          int
	Labels              []LabelMapping
	HealthProbeInterval time.Duration
	FlowIdleTimeout     time.Duration
	MetricsAddress      string
}

func NormalizeConfig(cfg Config) (Config, error) {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		return Config{}, errors.New("balancer name is required")
	}
	overlay := strings.TrimSpace(cfg.OverlayIP)
	if overlay == "" {
		return Config{}, errors.New("balancer overlay IP is required")
	}
	if _, err := netip.ParseAddr(overlay); err != nil {
		return Config{}, fmt.Errorf("parse balancer overlay IP %q: %w", overlay, err)
	}
	mode := cfg.Mode
	if mode == "" {
		mode = ModeDumb
	}
	if err := validateMode(mode); err != nil {
		return Config{}, err
	}
	egresses, byID, err := normalizeEgresses(cfg.Egresses)
	if err != nil {
		return Config{}, err
	}
	labels, err := normalizeLabels(cfg.Labels, byID)
	if err != nil {
		return Config{}, err
	}
	healthInterval := cfg.HealthProbeInterval
	if healthInterval <= 0 {
		healthInterval = DefaultHealthProbeInterval
	}
	flowIdle := cfg.FlowIdleTimeout
	if flowIdle <= 0 {
		flowIdle = DefaultFlowIdleTimeout
	}
	return Config{
		Name:                name,
		OverlayIP:           overlay,
		Mode:                mode,
		Egresses:            egresses,
		Labels:              labels,
		HealthProbeInterval: healthInterval,
		FlowIdleTimeout:     flowIdle,
		MetricsAddress:      strings.TrimSpace(cfg.MetricsAddress),
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
		Mode:                normalized.Mode,
		EgressCount:         len(normalized.Egresses),
		Egresses:            append([]EgressTarget(nil), normalized.Egresses...),
		LabelCount:          len(normalized.Labels),
		Labels:              append([]LabelMapping(nil), normalized.Labels...),
		HealthProbeInterval: normalized.HealthProbeInterval,
		FlowIdleTimeout:     normalized.FlowIdleTimeout,
		MetricsAddress:      normalized.MetricsAddress,
	}, nil
}

func normalizeEgresses(egresses []EgressTarget) ([]EgressTarget, map[string]EgressTarget, error) {
	if len(egresses) == 0 {
		return nil, nil, errors.New("balancer requires at least one egress target")
	}
	normalized := make([]EgressTarget, 0, len(egresses))
	byID := make(map[string]EgressTarget, len(egresses))
	for i, egress := range egresses {
		next, err := NormalizeEgressTarget(egress)
		if err != nil {
			return nil, nil, fmt.Errorf("egress target %d: %w", i, err)
		}
		if _, exists := byID[next.ID]; exists {
			return nil, nil, fmt.Errorf("egress ID %q is duplicated", next.ID)
		}
		normalized = append(normalized, next)
		byID[next.ID] = next
	}
	return normalized, byID, nil
}

func NormalizeEgressTarget(egress EgressTarget) (EgressTarget, error) {
	id := strings.TrimSpace(egress.ID)
	if id == "" {
		return EgressTarget{}, errors.New("egress ID is required")
	}
	if strings.ContainsAny(id, " \t\r\n=,") {
		return EgressTarget{}, fmt.Errorf("egress ID %q contains invalid separator", id)
	}
	target := strings.TrimSpace(egress.Target)
	if err := validateTarget(target); err != nil {
		return EgressTarget{}, err
	}
	weight := egress.Weight
	if weight <= 0 {
		return EgressTarget{}, fmt.Errorf("egress %q weight must be positive", id)
	}
	return EgressTarget{ID: id, Target: target, Weight: weight}, nil
}

func normalizeLabels(labels []LabelMapping, egresses map[string]EgressTarget) ([]LabelMapping, error) {
	normalized := make([]LabelMapping, 0, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for i, label := range labels {
		next, err := NormalizeLabelMapping(label)
		if err != nil {
			return nil, fmt.Errorf("label mapping %d: %w", i, err)
		}
		if _, ok := egresses[next.EgressID]; !ok {
			return nil, fmt.Errorf("label mapping %s=%d references unknown egress %q", next.Type, next.Value, next.EgressID)
		}
		key := labelKey(next.Type, next.Value)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("label mapping %s=%d is duplicated", next.Type, next.Value)
		}
		seen[key] = struct{}{}
		normalized = append(normalized, next)
	}
	return normalized, nil
}

func NormalizeLabelMapping(label LabelMapping) (LabelMapping, error) {
	labelType := label.Type
	if err := validateLabelType(labelType); err != nil {
		return LabelMapping{}, err
	}
	if label.Value <= 0 {
		return LabelMapping{}, fmt.Errorf("%s value must be positive", labelType)
	}
	switch labelType {
	case LabelDSCP:
		if label.Value > 63 {
			return LabelMapping{}, fmt.Errorf("dscp value %d must be between 1 and 63", label.Value)
		}
	case LabelFWMark:
		if int64(label.Value) > maxFWMarkValue {
			return LabelMapping{}, fmt.Errorf("fwmark value %d must fit uint32", label.Value)
		}
	}
	egressID := strings.TrimSpace(label.EgressID)
	if egressID == "" {
		return LabelMapping{}, fmt.Errorf("%s mapping requires an egress ID", labelType)
	}
	return LabelMapping{Type: labelType, Value: label.Value, EgressID: egressID}, nil
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

func validateMode(mode Mode) error {
	switch mode {
	case ModeDumb, ModeLabeled:
		return nil
	default:
		return fmt.Errorf("unsupported balancer mode %q", mode)
	}
}

func validateLabelType(labelType LabelType) error {
	switch labelType {
	case LabelDSCP, LabelFWMark:
		return nil
	default:
		return fmt.Errorf("unsupported balancer label type %q", labelType)
	}
}

func labelKey(labelType LabelType, value int) string {
	return fmt.Sprintf("%s:%d", labelType, value)
}
