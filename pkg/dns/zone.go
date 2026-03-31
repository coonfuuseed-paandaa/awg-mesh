package dns

import (
	"fmt"
	"net"
	"strings"
)

// Record represents a DNS A or PTR record for the overlay zone.
type Record struct {
	Name  string // FQDN (e.g., "kz-01.mesh.zone.")
	Type  string // "A" or "PTR"
	Value string // IP for A, FQDN for PTR
}

// BuildZoneRecords generates A and PTR records from a list of nodes.
// Each node entry is a name->overlayIP pair.
func BuildZoneRecords(zone string, nodes map[string]string) []Record {
	trimmedZone := strings.TrimSpace(zone)
	if trimmedZone == "" {
		return nil
	}
	if !strings.HasSuffix(trimmedZone, ".") {
		trimmedZone += "."
	}

	records := make([]Record, 0, len(nodes)*2)
	for name, ip := range nodes {
		trimmedName := strings.TrimSpace(name)
		trimmedIP := strings.TrimSpace(ip)
		if trimmedName == "" || trimmedIP == "" {
			continue
		}

		parsedIP := net.ParseIP(trimmedIP)
		if parsedIP == nil || parsedIP.To4() == nil {
			continue // skip invalid and IPv6 addresses (A/PTR only support IPv4)
		}

		fqdn := trimmedName + "." + trimmedZone

		// A record
		records = append(records, Record{
			Name:  fqdn,
			Type:  "A",
			Value: parsedIP.String(),
		})

		// PTR record
		ptrName := reverseIP(parsedIP) + ".in-addr.arpa."
		records = append(records, Record{
			Name:  ptrName,
			Type:  "PTR",
			Value: fqdn,
		})
	}

	return records
}

// reverseIP returns the reversed octets of an IPv4 address for PTR records.
func reverseIP(ip net.IP) string {
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", ip4[3], ip4[2], ip4[1], ip4[0])
}
