package logging_test

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/logging"
)

// captureStderr redirects os.Stderr to a pipe for the duration of fn, then
// returns everything written to it. It is not safe for parallel use: it swaps
// the process-global os.Stderr, which logging.Setup writes to.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	original := os.Stderr
	read, write, err := os.Pipe()
	require.NoError(t, err)

	os.Stderr = write
	defer func() { os.Stderr = original }()

	fn()

	require.NoError(t, write.Close())

	var builder strings.Builder

	buf := make([]byte, 4096)
	for {
		n, readErr := read.Read(buf)
		builder.Write(buf[:n])

		if readErr != nil {
			break
		}
	}

	require.NoError(t, read.Close())

	return builder.String()
}

// nonEmptyLines splits captured output into the individual JSON log records,
// dropping any trailing blank line.
func nonEmptyLines(output string) []string {
	var lines []string

	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}

	return lines
}

func TestSetupEmitsStructuredJSONToStderr(t *testing.T) {
	output := captureStderr(t, func() {
		logging.Setup("info")
		slog.Info("hello", "answer", 42)
	})

	lines := nonEmptyLines(output)
	require.Len(t, lines, 1)

	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &record),
		"log record must be valid JSON: %q", lines[0])

	assert.Equal(t, "INFO", record[slog.LevelKey])
	assert.Equal(t, "hello", record[slog.MessageKey])
	assert.Contains(t, record, slog.TimeKey)
	assert.NotEmpty(t, record[slog.TimeKey])
	assert.EqualValues(t, 42, record["answer"])
}

func TestSetupEmptyLevelDefaultsToInfo(t *testing.T) {
	output := captureStderr(t, func() {
		logging.Setup("")
		slog.Debug("suppressed")
		slog.Info("emitted")
	})

	lines := nonEmptyLines(output)
	require.Len(t, lines, 1, "debug must be suppressed at the default info level")

	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &record))
	assert.Equal(t, "INFO", record[slog.LevelKey])
	assert.Equal(t, "emitted", record[slog.MessageKey])
}

func TestSetupUnrecognizedLevelWarnsAndFallsBackToInfo(t *testing.T) {
	output := captureStderr(t, func() {
		require.NotPanics(t, func() { logging.Setup("bogus") })
		slog.Debug("suppressed")
		slog.Info("emitted")
	})

	lines := nonEmptyLines(output)

	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record),
			"log record must be valid JSON: %q", line)
		records = append(records, record)
	}

	// The fallback warning is emitted by Setup itself, and the subsequent
	// info record proves the effective level is info (debug suppressed).
	require.Len(t, records, 2)
	assert.Equal(t, "WARN", records[0][slog.LevelKey])
	assert.Equal(t, "bogus", records[0]["input"])
	assert.Equal(t, "INFO", records[1][slog.LevelKey])
	assert.Equal(t, "emitted", records[1][slog.MessageKey])
}

func TestSetupLevelFiltering(t *testing.T) {
	tests := []struct {
		name     string
		level    string
		wantPass map[slog.Level]bool // whether a record at this severity is emitted
	}{
		{
			name:     "debug passes everything",
			level:    "debug",
			wantPass: map[slog.Level]bool{slog.LevelDebug: true, slog.LevelInfo: true, slog.LevelWarn: true, slog.LevelError: true},
		},
		{
			name:     "info suppresses debug",
			level:    "INFO",
			wantPass: map[slog.Level]bool{slog.LevelDebug: false, slog.LevelInfo: true, slog.LevelWarn: true, slog.LevelError: true},
		},
		{
			name:     "warn suppresses debug and info",
			level:    "warning",
			wantPass: map[slog.Level]bool{slog.LevelDebug: false, slog.LevelInfo: false, slog.LevelWarn: true, slog.LevelError: true},
		},
		{
			name:     "error suppresses all but error",
			level:    "error",
			wantPass: map[slog.Level]bool{slog.LevelDebug: false, slog.LevelInfo: false, slog.LevelWarn: false, slog.LevelError: true},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := captureStderr(t, func() {
				logging.Setup(test.level)
				slog.Debug("d")
				slog.Info("i")
				slog.Warn("w")
				slog.Error("e")
			})

			emitted := map[slog.Level]bool{}

			for _, line := range nonEmptyLines(output) {
				var record map[string]any
				require.NoError(t, json.Unmarshal([]byte(line), &record))

				switch record[slog.LevelKey] {
				case "DEBUG":
					emitted[slog.LevelDebug] = true
				case "INFO":
					emitted[slog.LevelInfo] = true
				case "WARN":
					emitted[slog.LevelWarn] = true
				case "ERROR":
					emitted[slog.LevelError] = true
				}
			}

			for level, want := range test.wantPass {
				assert.Equalf(t, want, emitted[level], "severity %s emitted?", level)
			}
		})
	}
}
