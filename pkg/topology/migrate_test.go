package topology

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectSchemaVersion_V1Fixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "v1x-topology.yml"))
	if err != nil {
		t.Fatalf("read v1 fixture: %v", err)
	}
	got, err := DetectSchemaVersion(data)
	if err != nil {
		t.Fatalf("DetectSchemaVersion on v1 fixture: %v", err)
	}
	if got != SchemaV1 {
		t.Fatalf("expected SchemaV1 (1), got %d", got)
	}
}

func TestDetectSchemaVersion_V2Fixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "v2-topology.yml"))
	if err != nil {
		t.Fatalf("read v2 fixture: %v", err)
	}
	got, err := DetectSchemaVersion(data)
	if err != nil {
		t.Fatalf("DetectSchemaVersion on v2 fixture: %v", err)
	}
	if got != SchemaV2 {
		t.Fatalf("expected SchemaV2 (2), got %d", got)
	}
}

func TestDetectSchemaVersion_Empty(t *testing.T) {
	if _, err := DetectSchemaVersion(nil); err == nil {
		t.Fatalf("DetectSchemaVersion(nil) must error")
	}
	if _, err := DetectSchemaVersion([]byte{}); err == nil {
		t.Fatalf("DetectSchemaVersion(empty) must error")
	}
}

func TestDetectSchemaVersion_OnlyTransportPool(t *testing.T) {
	// transport: with pool is the v1.x marker even without masters/endpoints keys.
	data := []byte(`transport:
  pool: 10.255.0.0/16
  prefix_length: 30
`)
	got, err := DetectSchemaVersion(data)
	if err != nil {
		t.Fatalf("DetectSchemaVersion: %v", err)
	}
	if got != SchemaV1 {
		t.Fatalf("expected SchemaV1, got %d", got)
	}
}

func TestDetectSchemaVersion_UnsupportedSchema(t *testing.T) {
	data := []byte(`schema_version: 99
`)
	_, err := DetectSchemaVersion(data)
	if err == nil {
		t.Fatalf("schema_version=99 must error")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected 'unsupported' in error, got: %v", err)
	}
}

func TestMigrateV1ToV2_Stub(t *testing.T) {
	// CR-001: stub returns error pointing to CR-013.
	_, err := MigrateV1ToV2([]byte("masters: []"))
	if err == nil {
		t.Fatalf("MigrateV1ToV2 should error in CR-001")
	}
	if !strings.Contains(err.Error(), "CR-013") {
		t.Fatalf("expected CR-013 forward reference, got: %v", err)
	}
}
