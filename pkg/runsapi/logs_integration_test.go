//go:build integration

// These tests (issue #274) need a real VictoriaLogs from `make
// web-dev-observability-up` and skip unless VICTORIALOGS_URL is set; Temporal
// is faked.
package runsapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
)

// TestGetRunLogsAgainstRealVictoriaLogs proves the generated LogsQL is valid
// against a real VictoriaLogs, not just the httptest fake.
func TestGetRunLogsAgainstRealVictoriaLogs(t *testing.T) {
	victoriaLogsURL := os.Getenv(victoriaLogsURLEnv)
	if victoriaLogsURL == "" {
		t.Skipf("%s not set; run `make web-dev-observability-up` and re-run with VICTORIALOGS_URL=http://localhost:9428", victoriaLogsURLEnv)
	}

	runID := "99999999-8888-4777-8666-" + fmt.Sprintf("%012d", time.Now().UnixNano()%1_000_000_000_000)

	start := time.Now().Add(-time.Hour).UTC()
	line1Time := time.Now().Add(-time.Minute).UTC()
	line2Time := time.Now().Add(-30 * time.Second).UTC()

	insertSyntheticLines(t, victoriaLogsURL, runID, []syntheticLine{
		{Time: line1Time, Level: "INFO", Msg: "resolving snapshots"},
		{Time: line2Time, Level: "WARN", Msg: "pack running slow"},
	})

	getenv := envWith(map[string]string{victoriaLogsURLEnv: victoriaLogsURL})
	temporalClient := &fakeTemporalClient{
		describeResponse: describeResponseFor(runID, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, start, nil),
	}
	handler := newMux(newHandler(temporalClient, getenv))

	// Ingestion is asynchronous, so poll.
	var body RunLogsResponse

	require.Eventually(t, func() bool {
		recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+runID+"/logs", nil)
		if recorder.Code != http.StatusOK {
			return false
		}

		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

		return len(body.Lines) == 2
	}, 10*time.Second, 200*time.Millisecond, "expected 2 lines to become queryable in VictoriaLogs")

	assert.True(t, body.Live)
	assert.Equal(t, "resolving snapshots", body.Lines[0].Message)
	assert.Equal(t, "INFO", body.Lines[0].Level)
	assert.Equal(t, "pack running slow", body.Lines[1].Message)
	assert.Equal(t, "WARN", body.Lines[1].Level)
	assert.True(t, body.Lines[0].Time.Before(body.Lines[1].Time))
}

// TestGetRunLogsFieldPrefixAgainstRealVictoriaLogs is the merge-key-shaped
// counterpart, run with VICTORIALOGS_FIELD_PREFIX=log_fields.
func TestGetRunLogsFieldPrefixAgainstRealVictoriaLogs(t *testing.T) {
	victoriaLogsURL := os.Getenv(victoriaLogsURLEnv)
	if victoriaLogsURL == "" {
		t.Skipf("%s not set; run `make web-dev-observability-up` and re-run with VICTORIALOGS_URL=http://localhost:9428", victoriaLogsURLEnv)
	}

	const prefix = "log_fields."

	runID := "88888888-7777-4666-8555-" + fmt.Sprintf("%012d", time.Now().UnixNano()%1_000_000_000_000)

	start := time.Now().Add(-time.Hour).UTC()
	line1Time := time.Now().Add(-time.Minute).UTC()
	line2Time := time.Now().Add(-30 * time.Second).UTC()

	insertPrefixedLines(t, victoriaLogsURL, prefix, runID, []syntheticLine{
		{Time: line1Time, Level: "INFO", Msg: "resolving snapshots"},
		{Time: line2Time, Level: "WARN", Msg: "pack running slow"},
	})

	getenv := envWith(map[string]string{
		victoriaLogsURLEnv:         victoriaLogsURL,
		victoriaLogsFieldPrefixEnv: prefix,
	})
	temporalClient := &fakeTemporalClient{
		describeResponse: describeResponseFor(runID, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, start, nil),
	}
	handler := newMux(newHandler(temporalClient, getenv))

	var body RunLogsResponse

	require.Eventually(t, func() bool {
		recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+runID+"/logs", nil)
		if recorder.Code != http.StatusOK {
			return false
		}

		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

		return len(body.Lines) == 2
	}, 10*time.Second, 200*time.Millisecond, "expected 2 prefixed lines to become queryable in VictoriaLogs")

	assert.True(t, body.Live)
	assert.Equal(t, "resolving snapshots", body.Lines[0].Message, "the human message comes from the prefixed field, not the raw _msg blob")
	assert.Equal(t, "INFO", body.Lines[0].Level)
	assert.Equal(t, "pack running slow", body.Lines[1].Message)
	assert.Equal(t, "WARN", body.Lines[1].Level)
}

type syntheticLine struct {
	Time  time.Time
	Level string
	Msg   string
}

// insertSyntheticLines ingests lines with the dev vector shipper's field mapping.
func insertSyntheticLines(t *testing.T, victoriaLogsURL, runID string, lines []syntheticLine) {
	t.Helper()

	var builder strings.Builder

	for _, line := range lines {
		fmt.Fprintf(&builder, `{"time":%q,"level":%q,"msg":%q,"WorkflowID":"backup","RunID":%q}`+"\n",
			line.Time.Format(time.RFC3339Nano), line.Level, line.Msg, runID)
	}

	url := strings.TrimSuffix(victoriaLogsURL, "/") + "/insert/jsonline?_stream_fields=WorkflowID,RunID&_msg_field=msg&_time_field=time"

	response, err := http.Post(url, "application/stream+json", strings.NewReader(builder.String()))
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	require.Equal(t, http.StatusOK, response.StatusCode)
}

// insertPrefixedLines ingests lines in the fluentbit Merge_Log_Key shape: slog
// fields nested under the prefix object and _msg holding the raw slog line.
func insertPrefixedLines(t *testing.T, victoriaLogsURL, prefix, runID string, lines []syntheticLine) {
	t.Helper()

	// VictoriaLogs flattens {mergeKey: {level: ...}} to "mergeKey.level".
	mergeKey := strings.TrimSuffix(prefix, ".")

	var builder strings.Builder

	for _, line := range lines {
		workerTime := line.Time.Format(time.RFC3339Nano)
		rawSlogLine := fmt.Sprintf(`{"time":%q,"level":%q,"msg":%q,"WorkflowID":"backup","RunID":%q}`,
			workerTime, line.Level, line.Msg, runID)

		record := map[string]any{
			"ingestTime": workerTime,
			"_msg":       rawSlogLine,
			mergeKey: map[string]any{
				"time":       workerTime,
				"level":      line.Level,
				"msg":        line.Msg,
				"WorkflowID": "backup",
				"RunID":      runID,
			},
		}

		encoded, err := json.Marshal(record)
		require.NoError(t, err)

		builder.Write(encoded)
		builder.WriteByte('\n')
	}

	url := strings.TrimSuffix(victoriaLogsURL, "/") +
		"/insert/jsonline?_stream_fields=" + prefix + "WorkflowID," + prefix + "RunID&_msg_field=_msg&_time_field=ingestTime"

	response, err := http.Post(url, "application/stream+json", strings.NewReader(builder.String()))
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	require.Equal(t, http.StatusOK, response.StatusCode)
}
