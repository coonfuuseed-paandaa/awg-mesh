//go:build linux

package routing

import (
	"fmt"
	"os/exec"
	"strings"
)

// RouteEntry represents a single entry from the kernel routing table.
type RouteEntry struct {
	Destination string
	Via         string
	Device      string
}

// AddRoute adds a unicast route: ip route add dest via via dev dev.
func AddRoute(dest string, via string, dev string) error {
	if dest != "default" {
		if err := validateCIDR(dest); err != nil {
			return err
		}
	}
	if err := validateIP(via); err != nil {
		return err
	}
	if err := validateInterface(dev); err != nil {
		return err
	}
	out, err := exec.Command("ip", "route", "add", dest, "via", via, "dev", dev).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip route add %s via %s dev %s: %w: %s", dest, via, dev, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DeleteRoute removes a route by destination prefix.
func DeleteRoute(dest string) error {
	if dest != "default" {
		if err := validateCIDR(dest); err != nil {
			return err
		}
	}
	out, err := exec.Command("ip", "route", "del", dest).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip route del %s: %w: %s", dest, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ReplaceRoute atomically replaces (or adds) a route for dest.
func ReplaceRoute(dest string, via string, dev string) error {
	if dest != "default" {
		if err := validateCIDR(dest); err != nil {
			return err
		}
	}
	if err := validateIP(via); err != nil {
		return err
	}
	if err := validateInterface(dev); err != nil {
		return err
	}
	out, err := exec.Command("ip", "route", "replace", dest, "via", via, "dev", dev).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip route replace %s via %s dev %s: %w: %s", dest, via, dev, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ListRoutes parses the output of "ip route show" and returns all routing entries.
func ListRoutes() ([]RouteEntry, error) {
	out, err := exec.Command("ip", "route", "show").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ip route show: %w: %s", err, strings.TrimSpace(string(out)))
	}

	var entries []RouteEntry
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		entry := parseRouteLine(line)
		entries = append(entries, entry)
	}
	return entries, nil
}

// parseRouteLine extracts Destination, Via, and Device from a single "ip route show" line.
// Format example: "10.0.0.0/24 via 192.168.1.1 dev eth0 proto static metric 100"
func parseRouteLine(line string) RouteEntry {
	tokens := strings.Fields(line)
	var entry RouteEntry
	if len(tokens) == 0 {
		return entry
	}
	entry.Destination = tokens[0]
	for i := 1; i < len(tokens)-1; i++ {
		switch tokens[i] {
		case "via":
			entry.Via = tokens[i+1]
		case "dev":
			entry.Device = tokens[i+1]
		}
	}
	return entry
}
