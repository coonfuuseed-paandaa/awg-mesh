// Package upgrade provides the guided rolling upgrade pipeline for awg-mesh clusters.
// It drives mesh-ctl upgrade <version> (F1) and supports mesh-ctl upgrade-compose (F6).
package upgrade

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
)

// UpgradeStepStatus represents the execution state of one node upgrade step.
type UpgradeStepStatus string

const (
	StatusPlanned    UpgradeStepStatus = "planned"
	StatusRunning    UpgradeStepStatus = "running"
	StatusDone       UpgradeStepStatus = "done"
	StatusFailed     UpgradeStepStatus = "failed"
	StatusRolledBack UpgradeStepStatus = "rolled_back"
	StatusSkipped    UpgradeStepStatus = "skipped"
)

// NodeUpgradeStep describes one node's participation in the upgrade.
type NodeUpgradeStep struct {
	Name     string
	Role     string // "master" or "endpoint"
	Region   string // endpoint region; empty for masters
	OldImage string // "UNKNOWN" — GetStatus has no version field in current proto
	NewImage string // computed target image
	Status   UpgradeStepStatus
}

// UpgradePlan is the immutable ordered list of node upgrade steps computed
// from the topology and the target version. It is a value object — create
// a new one rather than mutating an existing plan.
type UpgradePlan struct {
	Version   string
	Nodes     []NodeUpgradeStep
	Timestamp time.Time
}

// PlanOptions configures plan and order computation.
type PlanOptions struct {
	// ManualOrder overrides the computed node order when non-empty.
	// Every name must exist in the topology.
	ManualOrder []string
	// NodeDowntimeBudget is the maximum allowed downtime per node (default 60 s).
	NodeDowntimeBudget time.Duration
}

const (
	// defaultFallbackImage is used when the topology does not declare a defaults.image.node.
	defaultFallbackImage = "ghcr.io/coonfuuseed-paandaa/awg-mesh-node"
)

// ComputeOrder returns the names of all topology nodes in upgrade order.
//
// Default order: endpoints first (grouped by region, sorted alphabetically
// within each group), then masters (sorted alphabetically by name).
//
// When override is non-empty, it is used verbatim. Every name in override
// must exist in the topology, and duplicates are rejected — both validations
// run before any changes are made.
func ComputeOrder(topo *topology.Topology, override []string) ([]string, error) {
	if topo == nil {
		return nil, fmt.Errorf("topology is required")
	}

	// Build a set of all known node names for O(1) lookup.
	known := make(map[string]bool, len(topo.Masters)+len(topo.Endpoints))
	for _, m := range topo.Masters {
		known[m.Name] = true
	}
	for _, e := range topo.Endpoints {
		known[e.Name] = true
	}

	if len(override) > 0 {
		return validateAndReturnOverride(override, known)
	}

	return defaultOrder(topo), nil
}

// validateAndReturnOverride checks that override contains no duplicates and
// only known node names, then returns override unchanged. This is a pure
// validation function — it never modifies override.
func validateAndReturnOverride(override []string, known map[string]bool) ([]string, error) {
	seen := make(map[string]bool, len(override))
	for _, name := range override {
		if !known[name] {
			return nil, fmt.Errorf("node %q in --order is not present in topology", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("node %q appears more than once in --order", name)
		}
		seen[name] = true
	}
	// Return a new slice to preserve immutability of the input.
	result := make([]string, len(override))
	copy(result, override)
	return result, nil
}

// defaultOrder produces endpoints-first (by region then name), masters last (by name).
func defaultOrder(topo *topology.Topology) []string {
	// Group endpoints by region.
	type epEntry struct {
		name   string
		region string
	}
	eps := make([]epEntry, 0, len(topo.Endpoints))
	for _, e := range topo.Endpoints {
		eps = append(eps, epEntry{name: e.Name, region: e.Region})
	}
	// Sort: primary key = region (alphabetical), secondary key = name (alphabetical).
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].region != eps[j].region {
			return eps[i].region < eps[j].region
		}
		return eps[i].name < eps[j].name
	})

	// Sort masters alphabetically.
	masters := make([]string, 0, len(topo.Masters))
	for _, m := range topo.Masters {
		masters = append(masters, m.Name)
	}
	sort.Strings(masters)

	result := make([]string, 0, len(eps)+len(masters))
	for _, e := range eps {
		result = append(result, e.name)
	}
	result = append(result, masters...)
	return result
}

// ComputePlan builds an UpgradePlan for the given topology and target version.
//
// The old-version column always shows "UNKNOWN" because the current proto's
// GetStatus response contains no version field. This is informational only
// and does not affect upgrade correctness.
//
// Nodes whose current image already matches the target image are marked
// StatusSkipped. Because OldImage is always UNKNOWN this heuristic is
// disabled for v1.10.2 — all nodes are treated as needing an upgrade.
func ComputePlan(topo *topology.Topology, version string, opts PlanOptions) (*UpgradePlan, error) {
	if topo == nil {
		return nil, fmt.Errorf("topology is required")
	}
	if strings.TrimSpace(version) == "" {
		return nil, fmt.Errorf("version is required")
	}

	order, err := ComputeOrder(topo, opts.ManualOrder)
	if err != nil {
		return nil, fmt.Errorf("compute upgrade order: %w", err)
	}

	// Build lookup maps for masters and endpoints.
	masterByName := make(map[string]*topology.MasterNode, len(topo.Masters))
	for i := range topo.Masters {
		masterByName[topo.Masters[i].Name] = &topo.Masters[i]
	}
	endpointByName := make(map[string]*topology.EndpointNode, len(topo.Endpoints))
	for i := range topo.Endpoints {
		endpointByName[topo.Endpoints[i].Name] = &topo.Endpoints[i]
	}

	// Resolve base image registry from topology defaults.
	baseImage := topo.Defaults.Image.Node
	if baseImage == "" {
		baseImage = defaultFallbackImage
	}
	// Strip any existing tag to use only the registry+name portion.
	baseImage = stripImageTag(baseImage)
	newImage := baseImage + ":" + version

	nodes := make([]NodeUpgradeStep, 0, len(order))
	for _, name := range order {
		step := NodeUpgradeStep{
			Name:     name,
			OldImage: "UNKNOWN",
			NewImage: newImage,
			Status:   StatusPlanned,
		}
		if m, ok := masterByName[name]; ok {
			step.Role = "master"
			step.Region = ""
			_ = m // suppress unused-variable warning; used for role detection
		} else if e, ok := endpointByName[name]; ok {
			step.Role = "endpoint"
			step.Region = e.Region
		}
		nodes = append(nodes, step)
	}

	return &UpgradePlan{
		Version:   version,
		Nodes:     nodes,
		Timestamp: time.Now(),
	}, nil
}

// stripImageTag removes any ":tag" or "@digest" suffix from an image reference,
// returning only the registry + name portion. If the reference contains no tag,
// it is returned unchanged.
func stripImageTag(ref string) string {
	// Handle digest reference first (@sha256:...).
	if idx := strings.Index(ref, "@"); idx >= 0 {
		return ref[:idx]
	}
	// Handle tag. A colon after the last slash is a tag separator.
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[:lastColon]
	}
	return ref
}
