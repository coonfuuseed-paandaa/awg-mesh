package topology

import (
	"errors"
	"fmt"
	"net/netip"
)

// ErrInvalidDSCP is returned when a DSCP value falls outside the allowed range 1..63.
var ErrInvalidDSCP = errors.New("dscp out of range 1..63")

// ValidateDSCP checks that dscp is in the inclusive range [1, 63].
// Returns a sentinel-matchable error wrapping ErrInvalidDSCP when out of range.
func ValidateDSCP(dscp int) error {
	if dscp < 1 || dscp > 63 {
		return fmt.Errorf("%w: got %d", ErrInvalidDSCP, dscp)
	}
	return nil
}

// ValidationError describes a topology validation finding.
type ValidationError struct {
	Field    string
	Message  string
	Severity string // "error" or "warning"
}

// ValidateTopology validates topology consistency and references.
func ValidateTopology(t *Topology) []ValidationError {
	errors := make([]ValidationError, 0)
	addError := func(field string, message string, severity string) {
		errors = append(errors, ValidationError{
			Field:    field,
			Message:  message,
			Severity: severity,
		})
	}

	overlayPrefix, overlayErr := netip.ParsePrefix(t.Overlay.Space)
	if overlayErr != nil {
		addError("overlay.space", fmt.Sprintf("invalid overlay CIDR %q: %v", t.Overlay.Space, overlayErr), "error")
	} else {
		overlayPrefix = overlayPrefix.Masked()
	}

	rangeNames := make(map[string]struct{}, len(t.Overlay.Ranges))
	validRanges := make([]Range, 0, len(t.Overlay.Ranges))

	for i, namedRange := range t.Overlay.Ranges {
		fieldBase := fmt.Sprintf("overlay.ranges[%d]", i)

		if _, exists := rangeNames[namedRange.Name]; exists {
			addError(fieldBase+".name", fmt.Sprintf("duplicate range name %q", namedRange.Name), "error")
		} else {
			rangeNames[namedRange.Name] = struct{}{}
		}

		parsedRange, err := ParseRange(namedRange)
		if err != nil {
			addError(fieldBase+".cidr", err.Error(), "error")
			continue
		}

		if overlayErr == nil && !isRangeWithinOverlay(overlayPrefix, parsedRange.Network) {
			addError(fieldBase+".cidr", "range is not contained in overlay space", "error")
		}

		if namedRange.BalancerIP != "" && !parsedRange.Contains(parsedRange.BalancerIP) {
			addError(fieldBase+".balancer_ip", "balancer_ip must be inside the range", "error")
		}

		validRanges = append(validRanges, parsedRange)
	}

	for i := 0; i < len(validRanges); i++ {
		for j := i + 1; j < len(validRanges); j++ {
			if RangesOverlap(validRanges[i], validRanges[j]) {
				addError(
					"overlay.ranges",
					fmt.Sprintf("ranges %q and %q overlap", validRanges[i].Name, validRanges[j].Name),
					"error",
				)
			}
		}
	}

	validateUniqueNames(t.Masters, "masters", addError, func(m MasterNode) string { return m.Name })
	validateUniqueNames(t.Endpoints, "endpoints", addError, func(e EndpointNode) string { return e.Name })
	validateUniqueNames(t.Clients, "clients", addError, func(c ClientNode) string { return c.Name })

	// Warn when a master name exceeds 12 characters: Linux iface names are limited to 15
	// characters total, and the "wg-" prefix leaves only 12 characters for the master name.
	const maxMasterNameLen = 12
	for i, master := range t.Masters {
		if len(master.Name) > maxMasterNameLen {
			addError(
				fmt.Sprintf("masters[%d].name", i),
				fmt.Sprintf("master name %q has %d characters; names longer than %d may be truncated in WireGuard interface names (Linux limit: 15, prefix \"wg-\" uses 3)", master.Name, len(master.Name), maxMasterNameLen),
				"warning",
			)
		}
	}

	validateUniqueOverlayIPs(t, addError)
	validateReferences(t, addError)
	validateClientDSCPPolicies(t, addError)

	return errors
}

func validateUniqueNames[T any](
	items []T,
	fieldPrefix string,
	addError func(field string, message string, severity string),
	nameOf func(T) string,
) {
	seen := make(map[string]struct{}, len(items))
	for i, item := range items {
		name := nameOf(item)
		if _, exists := seen[name]; exists {
			addError(fmt.Sprintf("%s[%d].name", fieldPrefix, i), fmt.Sprintf("duplicate name %q", name), "error")
			continue
		}
		seen[name] = struct{}{}
	}
}

func validateUniqueOverlayIPs(t *Topology, addError func(field string, message string, severity string)) {
	owners := make(map[netip.Addr]string, len(t.Masters)+len(t.Endpoints)+len(t.Clients))
	recordIP := func(field string, value string) {
		addr, err := netip.ParseAddr(value)
		if err != nil {
			addError(field, fmt.Sprintf("invalid overlay IP %q: %v", value, err), "error")
			return
		}
		addr = normalizeAddr(addr)
		if owner, exists := owners[addr]; exists {
			addError(field, fmt.Sprintf("overlay IP %s already used by %s", value, owner), "error")
			return
		}
		owners[addr] = field
	}

	for i, master := range t.Masters {
		recordIP(fmt.Sprintf("masters[%d].overlay_ip", i), master.OverlayIP)
	}
	for i, endpoint := range t.Endpoints {
		recordIP(fmt.Sprintf("endpoints[%d].overlay_ip", i), endpoint.OverlayIP)
	}
	for i, client := range t.Clients {
		recordIP(fmt.Sprintf("clients[%d].overlay_ip", i), client.OverlayIP)
	}
}

func validateReferences(t *Topology, addError func(field string, message string, severity string)) {
	endpoints := make(map[string]struct{}, len(t.Endpoints))
	for _, endpoint := range t.Endpoints {
		endpoints[endpoint.Name] = struct{}{}
	}

	for i, master := range t.Masters {
		for j, endpointRef := range master.Endpoints {
			if _, exists := endpoints[endpointRef]; exists {
				continue
			}
			addError(
				fmt.Sprintf("masters[%d].endpoints[%d]", i, j),
				fmt.Sprintf("endpoint %q not found", endpointRef),
				"warning",
			)
		}
	}

	masters := make(map[string]struct{}, len(t.Masters))
	for _, master := range t.Masters {
		masters[master.Name] = struct{}{}
	}

	for i, client := range t.Clients {
		for j, masterRef := range client.Masters {
			if _, exists := masters[masterRef]; exists {
				continue
			}
			addError(
				fmt.Sprintf("clients[%d].masters[%d]", i, j),
				fmt.Sprintf("master %q not found", masterRef),
				"warning",
			)
		}
	}
}

func isRangeWithinOverlay(overlay netip.Prefix, candidate netip.Prefix) bool {
	maskedCandidate := candidate.Masked()
	first := maskedCandidate.Addr()
	last := prefixLastAddr(maskedCandidate)
	return overlay.Contains(first) && overlay.Contains(last)
}

func validateClientDSCPPolicies(t *Topology, addError func(field string, message string, severity string)) {
	for i, client := range t.Clients {
		for j, policy := range client.RoutingPolicies {
			if err := ValidateDSCP(policy.DSCP); err != nil {
				addError(
					fmt.Sprintf("clients[%d].routing_policies[%d].dscp", i, j),
					fmt.Sprintf("client %q policy %q: %v", client.Name, policy.Name, err),
					"error",
				)
			}
		}
	}
}
