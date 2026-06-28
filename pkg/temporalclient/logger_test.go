package temporalclient

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withSlogDefault swaps slog.Default for the duration of the test and routes
// its output to the returned buffer so tests can inspect what the SDK logger
// actually emitted at the configured level.
func withSlogDefault(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()

	buf := &bytes.Buffer{}
	prev := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return buf
}

func TestLoggerEmitsAllLevelsAtDebug(t *testing.T) {
	buf := withSlogDefault(t, slog.LevelDebug)

	logger := newLogger()
	logger.Debug("temporal-debug-message", "k", "v")
	logger.Info("temporal-info-message")
	logger.Warn("temporal-warn-message")
	logger.Error("temporal-error-message")

	out := buf.String()
	assert.Contains(t, out, "temporal-debug-message")
	assert.Contains(t, out, "k=v")
	assert.Contains(t, out, "temporal-info-message")
	assert.Contains(t, out, "temporal-warn-message")
	assert.Contains(t, out, "temporal-error-message")
}

func TestLoggerSuppressesDebugAndInfoAtWarn(t *testing.T) {
	buf := withSlogDefault(t, slog.LevelWarn)

	logger := newLogger()
	logger.Debug("temporal-debug-message")
	logger.Info("temporal-info-message")

	require.Empty(t, buf.String(), "Debug/Info should not produce output when slog level is warn")

	logger.Warn("temporal-warn-message")
	logger.Error("temporal-error-message")

	out := buf.String()
	assert.Contains(t, out, "temporal-warn-message")
	assert.Contains(t, out, "temporal-error-message")
}
