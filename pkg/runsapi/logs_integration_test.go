//go:build integration

// TestGetRunLogsAgainstRealVictoriaLogs (issue #274) exercises
// pkg/runsapi's log-panel endpoint against a real VictoriaLogs instance —
// `make web-dev-observability-up` (docker-compose.web-dev.yml), never
// `make test-integration`'s own Temporal/mhvtl stack, so this skips cleanly
// (like the MHVTL-gated tests elsewhere in this package) whenever
// VICTORIALOGS_URL is not set, which is always true for a plain
// `make test-integration` run. Temporal itself is faked (fakeTemporalClient)
// — only the VictoriaLogs half of this endpoint needs to be real, and the
// dev observability stack is deliberately not wired into the Temporal
// integration stack (Makefile: "never a dependency of
// test-integration/test-e2e").
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

// TestGetRunLogsAgainstRealVictoriaLogs inserts a few synthetic slog-shaped
// lines directly into a real VictoriaLogs instance under a unique fake
// RunID, then drives them back out through GET /api/runs/{runID}/logs —
// proving the LogsQL this package builds (buildLogsQLQuery) is actually
// valid VictoriaLogs syntax, not just something this package's own
// httptest-fake VL server (logs_test.go) happens to accept.
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

	// VictoriaLogs ingestion is asynchronous; a real instance is fast but
	// not synchronous with the insert HTTP call returning, so poll briefly
	// rather than assuming the lines are queryable immediately.
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

// TestGetRunLogsFieldPrefixAgainstRealVictoriaLogs is the merge-key
// counterpart to the test above: it ingests records whose worker fields are
// nested under a "log_fields." prefix (the shape a fluentbit kubernetes filter
// with Merge_Log_Key produces) and _msg holds the raw slog line, then drives
// them back out with VICTORIALOGS_FIELD_PREFIX=log_fields. set — proving the
// prefixed "log_fields.RunID":= filter and the log_fields.msg projection are
// valid against a real VictoriaLogs instance, not just this package's fake.
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

// insertSyntheticLines POSTs lines to VictoriaLogs' JSON stream ingestion
// API, matching the field mapping docker-compose.web-dev.yml's vector
// shipper uses in the real dev stack (docs/web-ui.md): _msg_field=msg,
// _time_field=time, streamed on WorkflowID/RunID.
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

// insertPrefixedLines ingests lines in the fluentbit merge-key shape: the
// worker's slog fields nested under a "<prefix without the dot>" object (which
// VictoriaLogs flattens to "<prefix>time"/"<prefix>level"/... dotted field
// names) and _msg holding the byte-exact raw slog line, exactly as a
// kubernetes filter with Merge_Log_Key produces. VL's own _time is taken from
// a top-level ingestTime field so it stays VictoriaLogs-native.
func insertPrefixedLines(t *testing.T, victoriaLogsURL, prefix, runID string, lines []syntheticLine) {
	t.Helper()

	// The merge object's name is the prefix with its trailing dot removed
	// (e.g. "log_fields." -> "log_fields"): VictoriaLogs flattens {mergeKey:
	// {level: ...}} to "mergeKey.level".
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
