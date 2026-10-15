package temporalclient

import (
	"log/slog"

	temporallog "go.temporal.io/sdk/log"
)

// newLogger returns a Temporal SDK logger that forwards to slog.Default().
func newLogger() temporallog.Logger {
	return temporallog.NewStructuredLogger(slog.Default())
}
