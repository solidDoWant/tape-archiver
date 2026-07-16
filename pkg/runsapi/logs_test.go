package runsapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// testRunID is a well-formed UUID (runIDPattern's only requirement) used by
// every logs_test.go case that needs to get past getRunLogs' own validation
// and reach VictoriaLogs/Temporal.
const testRunID = "11111111-2222-4333-8444-555555555555"

// fakeVictoriaLogs stands in for a real VictoriaLogs instance: it records
// every request's "query" parameter (so a test can assert on the LogsQL
// this package builds) and answers with a fixed newline-delimited JSON body
// and status code.
type fakeVictoriaLogs struct {
	*httptest.Server

	queries []string
	status  int
	body    string
}

func newFakeVictoriaLogs(t *testing.T, status int, body string) *fakeVictoriaLogs {
	t.Helper()

	fake := &fakeVictoriaLogs{status: status, body: body}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, victoriaLogsQueryPath, r.URL.Path)
		fake.queries = append(fake.queries, r.URL.Query().Get("query"))
		w.WriteHeader(fake.status)
		_, _ = w.Write([]byte(fake.body))
	}))
	t.Cleanup(fake.Close)

	return fake
}

// vlLine renders one synthetic VictoriaLogs record line, matching the shape
// docs/web-ui.md's vector shipper produces (_time/_msg from a worker's
// slog "time"/"msg" fields, "level" passed through unchanged).
func vlLine(t time.Time, level, msg string) string {
	return fmt.Sprintf(`{"_time":%q,"_msg":%q,"level":%q,"RunID":"x"}`, t.UTC().Format(time.RFC3339Nano), msg, level)
}

func TestGetRunLogsUnconfiguredIsUnavailable(t *testing.T) {
	handler := newMux(newHandler(&fakeTemporalClient{}, emptyEnv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs", nil)

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.NotEmpty(t, decodeAPIError(t, recorder))
}

func TestGetRunLogsUnreachableIsUnavailable(t *testing.T) {
	// A URL nothing listens on: the request fails at the transport level,
	// exercising the "configured but unreachable" half of AC2 rather than
	// AC1's "unconfigured" half above.
	getenv := envWith(map[string]string{victoriaLogsURLEnv: "http://127.0.0.1:1"})

	fake := &fakeTemporalClient{describeResponse: describeResponseFor(testRunID, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, time.Now(), nil)}
	handler := newMux(newHandler(fake, getenv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs", nil)

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.NotEmpty(t, decodeAPIError(t, recorder))
}

func TestGetRunLogsVictoriaLogsErrorIsUnavailable(t *testing.T) {
	fake := newFakeVictoriaLogs(t, http.StatusInternalServerError, "boom")
	getenv := envWith(map[string]string{victoriaLogsURLEnv: fake.URL})

	temporalClient := &fakeTemporalClient{describeResponse: describeResponseFor(testRunID, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, time.Now(), nil)}
	handler := newMux(newHandler(temporalClient, getenv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs", nil)

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.NotEmpty(t, decodeAPIError(t, recorder))
}

func TestGetRunLogsRejectsMalformedInput(t *testing.T) {
	fake := newFakeVictoriaLogs(t, http.StatusOK, "")
	getenv := envWith(map[string]string{victoriaLogsURLEnv: fake.URL})

	tests := []struct {
		name string
		path string
	}{
		{"non-UUID run ID", "/api/runs/not-a-uuid/logs"},
		{"unknown phase", "/api/runs/" + testRunID + "/logs?phase=NotAPhase"},
		{"malformed since", "/api/runs/" + testRunID + "/logs?since=not-a-time"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newMux(newHandler(&fakeTemporalClient{}, getenv))

			recorder := doJSON(t, handler, http.MethodGet, test.path, nil)

			assert.Equal(t, http.StatusBadRequest, recorder.Code)
			require.NotEmpty(t, decodeAPIError(t, recorder))
			assert.Empty(t, fake.queries, "a rejected request must never reach VictoriaLogs")
		})
	}
}

func TestGetRunLogsWholeRunHappyPath(t *testing.T) {
	start := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	close := start.Add(time.Hour)

	line1 := start.Add(time.Minute)
	line2 := start.Add(2 * time.Minute)

	fake := newFakeVictoriaLogs(t, http.StatusOK, vlLine(line1, "INFO", "resolving snapshots")+"\n"+vlLine(line2, "WARN", "pack slow")+"\n")
	getenv := envWith(map[string]string{victoriaLogsURLEnv: fake.URL})

	temporalClient := &fakeTemporalClient{describeResponse: describeResponseFor(testRunID, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start, &close)}
	handler := newMux(newHandler(temporalClient, getenv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body RunLogsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

	assert.Equal(t, testRunID, body.RunID)
	assert.Empty(t, body.Phase)
	assert.False(t, body.Live, "a completed run's window is closed")
	require.Len(t, body.Lines, 2)
	assert.Equal(t, "resolving snapshots", body.Lines[0].Message)
	assert.Equal(t, "INFO", body.Lines[0].Level)
	assert.True(t, body.Lines[0].Time.Equal(line1))
	assert.Equal(t, "pack slow", body.Lines[1].Message)

	require.Len(t, fake.queries, 1)
	query, err := url.QueryUnescape(fake.queries[0])
	require.NoError(t, err)
	assert.Contains(t, query, `RunID:="`+testRunID+`"`)
	assert.Contains(t, query, "(*)")
	assert.Contains(t, query, "sort by (_time)")
	assert.Contains(t, query, fmt.Sprintf("limit %d", maxLogLines))
	// The cap must keep the NEWEST lines, not the oldest: an over-long window's
	// tail (a failure's error lines) must survive truncation. That is expressed
	// as "sort desc | limit | sort asc" — see buildLogsQLQuery.
	assert.Contains(t, query, "sort by (_time) desc")
	assert.Regexp(t, `sort by \(_time\) desc \| limit \d+ \| sort by \(_time\)`, query)
}

// TestGetRunLogsSurfacesErrorField covers the whole point of LogLine.Error: a
// line whose real cause lives in a structured field, not the terse _msg, must
// come through so the log panel can show it. Both conventions are exercised —
// the Temporal SDK's retrying-activity log ("Activity error." + capitalized
// "Error"), and this repo's own slog error logs (lowercase "error").
func TestGetRunLogsSurfacesErrorField(t *testing.T) {
	start := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	close := start.Add(time.Hour)

	sdk := fmt.Sprintf(
		`{"_time":%q,"_msg":"Activity error.","level":"ERROR","RunID":"x","ActivityType":"ResolveK8sSources","Error":"resolve sources[0]: boom"}`,
		start.Add(time.Minute).UTC().Format(time.RFC3339Nano),
	)
	app := fmt.Sprintf(
		`{"_time":%q,"_msg":"prepare failed","level":"ERROR","RunID":"x","error":"disk full"}`,
		start.Add(2*time.Minute).UTC().Format(time.RFC3339Nano),
	)
	plain := fmt.Sprintf(
		`{"_time":%q,"_msg":"resolving snapshots","level":"INFO","RunID":"x"}`,
		start.Add(3*time.Minute).UTC().Format(time.RFC3339Nano),
	)

	fake := newFakeVictoriaLogs(t, http.StatusOK, sdk+"\n"+app+"\n"+plain+"\n")
	getenv := envWith(map[string]string{victoriaLogsURLEnv: fake.URL})

	temporalClient := &fakeTemporalClient{describeResponse: describeResponseFor(testRunID, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start, &close)}
	handler := newMux(newHandler(temporalClient, getenv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body RunLogsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

	require.Len(t, body.Lines, 3)
	assert.Equal(t, "Activity error.", body.Lines[0].Message)
	assert.Equal(t, "resolve sources[0]: boom", body.Lines[0].Error, "the SDK's capitalized Error field must surface")
	assert.Equal(t, "prepare failed", body.Lines[1].Message)
	assert.Equal(t, "disk full", body.Lines[1].Error, "this repo's lowercase error field must surface")
	assert.Empty(t, body.Lines[2].Error, "a line with no error field carries none")
}

func TestGetRunLogsWholeRunStillOpenIsLive(t *testing.T) {
	fake := newFakeVictoriaLogs(t, http.StatusOK, "")
	getenv := envWith(map[string]string{victoriaLogsURLEnv: fake.URL})

	temporalClient := &fakeTemporalClient{describeResponse: describeResponseFor(testRunID, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, time.Now(), nil)}
	handler := newMux(newHandler(temporalClient, getenv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body RunLogsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.True(t, body.Live)
	assert.Empty(t, body.Lines)
}

func TestGetRunLogsSinceNarrowsTheQuery(t *testing.T) {
	fake := newFakeVictoriaLogs(t, http.StatusOK, "")
	getenv := envWith(map[string]string{victoriaLogsURLEnv: fake.URL})

	temporalClient := &fakeTemporalClient{describeResponse: describeResponseFor(testRunID, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, time.Now().Add(-time.Hour), nil)}
	handler := newMux(newHandler(temporalClient, getenv))

	since := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs?since="+url.QueryEscape(since), nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	require.Len(t, fake.queries, 1)
	query, err := url.QueryUnescape(fake.queries[0])
	require.NoError(t, err)
	assert.Contains(t, query, "_time:>="+since,
		"the lower bound must be since, and INCLUSIVE — an exclusive bound would permanently lose "+
			"same-timestamp lines split across a poll boundary by asynchronous log shipping")
}

func TestGetRunLogsPhaseHappyPath(t *testing.T) {
	fake := newFakeVictoriaLogs(t, http.StatusOK, "")
	getenv := envWith(map[string]string{victoriaLogsURLEnv: fake.URL})

	temporalClient := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
	}}
	handler := newMux(newHandler(temporalClient, getenv))

	// "Resolve" completed with a real time window in the fixture (see
	// history_test.go's buildSuccessfulRunHistory) — the completed-phase
	// happy path.
	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs?phase=Resolve", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body RunLogsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, "Resolve", body.Phase)
	assert.False(t, body.Live)
	require.Len(t, fake.queries, 1)
}

// TestGetRunLogsPhaseWindowsAreDisjointAcrossRetries covers the
// interleaved-tape-path hazard logWindow.spans exists for: on a run whose
// first Write failed and was retried after an operator pause
// (Load1 → Write1 fails → pause → Load2 → Write2), each phase's activities
// are NOT contiguous — "Load"'s single [earliest, latest] envelope contains
// the failed Write's and the pause's log lines, and "Write"'s contains the
// second Load's. A phase-scoped request must return only the lines that
// fall inside that phase's own per-activity spans (issue #274 AC3: "the
// matching log lines"), with lines in the gaps between spans excluded.
func TestGetRunLogsPhaseWindowsAreDisjointAcrossRetries(t *testing.T) {
	// eventBuilder times are deterministic: base 2026-01-01T00:00:00Z, +1
	// minute per event (history_test.go). The layout below yields:
	//   Load spans:  [00:02, 00:03] (Load1), [00:08, 00:09] (Load2)
	//   Write spans: [00:04, 00:05] (Write1 fails), [00:06, 00:07] (pause
	//                alert, attributed to Write via its input), [00:10,
	//                00:11] (Write2)
	// so Load's envelope [00:02, 00:09] swallows both Write1 and the pause,
	// and Write's [00:04, 00:11] swallows Load2 — exactly the over-inclusion
	// this test must fail on.
	builder := newEventBuilder()
	builder.started(t, testConfig)

	loadOne := builder.scheduled(t, "Load", nil)
	builder.completed(t, loadOne, nil)

	writeOne := builder.scheduled(t, "WriteTree", nil)
	builder.failed(writeOne, "drive 0: write tree: medium error")

	pause := builder.scheduled(t, "NotifyWritePathPause", backup.WritePathPauseInput{Phase: backup.PhaseWrite})
	builder.completed(t, pause, nil)

	loadTwo := builder.scheduled(t, "Load", nil)
	builder.completed(t, loadTwo, nil)

	writeTwo := builder.scheduled(t, "WriteTree", nil)
	builder.completed(t, writeTwo, nil)

	builder.runCompleted()

	at := func(minute, second int) time.Time {
		return time.Date(2026, 1, 1, 0, minute, second, 0, time.UTC)
	}

	// The fake returns every line for any query — the envelope query is one
	// request either way, so span filtering can only happen server-side,
	// after the response.
	body := strings.Join([]string{
		vlLine(at(2, 30), "INFO", "load one"),
		vlLine(at(4, 30), "ERROR", "write one failing"),
		vlLine(at(6, 30), "WARN", "pause alert"),
		vlLine(at(7, 30), "INFO", "gap line"),
		vlLine(at(8, 30), "INFO", "load two"),
		vlLine(at(10, 30), "INFO", "write two"),
	}, "\n")

	fake := newFakeVictoriaLogs(t, http.StatusOK, body)
	getenv := envWith(map[string]string{victoriaLogsURLEnv: fake.URL})

	temporalClient := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: builder.events}
	}}
	handler := newMux(newHandler(temporalClient, getenv))

	tests := []struct {
		phase        string
		wantMessages []string
	}{
		{phase: "Load", wantMessages: []string{"load one", "load two"}},
		{phase: "Write", wantMessages: []string{"write one failing", "pause alert", "write two"}},
	}

	for _, test := range tests {
		t.Run(test.phase, func(t *testing.T) {
			recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs?phase="+url.QueryEscape(test.phase), nil)
			require.Equal(t, http.StatusOK, recorder.Code)

			var response RunLogsResponse
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))

			messages := make([]string, 0, len(response.Lines))
			for _, logLine := range response.Lines {
				messages = append(messages, logLine.Message)
			}

			assert.Equal(t, test.wantMessages, messages)
		})
	}
}

func TestGetRunLogsPhaseNotYetStartedIsEmptyNotUnavailable(t *testing.T) {
	fake := newFakeVictoriaLogs(t, http.StatusOK, "")
	getenv := envWith(map[string]string{victoriaLogsURLEnv: fake.URL})

	temporalClient := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
	}}
	handler := newMux(newHandler(temporalClient, getenv))

	// "Burn" never ran in the fixture (optical burning disabled) — no
	// activity, so no time window at all yet.
	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs?phase=Burn", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body RunLogsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, "Burn", body.Phase)
	assert.False(t, body.Live)
	assert.Empty(t, body.Lines)
	assert.Empty(t, fake.queries, "no window means no reason to query VictoriaLogs at all")
}

func TestGetRunLogsErrorClassificationMatchesOtherEndpoints(t *testing.T) {
	fake := newFakeVictoriaLogs(t, http.StatusOK, "")
	getenv := envWith(map[string]string{victoriaLogsURLEnv: fake.URL})

	tests := []struct {
		name       string
		path       string
		client     *fakeTemporalClient
		wantStatus int
	}{
		{
			name:       "unknown run (whole-run mode)",
			path:       "/api/runs/" + testRunID + "/logs",
			client:     &fakeTemporalClient{describeErr: &serviceerror.NotFound{Message: "not found"}},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "unknown run (phase mode)",
			path: "/api/runs/" + testRunID + "/logs?phase=Resolve",
			client: &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
				return &fakeHistoryIterator{err: serviceerror.NewNotFound("workflow execution not found")}
			}},
			wantStatus: http.StatusNotFound,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newMux(newHandler(test.client, getenv))

			recorder := doJSON(t, handler, http.MethodGet, test.path, nil)
			assert.Equal(t, test.wantStatus, recorder.Code)
			require.NotEmpty(t, decodeAPIError(t, recorder))
		})
	}
}

// envWith returns a getenv func serving overrides, and "" for anything else
// — the same shape newHandler's getenv parameter expects.
func envWith(overrides map[string]string) func(string) string {
	return func(key string) string {
		return overrides[key]
	}
}

// describeResponseFor builds a DescribeWorkflowExecutionResponse with just
// the fields resolveLogWindow's whole-run path reads (via fetchRunDetail ->
// toRunSummary), reusing executionInfo (runsapi_test.go).
func describeResponseFor(runID string, status enumspb.WorkflowExecutionStatus, start time.Time, closeTime *time.Time) *workflowservice.DescribeWorkflowExecutionResponse {
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: executionInfo(runID, status, start, closeTime),
	}
}
