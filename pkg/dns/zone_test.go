package dns

import (
	"testing"
)

func TestBuildZoneRecords(t *testing.T) {
	nodes := map[string]string{
		"node-asia-01": "172.20.70.34",
		"node-us-01": "172.20.70.38",
	}

	records := BuildZoneRecords("mesh.zone", nodes)

	if len(records) != 4 {
		t.Fatalf("expected 4 records (2 A + 2 PTR), got %d", len(records))
	}

	aRecords := make(map[string]string)
	ptrRecords := make(map[string]string)
	for _, r := range records {
		switch r.Type {
		case "A":
			aRecords[r.Name] = r.Value
		case "PTR":
			ptrRecords[r.Name] = r.Value
		}
	}

	if ip, ok := aRecords["node-asia-01.mesh.zone."]; !ok || ip != "172.20.70.34" {
		t.Errorf("expected A record for node-asia-01.mesh.zone. = 172.20.70.34, got %q", ip)
	}
	if ip, ok := aRecords["node-us-01.mesh.zone."]; !ok || ip != "172.20.70.38" {
		t.Errorf("expected A record for node-us-01.mesh.zone. = 172.20.70.38, got %q", ip)
	}

	if name, ok := ptrRecords["34.70.20.172.in-addr.arpa."]; !ok || name != "node-asia-01.mesh.zone." {
		t.Errorf("expected PTR for 34.70.20.172 = node-asia-01.mesh.zone., got %q", name)
	}
	if name, ok := ptrRecords["38.70.20.172.in-addr.arpa."]; !ok || name != "node-us-01.mesh.zone." {
		t.Errorf("expected PTR for 38.70.20.172 = node-us-01.mesh.zone., got %q", name)
	}
}

func TestBuildZoneRecordsEmptyZone(t *testing.T) {
	records := BuildZoneRecords("", map[string]string{"a": "1.2.3.4"})
	if records != nil {
		t.Errorf("expected nil for empty zone, got %d records", len(records))
	}
}

func TestBuildZoneRecordsInvalidIP(t *testing.T) {
	records := BuildZoneRecords("test.zone", map[string]string{"a": "not-an-ip"})
	if len(records) != 0 {
		t.Errorf("expected 0 records for invalid IP, got %d", len(records))
	}
}

func TestBuildZoneRecordsTrailingDot(t *testing.T) {
	records := BuildZoneRecords("mesh.zone.", map[string]string{"a": "1.2.3.4"})
	found := false
	for _, r := range records {
		if r.Type == "A" && r.Name == "a.mesh.zone." {
			found = true
		}
	}
	if !found {
		t.Error("zone with trailing dot should produce valid A records")
	}
}
