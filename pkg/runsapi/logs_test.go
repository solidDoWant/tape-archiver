package runsapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
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
	assert.Contains(t, query, "_time:>", "an exclusive lower bound is used once ?since= is given")
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
