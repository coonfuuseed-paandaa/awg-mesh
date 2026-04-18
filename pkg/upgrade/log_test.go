package upgrade

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func makeLogger(t *testing.T) (*Logger, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "upgrade.log")
	l, err := NewLogger(path)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, path
}

func entry(node, phase, status string) UpgradeLogEntry {
	return UpgradeLogEntry{
		Version:    "v1.10.2",
		NodeName:   node,
		Phase:      phase,
		Status:     status,
		DurationMs: 42,
		Timestamp:  time.Now().UTC().Truncate(time.Second),
	}
}

// TestLogger_AppendReadAll verifies a round-trip: write N entries, read them back.
func TestLogger_AppendReadAll(t *testing.T) {
	l, _ := makeLogger(t)

	entries := []UpgradeLogEntry{
		entry("node-1", "prepare", "ok"),
		entry("node-1", "deploy", "ok"),
		entry("node-2", "prepare", "failed"),
	}
	for _, e := range entries {
		if err := l.Append(e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	got, err := l.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("count: got %d want %d", len(got), len(entries))
	}
	for i, want := range entries {
		if got[i].NodeName != want.NodeName {
			t.Errorf("[%d] NodeName: got %q want %q", i, got[i].NodeName, want.NodeName)
		}
		if got[i].Phase != want.Phase {
			t.Errorf("[%d] Phase: got %q want %q", i, got[i].Phase, want.Phase)
		}
		if got[i].Status != want.Status {
			t.Errorf("[%d] Status: got %q want %q", i, got[i].Status, want.Status)
		}
	}
}

// TestLogger_EmptyFile verifies ReadAll on a freshly created (empty) log returns nil, nil.
func TestLogger_EmptyFile(t *testing.T) {
	l, _ := makeLogger(t)
	entries, err := l.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll on empty log: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries for empty log, got %v", entries)
	}
}

// TestLogger_CloseIdempotent verifies Close() may be called multiple times without error.
func TestLogger_CloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(filepath.Join(dir, "upgrade.log"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestLogger_ConcurrentAppend verifies that concurrent Append calls do not corrupt the log.
func TestLogger_ConcurrentAppend(t *testing.T) {
	l, _ := makeLogger(t)

	const goroutines = 20
	const perGoroutine = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(id int) {
			defer wg.Done()
			for j := range perGoroutine {
				e := entry("node", "phase", "ok")
				e.DurationMs = int64(id*100 + j)
				if err := l.Append(e); err != nil {
					// Cannot call t.Fatal from goroutine — log and let count check catch it.
					t.Logf("goroutine %d: Append error: %v", id, err)
				}
			}
		}(i)
	}
	wg.Wait()

	got, err := l.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll after concurrent writes: %v", err)
	}
	want := goroutines * perGoroutine
	if len(got) != want {
		t.Errorf("entry count: got %d want %d", len(got), want)
	}
}

// TestLogPath verifies the conventional log path format.
func TestLogPath(t *testing.T) {
	ts := time.Date(2026, 4, 18, 18, 23, 0, 0, time.UTC)
	got := LogPath("/home/user/.mesh-ctl", "v1.10.2", ts)
	want := filepath.Join("/home/user/.mesh-ctl", "upgrade-v1.10.2-20260418T182300Z.log")
	if got != want {
		t.Errorf("LogPath: got %q want %q", got, want)
	}
}

// TestMostRecentLogPath verifies the most-recently-modified log is returned.
func TestMostRecentLogPath(t *testing.T) {
	dir := t.TempDir()

	// Write two log files with distinct modification times.
	older := filepath.Join(dir, "upgrade-v1.10.0-20260101T000000Z.log")
	newer := filepath.Join(dir, "upgrade-v1.10.2-20260418T182300Z.log")

	if err := os.WriteFile(older, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	// Explicitly set distinct mtimes to avoid filesystem mtime-granularity races.
	oldTime := time.Now().Add(-time.Hour)
	newTime := time.Now()
	if err := os.Chtimes(older, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, newTime, newTime); err != nil {
		t.Fatal(err)
	}

	got, err := MostRecentLogPath(dir)
	if err != nil {
		t.Fatalf("MostRecentLogPath: %v", err)
	}
	if got != newer {
		t.Errorf("got %q want %q", got, newer)
	}
}

// TestMostRecentLogPath_Empty verifies that an empty directory returns "".
func TestMostRecentLogPath_Empty(t *testing.T) {
	dir := t.TempDir()
	got, err := MostRecentLogPath(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// TestMostRecentLogPath_NonexistentDir verifies that a missing directory returns "".
func TestMostRecentLogPath_NonexistentDir(t *testing.T) {
	got, err := MostRecentLogPath("/nonexistent/path/that/does/not/exist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string for nonexistent dir, got %q", got)
	}
}

// TestNewLogger_EmptyPath verifies NewLogger rejects an empty path.
func TestNewLogger_EmptyPath(t *testing.T) {
	_, err := NewLogger("")
	if err == nil {
		t.Fatal("expected error for empty log path, got nil")
	}
}
