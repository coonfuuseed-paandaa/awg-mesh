//go:build linux

package routing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteRtTables_HappyPath(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "awg-mesh.conf")
	rtTablesPath = outPath
	t.Cleanup(func() { rtTablesPath = "/etc/iproute2/rt_tables.d/awg-mesh.conf" })

	policies := []DSCPPolicy{
		{DSCP: 10, Fwmark: 10, TableID: 110},
		{DSCP: 0, Fwmark: 0, TableID: 100},
		{DSCP: 28, Fwmark: 28, TableID: 128},
	}
	if err := writeRtTables(policies); err != nil {
		t.Fatalf("writeRtTables: %v", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "100 awg-dscp-0\n110 awg-dscp-10\n128 awg-dscp-28\n"
	if string(got) != want {
		t.Errorf("content mismatch:\ngot:  %q\nwant: %q", string(got), want)
	}
}

func TestWriteRtTables_Idempotent(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "awg-mesh.conf")
	rtTablesPath = outPath
	t.Cleanup(func() { rtTablesPath = "/etc/iproute2/rt_tables.d/awg-mesh.conf" })

	policies := []DSCPPolicy{
		{DSCP: 10, Fwmark: 10, TableID: 110},
		{DSCP: 28, Fwmark: 28, TableID: 128},
	}

	if err := writeRtTables(policies); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile after first write: %v", err)
	}

	if err := writeRtTables(policies); err != nil {
		t.Fatalf("second write: %v", err)
	}
	second, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile after second write: %v", err)
	}

	if string(first) != string(second) {
		t.Errorf("idempotency violated:\nfirst:  %q\nsecond: %q", string(first), string(second))
	}
}

func TestWriteRtTables_DeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "awg-mesh.conf")
	rtTablesPath = outPath
	t.Cleanup(func() { rtTablesPath = "/etc/iproute2/rt_tables.d/awg-mesh.conf" })

	// Deliberately pass policies in reverse order.
	policies := []DSCPPolicy{
		{DSCP: 28, Fwmark: 28, TableID: 128},
		{DSCP: 10, Fwmark: 10, TableID: 110},
		{DSCP: 0, Fwmark: 0, TableID: 100},
	}
	if err := writeRtTables(policies); err != nil {
		t.Fatalf("writeRtTables: %v", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "100 awg-dscp-0\n110 awg-dscp-10\n128 awg-dscp-28\n"
	if string(got) != want {
		t.Errorf("order not deterministic:\ngot:  %q\nwant: %q", string(got), want)
	}
}

func TestWriteRtTables_ReadOnlyFS(t *testing.T) {
	dir := t.TempDir()
	// Point to a path whose parent directory does not exist. os.WriteFile on
	// the .tmp file will return ENOENT regardless of process UID (including root),
	// which avoids the chmod-as-root bypass that affects DAC permission tests.
	outPath := filepath.Join(dir, "nonexistent-subdir", "awg-mesh.conf")
	rtTablesPath = outPath
	t.Cleanup(func() { rtTablesPath = "/etc/iproute2/rt_tables.d/awg-mesh.conf" })

	policies := []DSCPPolicy{
		{DSCP: 10, Fwmark: 10, TableID: 110},
	}
	if err := writeRtTables(policies); err == nil {
		t.Fatal("expected error when parent dir does not exist, got nil")
	}

	// File must not exist after the failed write (atomic write protects against partial output).
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Errorf("file must not exist after failed atomic write; Stat returned: %v", statErr)
	}
}
