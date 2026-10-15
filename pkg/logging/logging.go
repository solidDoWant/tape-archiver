// Package logging configures the global slog JSON handler, which tags records
// with the Temporal run identity carried by the log context (#303).
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Setup installs a JSON stderr handler at level (debug, info, warn, error);
// unrecognized levels fall back to info with a warning.
func Setup(level string) {
	lvl, unrecognized := parseLevel(level)

	handler := contextHandler{Handler: slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})}
	slog.SetDefault(slog.New(handler))

	if unrecognized {
		slog.Warn("unrecognized log level, falling back to info", "input", level)
	}
}

func parseLevel(level string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		return slog.LevelInfo, false
	case "debug":
		return slog.LevelDebug, false
	case "warn", "warning":
		return slog.LevelWarn, false
	case "error":
		return slog.LevelError, false
	default:
		return slog.LevelInfo, true
	}
}
