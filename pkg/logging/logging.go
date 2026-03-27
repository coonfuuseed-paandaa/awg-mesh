package logging

import (
	"os"
	"sort"
	"strings"

	"github.com/rs/zerolog"
)

const defaultComponent = "app"

// NewLogger creates a structured JSON logger with timestamp and component field.
func NewLogger(component string) zerolog.Logger {
	componentName := strings.TrimSpace(component)
	if componentName == "" {
		componentName = defaultComponent
	}

	return zerolog.New(os.Stderr).
		With().
		Timestamp().
		Str("component", componentName).
		Logger()
}

// SetGlobalLevel configures zerolog global level from string value.
func SetGlobalLevel(level string) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case "info":
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case "warn":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "error":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}
}

// WithFields returns a logger enriched with the given string fields.
func WithFields(logger zerolog.Logger, fields map[string]string) zerolog.Logger {
	if len(fields) == 0 {
		return logger
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	builder := logger.With()
	for _, key := range keys {
		builder = builder.Str(key, fields[key])
	}

	return builder.Logger()
}
