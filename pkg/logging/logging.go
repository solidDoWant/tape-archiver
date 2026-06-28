// Package logging configures the global slog handler for the project. Setup
// installs a structured JSON handler that writes to stderr; every other package
// logs through the standard library slog default.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Setup configures the global slog default handler to emit structured JSON to
// stderr at the given level. Recognized levels (case-insensitive) are debug,
// info, warn (or warning), and error. An empty string defaults to info; any
// other string falls back to info and logs a warning. Setup never panics or
// returns an error.
func Setup(level string) {
	lvl, unrecognized := parseLevel(level)

	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(handler))

	if unrecognized {
		slog.Warn("unrecognized log level, falling back to info", "input", level)
	}
}

// parseLevel maps a level string to a slog.Level. The second return value is
// true when the input was not a recognized level (and info is used as the
// fallback).
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
