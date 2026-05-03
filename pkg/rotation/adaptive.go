package rotation

import (
	"fmt"
	"sort"
	"time"
)

const (
	DefaultThroughputDropThreshold = 0.30
	DefaultAdaptiveWindow          = 30 * time.Second
	DefaultRetryStormThreshold     = 10
	DefaultRTTSpikeMultiplier      = 2.0
)

// MetricSample is one observed link-quality sample used for adaptive rotation.
type MetricSample struct {
	NodeName               string
	Window                 time.Duration
	BaselineThroughputMbps float64
	CurrentThroughputMbps  float64
	HandshakeRetries       int
	BaselineRTT            time.Duration
	CurrentRTT             time.Duration
}

// AdaptiveDecision describes whether adaptive rotation should fire.
type AdaptiveDecision struct {
	Trigger   bool
	Tier      string
	NodeName  string
	Reason    string
	DropRatio float64
}

// AdaptiveConfig configures adaptive trigger thresholds.
type AdaptiveConfig struct {
	ThroughputDropThreshold float64
	MinWindow               time.Duration
	RetryStormThreshold     int
	RTTSpikeMultiplier      float64
}

// AdaptiveDetector evaluates link metrics and emits tier-1 rotation triggers.
type AdaptiveDetector struct {
	cfg AdaptiveConfig
}

// NewAdaptiveDetector constructs a detector with production defaults.
func NewAdaptiveDetector(cfg AdaptiveConfig) *AdaptiveDetector {
	if cfg.ThroughputDropThreshold <= 0 {
		cfg.ThroughputDropThreshold = DefaultThroughputDropThreshold
	}
	if cfg.MinWindow <= 0 {
		cfg.MinWindow = DefaultAdaptiveWindow
	}
	if cfg.RetryStormThreshold <= 0 {
		cfg.RetryStormThreshold = DefaultRetryStormThreshold
	}
	if cfg.RTTSpikeMultiplier <= 0 {
		cfg.RTTSpikeMultiplier = DefaultRTTSpikeMultiplier
	}
	return &AdaptiveDetector{cfg: cfg}
}

// Evaluate returns the first deterministic tier-1 trigger, if any.
func (d *AdaptiveDetector) Evaluate(samples []MetricSample) AdaptiveDecision {
	if d == nil {
		d = NewAdaptiveDetector(AdaptiveConfig{})
	}
	ordered := append([]MetricSample(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].NodeName < ordered[j].NodeName })
	for _, sample := range ordered {
		if sample.Window < d.cfg.MinWindow {
			continue
		}
		if sample.BaselineThroughputMbps > 0 && sample.CurrentThroughputMbps >= 0 {
			drop := (sample.BaselineThroughputMbps - sample.CurrentThroughputMbps) / sample.BaselineThroughputMbps
			if drop >= d.cfg.ThroughputDropThreshold {
				return AdaptiveDecision{
					Trigger:   true,
					Tier:      Tier1,
					NodeName:  sample.NodeName,
					Reason:    fmt.Sprintf("throughput-drop:%.2f", drop),
					DropRatio: drop,
				}
			}
		}
		if sample.HandshakeRetries >= d.cfg.RetryStormThreshold {
			return AdaptiveDecision{
				Trigger:  true,
				Tier:     Tier1,
				NodeName: sample.NodeName,
				Reason:   fmt.Sprintf("handshake-retry-storm:%d", sample.HandshakeRetries),
			}
		}
		if sample.BaselineRTT > 0 && sample.CurrentRTT >= time.Duration(float64(sample.BaselineRTT)*d.cfg.RTTSpikeMultiplier) {
			return AdaptiveDecision{
				Trigger:  true,
				Tier:     Tier1,
				NodeName: sample.NodeName,
				Reason:   fmt.Sprintf("rtt-spike:%s", sample.CurrentRTT),
			}
		}
	}
	return AdaptiveDecision{}
}
