package logging_test

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/logging"
)

// decodeSingleRecord captures fn's structured output, asserts it produced exactly
// one JSON log line, and returns it decoded.
func decodeSingleRecord(t *testing.T, fn func()) map[string]any {
	t.Helper()

	lines := nonEmptyLines(captureStderr(t, fn))
	require.Len(t, lines, 1)

	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &record),
		"log record must be valid JSON: %q", lines[0])

	return record
}

// TestSetupTagsRecordsWithRunContext is the core of #303: a line logged with a
// context carrying run identity carries the WorkflowID/RunID attributes the web
// UI's log query filters on. The attribute keys are asserted verbatim because a
// rename silently empties the run-detail log panels.
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

// TestSetupWithoutRunContextAddsNoTags confirms the enrichment is inert outside a
// run: process-global logging (cmd/web, worker startup) is unchanged.
func TestSetupWithoutRunContextAddsNoTags(t *testing.T) {
	record := decodeSingleRecord(t, func() {
		logging.Setup("info")
		slog.Info("no run context here")
	})

	assert.NotContains(t, record, "WorkflowID")
	assert.NotContains(t, record, "RunID")
}

// TestSetupOmitsEmptyRunTags confirms a partially-populated context contributes
// only the fields it actually has, rather than emitting empty-string tags that
// would match nothing.
func TestSetupOmitsEmptyRunTags(t *testing.T) {
	record := decodeSingleRecord(t, func() {
		logging.Setup("info")

		ctx := logging.ContextWithRunTags(t.Context(), "", "run-only")
		slog.WarnContext(ctx, "run id but no workflow id")
	})

	assert.NotContains(t, record, "WorkflowID")
	assert.Equal(t, "run-only", record["RunID"])
}

// TestSetupTagsSurviveDerivedLoggers confirms run-tag enrichment is preserved
// through slog.With, the same mechanism the Temporal SDK's structured logger uses
// to attach its own tags — so a derived logger does not drop run identity.
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
