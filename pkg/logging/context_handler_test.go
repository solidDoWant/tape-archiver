package logging_test

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/logging"
)

func decodeSingleRecord(t *testing.T, fn func()) map[string]any {
	t.Helper()

	lines := nonEmptyLines(captureStderr(t, fn))
	require.Len(t, lines, 1)

	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &record),
		"log record must be valid JSON: %q", lines[0])

	return record
}

// TestSetupTagsRecordsWithRunContext asserts the keys verbatim because a rename
// silently empties the run-detail log panels (#303).
func TestSetupTagsRecordsWithRunContext(t *testing.T) {
	record := decodeSingleRecord(t, func() {
		logging.Setup("info")

		ctx := logging.ContextWithRunTags(t.Context(), "backup-workflow-42", "run-uuid-abc")
		slog.InfoContext(ctx, "phase complete")
	})

	assert.Equal(t, "phase complete", record[slog.MessageKey])
	assert.Equal(t, "backup-workflow-42", record["WorkflowID"])
	assert.Equal(t, "run-uuid-abc", record["RunID"])
}

func TestSetupWithoutRunContextAddsNoTags(t *testing.T) {
	record := decodeSingleRecord(t, func() {
		logging.Setup("info")
		slog.Info("no run context here")
	})

	assert.NotContains(t, record, "WorkflowID")
	assert.NotContains(t, record, "RunID")
}

func TestSetupOmitsEmptyRunTags(t *testing.T) {
	record := decodeSingleRecord(t, func() {
		logging.Setup("info")

		ctx := logging.ContextWithRunTags(t.Context(), "", "run-only")
		slog.WarnContext(ctx, "run id but no workflow id")
	})

	assert.NotContains(t, record, "WorkflowID")
	assert.Equal(t, "run-only", record["RunID"])
}

// TestSetupTagsSurviveDerivedLoggers covers slog.With, which the Temporal SDK
// uses to attach its own tags.
func TestSetupTagsSurviveDerivedLoggers(t *testing.T) {
	record := decodeSingleRecord(t, func() {
		logging.Setup("info")

		ctx := logging.ContextWithRunTags(t.Context(), "wf-1", "run-1")
		slog.With("component", "tar").InfoContext(ctx, "archived")
	})

	assert.Equal(t, "tar", record["component"])
	assert.Equal(t, "wf-1", record["WorkflowID"])
	assert.Equal(t, "run-1", record["RunID"])
}
