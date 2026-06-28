package temporalclient

import (
	"log/slog"

	temporallog "go.temporal.io/sdk/log"
)

// newLogger returns a Temporal SDK log.Logger that forwards Debug/Info/Warn/
// Error calls to slog.Default(). Level filtering is delegated to the slog
// handler configured by pkg/logging.Setup, so LOG_LEVEL controls SDK log
// verbosity the same way it controls application log verbosity.
func newLogger() temporallog.Logger {
	return temporallog.NewStructuredLogger(slog.Default())
}
