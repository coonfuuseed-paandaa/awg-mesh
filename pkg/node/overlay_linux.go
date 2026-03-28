//go:build linux

package node

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// AssignOverlayIP assigns the overlay IP to loopback with a /32 mask.
func AssignOverlayIP(ip string) error {
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil {
		return fmt.Errorf("overlay IP must be a valid IP address")
	}
	normalizedIP := parsedIP.String()

	showOut, err := exec.Command("ip", "addr", "show", "dev", "lo").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip addr show dev lo: %w: %s", err, strings.TrimSpace(string(showOut)))
	}
	if strings.Contains(string(showOut), normalizedIP+"/32") {
		return nil
	}

	addOut, err := exec.Command("ip", "addr", "add", normalizedIP+"/32", "dev", "lo").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip addr add %s/32 dev lo: %w: %s", normalizedIP, err, strings.TrimSpace(string(addOut)))
	}
	return nil
}

// RemoveOverlayIP removes the overlay IP from loopback.
func RemoveOverlayIP(ip string) error {
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil {
		return fmt.Errorf("overlay IP must be a valid IP address")
	}
	normalizedIP := parsedIP.String()

	delOut, err := exec.Command("ip", "addr", "del", normalizedIP+"/32", "dev", "lo").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip addr del %s/32 dev lo: %w: %s", normalizedIP, err, strings.TrimSpace(string(delOut)))
	}
	return nil
}
