package logging

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
)

func TestNewLoggerComponent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		component     string
		wantComponent string
	}{
		{name: "default component", component: "", wantComponent: "app"},
		{name: "trimmed component", component: "  node  ", wantComponent: "node"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buffer bytes.Buffer
			logger := NewLogger(tt.component).Output(&buffer)
			logger.Info().Msg("hello")

			event := decodeLogEvent(t, buffer.Bytes())
			if event["component"] != tt.wantComponent {
				t.Fatalf("unexpected component: got %v want %s", event["component"], tt.wantComponent)
			}
			if _, exists := event["time"]; !exists {
				t.Fatalf("expected timestamp field in log event")
			}
		})
	}
}

func TestSetGlobalLevel(t *testing.T) {
	original := zerolog.GlobalLevel()
	defer zerolog.SetGlobalLevel(original)

	tests := []struct {
		name      string
		input     string
		wantLevel zerolog.Level
	}{
		{name: "debug", input: "debug", wantLevel: zerolog.DebugLevel},
		{name: "info", input: "info", wantLevel: zerolog.InfoLevel},
		{name: "warn", input: "warn", wantLevel: zerolog.WarnLevel},
		{name: "error", input: "error", wantLevel: zerolog.ErrorLevel},
		{name: "unknown defaults to info", input: "something", wantLevel: zerolog.InfoLevel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetGlobalLevel(tt.input)
			if got := zerolog.GlobalLevel(); got != tt.wantLevel {
				t.Fatalf("unexpected level for %q: got %s want %s", tt.input, got, tt.wantLevel)
			}
		})
	}
}

func TestWithFields(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer
	base := NewLogger("metrics").Output(&buffer)

	fields := map[string]string{"b": "2", "a": "1"}
	enriched := WithFields(base, fields)
	enriched.Info().Msg("event")

	event := decodeLogEvent(t, buffer.Bytes())
	if event["component"] != "metrics" {
		t.Fatalf("unexpected component: %v", event["component"])
	}
	if event["a"] != "1" || event["b"] != "2" {
		t.Fatalf("expected custom fields in event, got %#v", event)
	}

	buffer.Reset()
	same := WithFields(base, nil)
	same.Info().Msg("event")
	event = decodeLogEvent(t, buffer.Bytes())
	if _, exists := event["a"]; exists {
		t.Fatalf("did not expect field 'a' when no custom fields were passed")
	}
}

func decodeLogEvent(t *testing.T, raw []byte) map[string]any {
	t.Helper()

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		t.Fatalf("empty log output")
	}

	var event map[string]any
	if err := json.Unmarshal(trimmed, &event); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; raw=%q", err, string(trimmed))
	}
	return event
}
