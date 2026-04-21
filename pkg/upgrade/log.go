package upgrade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// UpgradeLogEntry is one structured event in the upgrade audit log.
// It is written as a single JSONL line per entry.
type UpgradeLogEntry struct {
	Version    string    `json:"version"`
	NodeName   string    `json:"node_name"`
	Phase      string    `json:"phase"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason,omitempty"`
	DurationMs int64     `json:"duration_ms"`
	Timestamp  time.Time `json:"timestamp"`
}

// Logger writes UpgradeLogEntry values as JSONL to a file on disk.
// It is safe for concurrent use — all writes are serialized via a mutex.
type Logger struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

// NewLogger opens (or creates) the log file at logPath.
// The directory containing logPath must already exist.
func NewLogger(logPath string) (*Logger, error) {
	if logPath == "" {
		return nil, fmt.Errorf("log path is required")
	}
	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open upgrade log %q: %w", logPath, err)
	}
	return &Logger{path: logPath, f: f}, nil
}

// Path returns the absolute path of the log file.
func (l *Logger) Path() string {
	return l.path
}

// Append writes one UpgradeLogEntry as a JSONL line.
// It is safe to call from multiple goroutines.
func (l *Logger) Append(entry UpgradeLogEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal log entry: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if _, err := l.f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write log entry: %w", err)
	}
	return nil
}

// ReadAll reads all UpgradeLogEntry values from the log file.
// It opens the file for reading independently of the write file descriptor,
// so it is safe to call while the Logger is still active.
func (l *Logger) ReadAll() ([]UpgradeLogEntry, error) {
	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read upgrade log %q: %w", l.path, err)
	}
	if len(data) == 0 {
		return nil, nil
	}

	var entries []UpgradeLogEntry
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var entry UpgradeLogEntry
		if err := dec.Decode(&entry); err != nil {
			return nil, fmt.Errorf("decode log entry: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// Close closes the underlying log file. Subsequent Append calls will fail.
// Close is idempotent — calling it multiple times returns nil.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// LogPath returns the conventional upgrade log path for a given config directory,
// version, and timestamp. The caller uses this to construct the Logger.
//
//	~/.mesh-ctl/upgrade-v1.10.2-20260418T182300Z.log
func LogPath(configDir, version string, ts time.Time) string {
	stamp := ts.UTC().Format("20060102T150405Z")
	name := "upgrade-" + version + "-" + stamp + ".log"
	return filepath.Join(configDir, name)
}

// MostRecentLogPath returns the path of the most recently modified
// upgrade-*.log file in configDir, or "" if none exist.
func MostRecentLogPath(configDir string) (string, error) {
	entries, err := os.ReadDir(configDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read config dir %q: %w", configDir, err)
	}

	var best string
	var bestTime time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < 8 || name[:8] != "upgrade-" || filepath.Ext(name) != ".log" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(bestTime) {
			bestTime = info.ModTime()
			best = filepath.Join(configDir, name)
		}
	}
	return best, nil
}
