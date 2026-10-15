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

// testRunID is a UUID that passes runIDPattern.
const testRunID = "11111111-2222-4333-8444-555555555555"

// fakeVictoriaLogs records each request's LogsQL query and answers with a fixed body and status.
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

// vlLine renders one record in the shape the dev vector shipper produces.
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
	// Nothing listens here: configured but unreachable (AC2).
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
	// The cap must keep the newest lines so a failure's tail survives truncation.
	assert.Contains(t, query, "sort by (_time) desc")
	assert.Regexp(t, `sort by \(_time\) desc \| limit \d+ \| sort by \(_time\)`, query)
}

// TestGetRunLogsSurfacesErrorField covers both the Temporal SDK's "Error" and
// this repo's slog "error" field conventions.
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

func TestGetRunLogsOversizedLineIsSkippedNotFatal(t *testing.T) {
	start := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	close := start.Add(time.Hour)

	good1 := vlLine(start.Add(time.Minute), "INFO", "before the big line")
	// A single record larger than maxLogLineBytes (a huge structured field).
	huge := fmt.Sprintf(`{"_time":%q,"_msg":%q,"level":"INFO","RunID":"x"}`,
		start.Add(2*time.Minute).UTC().Format(time.RFC3339Nano), strings.Repeat("x", maxLogLineBytes+1))
	good2 := vlLine(start.Add(3*time.Minute), "WARN", "after the big line")

	fake := newFakeVictoriaLogs(t, http.StatusOK, good1+"\n"+huge+"\n"+good2+"\n")
	getenv := envWith(map[string]string{victoriaLogsURLEnv: fake.URL})

	temporalClient := &fakeTemporalClient{describeResponse: describeResponseFor(testRunID, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start, &close)}
	handler := newMux(newHandler(temporalClient, getenv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs", nil)
	require.Equal(t, http.StatusOK, recorder.Code, "one oversized line must not fail the whole response")

	var body RunLogsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Len(t, body.Lines, 2, "both good lines survive; only the oversized one is skipped")
	assert.Equal(t, "before the big line", body.Lines[0].Message)
	assert.Equal(t, "after the big line", body.Lines[1].Message, "the tail after the oversized line is not dropped")
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

	// "Resolve" completed with a real time window in the fixture.
	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs?phase=Resolve", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body RunLogsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, "Resolve", body.Phase)
	assert.False(t, body.Live)
	require.Len(t, fake.queries, 1)
}

// TestGetRunLogsPhaseWindowsAreDisjointAcrossRetries checks that interleaved
// Load/Write retries (Load1, Write1 fails, pause, Load2, Write2) return only
// lines inside each phase's own activity spans, not its whole envelope.
func TestGetRunLogsPhaseWindowsAreDisjointAcrossRetries(t *testing.T) {
	// Events are one minute apart from 00:00, giving Load spans [00:02, 00:03],
	// [00:08, 00:09] and Write spans [00:04, 00:05], [00:06, 00:07] (pause),
	// [00:10, 00:11].
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

	// The fake returns every line, so filtering must happen after the response.
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

// TestGetRunLogsPhaseQueryRestrictsLimitToSpans checks the span disjunction is
// applied before the limit, so an interleaved phase cannot crowd out this one.
func TestGetRunLogsPhaseQueryRestrictsLimitToSpans(t *testing.T) {
	builder := newEventBuilder()
	builder.started(t, testConfig)

	loadOne := builder.scheduled(t, "Load", nil)
	builder.completed(t, loadOne, nil)

	writeOne := builder.scheduled(t, "WriteTree", nil)
	builder.failed(writeOne, "drive 0: write tree: medium error")

	loadTwo := builder.scheduled(t, "Load", nil)
	builder.completed(t, loadTwo, nil)

	writeTwo := builder.scheduled(t, "WriteTree", nil)
	builder.completed(t, writeTwo, nil)

	builder.runCompleted()

	fake := newFakeVictoriaLogs(t, http.StatusOK, "")
	getenv := envWith(map[string]string{victoriaLogsURLEnv: fake.URL})

	temporalClient := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: builder.events}
	}}
	handler := newMux(newHandler(temporalClient, getenv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs?phase=Load", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	require.Len(t, fake.queries, 1)
	query, err := url.QueryUnescape(fake.queries[0])
	require.NoError(t, err)

	// Two disjoint Load spans yield a _time disjunction...
	assert.Regexp(t, `\(_time:\[[^()]*\] OR _time:\[[^()]*\]\)`, query,
		"a phase with disjoint activity spans must AND a _time disjunction into the query")
	// ...applied before the limit pipe.
	assert.Regexp(t, `OR _time:\[[^()]*\]\).*\| sort by \(_time\) desc \| limit \d+`, query)
}

func TestGetRunLogsPhaseNotYetStartedIsEmptyNotUnavailable(t *testing.T) {
	fake := newFakeVictoriaLogs(t, http.StatusOK, "")
	getenv := envWith(map[string]string{victoriaLogsURLEnv: fake.URL})

	temporalClient := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
	}}
	handler := newMux(newHandler(temporalClient, getenv))

	// "Burn" never ran in the fixture.
	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs?phase=Burn", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body RunLogsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, "Burn", body.Phase)
	assert.False(t, body.Live, "a closed run's never-reached phase is genuinely final")
	assert.Empty(t, body.Lines)
	assert.Empty(t, fake.queries, "no window means no reason to query VictoriaLogs at all")
}

func TestGetRunLogsPhaseNotYetStartedOnLiveRunStaysLive(t *testing.T) {
	fake := newFakeVictoriaLogs(t, http.StatusOK, "")
	getenv := envWith(map[string]string{victoriaLogsURLEnv: fake.URL})

	// Resolve ran but the run is still open, so a later phase must stay Live.
	b := newEventBuilder()
	b.started(t, testConfig)
	resolve := b.scheduled(t, "ResolveAndCheck", backup.ResolveDataInput{Config: testConfig})
	b.completed(t, resolve, []backup.ResolvedArchive{{SourceIndex: 0, Label: "archive-0"}})
	// Deliberately no b.runCompleted(): the run is still running.
	events := b.events

	temporalClient := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: events}
	}}
	handler := newMux(newHandler(temporalClient, getenv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs?phase=Burn", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body RunLogsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, "Burn", body.Phase)
	assert.True(t, body.Live, "a not-yet-reached phase on a running run must stay live so the client keeps polling")
	assert.Empty(t, body.Lines)
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

// TestBuildLogsQLQueryFieldPrefix pins the query with and without a field prefix.
func TestBuildLogsQLQueryFieldPrefix(t *testing.T) {
	start := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	tests := []struct {
		name   string
		prefix string
		want   string
	}{
		{
			name:   "no prefix keeps the original top-level RunID field",
			prefix: "",
			want: fmt.Sprintf(
				`(*) AND RunID:=%q AND _time:[%s, %s] | sort by (_time) desc | limit %d | sort by (_time)`,
				testRunID, formatVLTime(start), formatVLTime(end), maxLogLines,
			),
		},
		{
			name:   "merge-key prefix quotes and prefixes only the RunID field",
			prefix: "log_fields.",
			want: fmt.Sprintf(
				`(*) AND "log_fields.RunID":=%q AND _time:[%s, %s] | sort by (_time) desc | limit %d | sort by (_time)`,
				testRunID, formatVLTime(start), formatVLTime(end), maxLogLines,
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := buildLogsQLQuery("*", test.prefix, testRunID, start, &end, nil, nil, maxLogLines)
			assert.Equal(t, test.want, got)
		})
	}
}

// TestParseLogsQLRecordFieldPrefix decodes realistic records from both the
// merge-key (prefixed) and top-level pipeline shapes.
func TestParseLogsQLRecordFieldPrefix(t *testing.T) {
	tests := []struct {
		name        string
		prefix      string
		raw         string
		wantLevel   string
		wantMessage string
		wantError   string
	}{
		{
			name:   "merge-key INFO line reads log_fields.msg, not the raw _msg blob",
			prefix: "log_fields.",
			raw: `{"_time":"2026-07-19T18:40:12.483920117Z",` +
				`"_msg":"{\"time\":\"2026-07-19T18:40:12.483872052Z\",\"level\":\"INFO\",\"msg\":\"resolved snapshot for archival\",\"RunID\":\"01983f2e-7c41-7f8a-9b3d-2a6c58e01f77\",\"size_bytes\":48318382080}",` +
				`"log_fields.level":"INFO","log_fields.msg":"resolved snapshot for archival",` +
				`"log_fields.RunID":"01983f2e-7c41-7f8a-9b3d-2a6c58e01f77","log_fields.size_bytes":"48318382080",` +
				`"kubernetes.namespace.name":"backup","stream":"stderr"}`,
			wantLevel:   "INFO",
			wantMessage: "resolved snapshot for archival",
			wantError:   "",
		},
		{
			name:   "merge-key ERROR line surfaces the capitalized Error field",
			prefix: "log_fields.",
			raw: `{"_time":"2026-07-19T18:41:03.102455890Z",` +
				`"_msg":"{\"level\":\"ERROR\",\"msg\":\"Activity error.\"}",` +
				`"log_fields.level":"ERROR","log_fields.msg":"Activity error.",` +
				`"log_fields.ActivityType":"PrepareStaging","log_fields.Attempt":"3",` +
				`"log_fields.Error":"prepare staging: dataset busy: cannot hold snapshot: pool suspended"}`,
			wantLevel:   "ERROR",
			wantMessage: "Activity error.",
			wantError:   "prepare staging: dataset busy: cannot hold snapshot: pool suspended",
		},
		{
			name:        "merge-key line surfaces the lowercase error field",
			prefix:      "log_fields.",
			raw:         `{"_time":"2026-07-19T18:41:03Z","log_fields.level":"ERROR","log_fields.msg":"prepare failed","log_fields.error":"disk full"}`,
			wantLevel:   "ERROR",
			wantMessage: "prepare failed",
			wantError:   "disk full",
		},
		{
			name:   "merge-key line missing log_fields.msg falls back to _msg",
			prefix: "log_fields.",
			// No log_fields.msg at all, so _msg is used.
			raw:         `{"_time":"2026-07-19T18:41:03Z","_msg":"fallback message","log_fields.level":"WARN"}`,
			wantLevel:   "WARN",
			wantMessage: "fallback message",
			wantError:   "",
		},
		{
			name:        "legacy shape reads _msg and top-level fields",
			prefix:      "",
			raw:         `{"_time":"2026-07-19T18:40:12.483872052Z","level":"INFO","RunID":"01983f2e-7c41-7f8a-9b3d-2a6c58e01f77","size_bytes":48318382080,"_msg":"resolved snapshot for archival"}`,
			wantLevel:   "INFO",
			wantMessage: "resolved snapshot for archival",
			wantError:   "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			line, ok := parseLogsQLRecord([]byte(test.raw), test.prefix)
			require.True(t, ok)
			assert.Equal(t, test.wantLevel, line.Level)
			assert.Equal(t, test.wantMessage, line.Message)
			assert.Equal(t, test.wantError, line.Error)
		})
	}
}

// TestGetRunLogsFieldPrefixEndToEnd drives VICTORIALOGS_FIELD_PREFIX through the whole handler.
func TestGetRunLogsFieldPrefixEndToEnd(t *testing.T) {
	start := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	close := start.Add(time.Hour)
	lineTime := start.Add(time.Minute).UTC().Format(time.RFC3339Nano)

	body := fmt.Sprintf(
		`{"_time":%q,"_msg":"{\"msg\":\"raw json blob\"}","log_fields.level":"INFO","log_fields.msg":"human message","log_fields.RunID":"x"}`,
		lineTime,
	)

	fake := newFakeVictoriaLogs(t, http.StatusOK, body+"\n")
	getenv := envWith(map[string]string{
		victoriaLogsURLEnv:         fake.URL,
		victoriaLogsFieldPrefixEnv: "log_fields.",
	})

	temporalClient := &fakeTemporalClient{describeResponse: describeResponseFor(testRunID, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start, &close)}
	handler := newMux(newHandler(temporalClient, getenv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+testRunID+"/logs", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var response RunLogsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))

	require.Len(t, response.Lines, 1)
	assert.Equal(t, "human message", response.Lines[0].Message, "the panel shows the prefixed message, not the raw _msg JSON")
	assert.Equal(t, "INFO", response.Lines[0].Level)

	require.Len(t, fake.queries, 1)
	query, err := url.QueryUnescape(fake.queries[0])
	require.NoError(t, err)
	assert.Contains(t, query, `"log_fields.RunID":="`+testRunID+`"`, "the query filters on the prefixed, quoted RunID field")
	assert.NotContains(t, query, ` RunID:=`, "the unprefixed RunID field must not appear")
}

// envWith returns a getenv serving overrides and "" otherwise.
func envWith(overrides map[string]string) func(string) string {
	return func(key string) string {
		return overrides[key]
	}
}

// describeResponseFor builds a minimal DescribeWorkflowExecutionResponse.
func describeResponseFor(runID string, status enumspb.WorkflowExecutionStatus, start time.Time, closeTime *time.Time) *workflowservice.DescribeWorkflowExecutionResponse {
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: executionInfo(runID, status, start, closeTime),
	}
}
