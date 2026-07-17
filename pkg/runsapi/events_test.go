package runsapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// setEventPollInterval overrides the package-level eventPollInterval for the
// duration of a test, restored via t.Cleanup. Overriding the same
// interval a real stream polls at is what makes exercising "push only on
// change" and "close on terminal" behavior fast (milliseconds) rather than
// tied to the real 2-second production interval.
//
// This is race-safe with -race despite mutating a package-level var: it must
// be called (and the interval set) strictly before the HTTP server under
// test starts (httptest.NewServer/http.Server.Serve both begin with a "go"
// statement), so the write happens-before every goroutine the server spawns
// to handle a connection — the same happens-before rule the race detector
// already relies on for any "go f()" call.
func setEventPollInterval(t *testing.T, interval time.Duration) {
	t.Helper()

	original := eventPollInterval
	eventPollInterval = interval

	t.Cleanup(func() { eventPollInterval = original })
}

// dynamicTemporalClient is a TemporalClient fake whose DescribeWorkflowExecution
// and QueryWorkflow answers reflect a status/phase that a test can change at
// will (setState) while a stream is connected — standing in for a run whose
// state actually evolves over time, which a static fixture (like
// fakeTemporalClient elsewhere in this package) cannot represent. Embedding
// fakeTemporalClient supplies harmless zero-value ListWorkflow/ExecuteWorkflow
// implementations (unused by streamRunEvents) without hand-rolling a second
// full TemporalClient fake.
type dynamicTemporalClient struct {
	fakeTemporalClient

	mu            sync.Mutex
	status        enumspb.WorkflowExecutionStatus
	phase         string
	pause         backup.CurrentPause
	pauseErr      error
	describeCalls int
}

func (c *dynamicTemporalClient) setState(status enumspb.WorkflowExecutionStatus, phase string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.status = status
	c.phase = phase
}

// setPause changes the pause state a subsequent CurrentPauseQuery answers
// with, so a test can simulate an operator-in-the-loop pause starting or
// clearing while a stream is connected — the same "state a test can change
// at will" role setState plays for status/phase.
func (c *dynamicTemporalClient) setPause(pause backup.CurrentPause) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.pause = pause
}

// setPauseErr makes a subsequent CurrentPauseQuery fail with err (nil to go
// back to answering normally with the current setPause value) — a test can
// toggle this to simulate a transient query blip mid-stream without
// disturbing DescribeWorkflowExecution/LastCompletedPhaseQuery, the same way
// a real transient "no worker polling" failure would only ever affect the
// one query that hit it.
func (c *dynamicTemporalClient) setPauseErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.pauseErr = err
}

func (c *dynamicTemporalClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.describeCalls
}

func (c *dynamicTemporalClient) DescribeWorkflowExecution(_ context.Context, _, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.describeCalls++

	info := &workflowpb.WorkflowExecutionInfo{
		Execution: &commonpb.WorkflowExecution{WorkflowId: backup.WorkflowID, RunId: runID},
		Status:    c.status,
		StartTime: timestamppb.New(time.Now()),
	}

	if c.status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		info.CloseTime = timestamppb.New(time.Now())
	}

	return &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: info}, nil
}

// QueryWorkflow routes by queryType — backup.CurrentPauseQuery answers with
// the dynamic pause state (setPause), anything else (LastCompletedPhaseQuery
// in practice) with the dynamic phase (setState) — mirroring how the real
// workflow answers two independent query handlers rather than collapsing
// both onto one fixture value.
func (c *dynamicTemporalClient) QueryWorkflow(_ context.Context, _, _, queryType string, _ ...interface{}) (converter.EncodedValue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if queryType == backup.CurrentPauseQuery {
		if c.pauseErr != nil {
			return nil, c.pauseErr
		}

		return fakeEncodedValue{value: c.pause}, nil
	}

	return fakeEncodedValue{value: c.phase}, nil
}

// sseFrame is one parsed "event: NAME\ndata: JSON\n\n" frame.
type sseFrame struct {
	event string
	data  string
}

// readSSEFrames parses body as a stream of SSE frames on a background
// goroutine, delivering each one on the returned channel, which is closed
// once body hits EOF or another read error (in particular, once the server
// closes the connection).
func readSSEFrames(body io.Reader) <-chan sseFrame {
	frames := make(chan sseFrame, 16)

	go func() {
		defer close(frames)

		reader := bufio.NewReader(body)

		for {
			eventLine, err := reader.ReadString('\n')
			if err != nil {
				return
			}

			dataLine, err := reader.ReadString('\n')
			if err != nil {
				return
			}

			// The blank line terminating the frame.
			if _, err := reader.ReadString('\n'); err != nil {
				return
			}

			frames <- sseFrame{
				event: strings.TrimPrefix(strings.TrimSuffix(eventLine, "\n"), "event: "),
				data:  strings.TrimPrefix(strings.TrimSuffix(dataLine, "\n"), "data: "),
			}
		}
	}()

	return frames
}

// waitForFrame reads one frame from frames, failing the test if none arrives
// within timeout or the stream closes first.
func waitForFrame(t *testing.T, frames <-chan sseFrame, timeout time.Duration) sseFrame {
	t.Helper()

	select {
	case frame, ok := <-frames:
		require.True(t, ok, "SSE stream closed while waiting for a frame")

		return frame
	case <-time.After(timeout):
		t.Fatal("timed out waiting for an SSE frame")

		return sseFrame{}
	}
}

// requireNoFrame asserts no frame arrives on frames within window, proving a
// quiescent (unchanged) run does not produce redundant events.
func requireNoFrame(t *testing.T, frames <-chan sseFrame, window time.Duration) {
	t.Helper()

	select {
	case frame, ok := <-frames:
		if ok {
			t.Fatalf("unexpected SSE frame during a quiet period: %+v", frame)
		}
	case <-time.After(window):
	}
}

// assertFrame decodes frame's data as a RunDetail and checks it against the
// expected event name, status, and phase.
func assertFrame(t *testing.T, frame sseFrame, wantEvent string, wantStatus enumspb.WorkflowExecutionStatus, wantPhase string) {
	t.Helper()

	assert.Equal(t, wantEvent, frame.event)

	var detail RunDetail

	require.NoError(t, json.Unmarshal([]byte(frame.data), &detail))
	assert.Equal(t, wantStatus.String(), detail.Status)
	assert.Equal(t, wantPhase, detail.LastCompletedPhase)
}

// TestStreamRunEventsInitialError covers every way the very first poll can
// fail: the response must be a normal JSON error with the same
// statusForTemporalError classification getRun uses, on a plain
// "application/json" response — never a 200 text/event-stream that then
// fails inside the stream body.
func TestCurrentPauseEqual(t *testing.T) {
	base := CurrentPauseInfo{
		Kind:          "write-failure",
		Phase:         "Write",
		AffectedTapes: []string{"TA0001L6"},
		ReloadSlots:   []int{1},
	}

	t.Run("nil and empty slices are the same state (no spurious update)", func(t *testing.T) {
		// backup.CurrentPauseQuery can return either a nil or an empty slice for
		// the same "nothing affected" state across polls; the delta check must not
		// treat that as a change (reflect.DeepEqual did).
		withNil := CurrentPauseInfo{Kind: "eject"}
		withEmpty := CurrentPauseInfo{Kind: "eject", AffectedTapes: []string{}, ReloadSlots: []int{}, Devices: []string{}}

		assert.True(t, currentPauseEqual(withNil, withEmpty))
		assert.True(t, currentPauseEqual(withEmpty, withNil))
	})

	t.Run("a real slice difference is a change", func(t *testing.T) {
		other := base
		other.AffectedTapes = []string{"TA0002L6"}

		assert.False(t, currentPauseEqual(base, other))
	})

	t.Run("a scalar difference is a change", func(t *testing.T) {
		unknown := base
		unknown.Unknown = true
		assert.False(t, currentPauseEqual(base, unknown))

		differentKind := base
		differentKind.Kind = "burn"
		assert.False(t, currentPauseEqual(base, differentKind))
	})
}

func TestStreamRunEventsInitialError(t *testing.T) {
	tests := []struct {
		name       string
		runID      string
		client     *fakeTemporalClient
		wantStatus int
	}{
		{
			name:       "an unknown run returns 404",
			runID:      "does-not-exist",
			client:     &fakeTemporalClient{describeErr: &serviceerror.NotFound{Message: "not found"}},
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "a malformed run ID returns 400",
			runID:      "not-a-uuid",
			client:     &fakeTemporalClient{describeErr: &serviceerror.InvalidArgument{Message: "Invalid RunId."}},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "a transient Temporal error returns 502",
			runID:      "run-1",
			client:     &fakeTemporalClient{describeErr: assertError{"transient RPC failure"}},
			wantStatus: http.StatusBadGateway,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := New(test.client)

			recorder := doJSON(t, handler, http.MethodGet, "/api/events/runs/"+test.runID, nil)
			assert.Equal(t, test.wantStatus, recorder.Code)
			assert.Equal(t, "application/json", recorder.Header().Get("Content-Type"))

			require.NotEmpty(t, decodeAPIError(t, recorder))
		})
	}
}

// TestStreamRunEventsPushesOnlyOnChangeThenClosesOnTerminal drives a real SSE
// connection (httptest.NewServer, a real client) against a run whose state a
// dynamicTemporalClient lets the test control mid-stream: it proves an
// unchanged run produces no redundant frames, a real status/phase change
// produces exactly one new "update" frame, and reaching a terminal status
// produces a final "update" followed by a "done" frame and then the server
// closes the connection on its own (no forever-open stream).
func TestStreamRunEventsPushesOnlyOnChangeThenClosesOnTerminal(t *testing.T) {
	setEventPollInterval(t, 15*time.Millisecond)

	client := &dynamicTemporalClient{}
	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "")

	server := httptest.NewServer(New(client))
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/events/runs/run-1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	frames := readSSEFrames(resp.Body)

	first := waitForFrame(t, frames, 2*time.Second)
	assertFrame(t, first, sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "")

	// The run's state has not changed: several poll intervals must pass with
	// no further frame.
	requireNoFrame(t, frames, 150*time.Millisecond)

	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Verify")

	second := waitForFrame(t, frames, 2*time.Second)
	assertFrame(t, second, sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Verify")

	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, "Verify")

	third := waitForFrame(t, frames, 2*time.Second)
	assertFrame(t, third, sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, "Verify")

	fourth := waitForFrame(t, frames, 2*time.Second)
	assertFrame(t, fourth, sseEventDone, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, "Verify")

	// The server must close the connection itself once terminal, not leave
	// it open forever waiting on the client.
	select {
	case _, ok := <-frames:
		assert.False(t, ok, "expected no further frames after the done event")
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close after the terminal done event")
	}
}

// TestStreamRunEventsPushesOnPauseChangeAlone covers the CurrentPause half of
// streamRunEvents' delta check (events.go): an operator-in-the-loop pause
// starting or clearing pushes a new "update" event even when neither Status
// nor LastCompletedPhase changes at the same moment — exactly the case a
// Load/Write-failure or Eject pause is (SPEC §4.3): the run stays RUNNING and
// the last *completed* phase does not advance while paused mid-phase. Without
// the CurrentPause comparison in the delta check, a client watching the live
// stream would never learn a pause started (or cleared) until some other,
// unrelated field happened to change too.
func TestStreamRunEventsPushesOnPauseChangeAlone(t *testing.T) {
	setEventPollInterval(t, 15*time.Millisecond)

	client := &dynamicTemporalClient{}
	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Load")

	server := httptest.NewServer(New(client))
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/events/runs/run-1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)

	frames := readSSEFrames(resp.Body)

	first := waitForFrame(t, frames, 2*time.Second)
	assertFrame(t, first, sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Load")

	// Status and phase are unchanged for the rest of this test — only the
	// pause state moves — so any further frame must come from the
	// CurrentPause comparison, not the Status/LastCompletedPhase one.
	requireNoFrame(t, frames, 150*time.Millisecond)

	client.setPause(backup.CurrentPause{
		Kind:          backup.PauseWriteFailure,
		Phase:         backup.PhaseWrite,
		AffectedTapes: []string{"TA0001L6"},
	})

	second := waitForFrame(t, frames, 2*time.Second)
	assert.Equal(t, sseEventUpdate, second.event)

	var pausedDetail RunDetail
	require.NoError(t, json.Unmarshal([]byte(second.data), &pausedDetail))
	assert.Equal(t, "write-failure", pausedDetail.CurrentPause.Kind, "the pause starting must push a new event")
	assert.Equal(t, "Load", pausedDetail.LastCompletedPhase, "the last completed phase is genuinely unchanged")

	requireNoFrame(t, frames, 150*time.Millisecond)

	client.setPause(backup.CurrentPause{})

	third := waitForFrame(t, frames, 2*time.Second)
	assert.Equal(t, sseEventUpdate, third.event)

	var clearedDetail RunDetail
	require.NoError(t, json.Unmarshal([]byte(third.data), &clearedDetail))
	assert.Equal(t, "", clearedDetail.CurrentPause.Kind, "the pause clearing (on resume) must push a new event too")
}

// TestStreamRunEventsPauseQueryFailureDoesNotFabricateHealthy covers a
// review finding on #250/PR #252: a transient CurrentPauseQuery failure
// mid-stream must never make a genuinely paused run look like it cleared.
// fetchRunDetail reports this tick's CurrentPause as Unknown rather than
// empty, and streamRunEvents must carry the last known real pause state
// forward into the event it pushes (triggered here by an unrelated phase
// change), not silently report "not paused".
func TestStreamRunEventsPauseQueryFailureDoesNotFabricateHealthy(t *testing.T) {
	setEventPollInterval(t, 15*time.Millisecond)

	client := &dynamicTemporalClient{}
	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Load")
	client.setPause(backup.CurrentPause{Kind: backup.PauseWriteFailure, Phase: backup.PhaseWrite, AffectedTapes: []string{"TA0001L6"}})

	server := httptest.NewServer(New(client))
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/events/runs/run-1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)

	frames := readSSEFrames(resp.Body)

	first := waitForFrame(t, frames, 2*time.Second)

	var initial RunDetail
	require.NoError(t, json.Unmarshal([]byte(first.data), &initial))
	require.Equal(t, "write-failure", initial.CurrentPause.Kind, "test setup: the stream must start out reporting the real pause")
	require.False(t, initial.CurrentPause.Unknown)

	// Simulate the query blip, then force an observable event by changing
	// the phase — if fetchRunDetail's Unknown result were compared directly
	// (instead of being carried forward), this would push CurrentPause.Kind
	// == "" alongside the phase change, exactly the bug being guarded
	// against here.
	client.setPauseErr(assertError{"no worker polling"})
	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Verify")

	second := waitForFrame(t, frames, 2*time.Second)
	assert.Equal(t, sseEventUpdate, second.event)

	var duringBlip RunDetail
	require.NoError(t, json.Unmarshal([]byte(second.data), &duringBlip))
	assert.Equal(t, "Verify", duringBlip.LastCompletedPhase, "the phase change that triggered this event must still be visible")
	assert.Equal(t, "write-failure", duringBlip.CurrentPause.Kind,
		"a transient CurrentPauseQuery failure must carry the last known real pause forward, never report the run as healthy")
	assert.False(t, duringBlip.CurrentPause.Unknown,
		"the carried-forward value is the last known GOOD state, not an unknown one, once it's been folded into an emitted event")

	// The query recovers; a real pause-clear is still detected normally.
	client.setPauseErr(nil)
	client.setPause(backup.CurrentPause{})

	third := waitForFrame(t, frames, 2*time.Second)

	var recovered RunDetail
	require.NoError(t, json.Unmarshal([]byte(third.data), &recovered))
	assert.Equal(t, "", recovered.CurrentPause.Kind, "once the query recovers, a genuine pause-clear is still detected")
}

// TestStreamRunEventsStopsPollingOnClientDisconnect proves the server-side
// poll loop actually stops — rather than leaking a goroutine that polls
// Temporal forever — once the client disconnects, by observing that
// DescribeWorkflowExecution call growth stops shortly after the client
// closes its side of the connection.
func TestStreamRunEventsStopsPollingOnClientDisconnect(t *testing.T) {
	setEventPollInterval(t, 10*time.Millisecond)

	client := &dynamicTemporalClient{}
	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "")

	server := httptest.NewServer(New(client))
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/events/runs/run-1")
	require.NoError(t, err)

	frames := readSSEFrames(resp.Body)
	waitForFrame(t, frames, 2*time.Second)

	// Disconnect from the client side.
	require.NoError(t, resp.Body.Close())

	// Give the server a generous window to observe the disconnect and stop
	// polling, then confirm the call count has actually stopped growing —
	// not just slowed down.
	require.Eventually(t, func() bool {
		before := client.callCount()

		time.Sleep(100 * time.Millisecond)

		return client.callCount() == before
	}, 3*time.Second, 50*time.Millisecond, "server never stopped polling after the client disconnected")
}

// TestStreamRunEventsSurvivesServerWriteTimeout proves the fix for the exact
// interaction this sub-issue flagged as untested: net/http's
// Server.WriteTimeout deadline is computed once, when a request's headers
// are read (confirmed against Go's net/http source), and is never reset per
// Write — so without streamRunEvents explicitly clearing it via
// http.ResponseController.SetWriteDeadline, an SSE connection would be
// killed by the server the moment that deadline elapsed, no matter how
// active the stream still was. A short (150ms) WriteTimeout that an
// actively-polling, 300ms+ -long stream survives here is conclusive: this
// would fail immediately, at ~150ms, without the fix — it cannot pass by
// accident.
func TestStreamRunEventsSurvivesServerWriteTimeout(t *testing.T) {
	setEventPollInterval(t, 20*time.Millisecond)

	client := &dynamicTemporalClient{}
	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := &http.Server{
		Handler:      New(client),
		WriteTimeout: 150 * time.Millisecond,
	}
	go func() { _ = server.Serve(listener) }()

	t.Cleanup(func() { _ = server.Close() })

	resp, err := http.Get("http://" + listener.Addr().String() + "/api/events/runs/run-1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	frames := readSSEFrames(resp.Body)

	first := waitForFrame(t, frames, time.Second)
	assertFrame(t, first, sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "")

	// Stay connected, observing real updates, for well past the server's
	// 150ms WriteTimeout.
	time.Sleep(300 * time.Millisecond)

	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Verify")

	second := waitForFrame(t, frames, time.Second)
	assertFrame(t, second, sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Verify")

	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, "Verify")

	third := waitForFrame(t, frames, time.Second)
	assertFrame(t, third, sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, "Verify")

	fourth := waitForFrame(t, frames, time.Second)
	assertFrame(t, fourth, sseEventDone, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, "Verify")
}

// TestStreamRunEventsClosesOnDrain proves an open, healthy SSE stream over a
// still-RUNNING run ends promptly once the WithDrainContext context is
// cancelled — the mechanism cmd/web uses so a graceful SIGTERM drain
// (http.Server.Shutdown) is not held at its full deadline by open browser
// tabs (issue #270). Without the drain case in streamRunEvents' select, this
// test would hang at the final read until its timeout: nothing else ends a
// healthy stream over a running run.
func TestStreamRunEventsClosesOnDrain(t *testing.T) {
	setEventPollInterval(t, 15*time.Millisecond)

	client := &dynamicTemporalClient{}
	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "")

	drainCtx, startDrain := context.WithCancel(t.Context())

	server := httptest.NewServer(New(client, WithDrainContext(drainCtx)))
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/events/runs/run-1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	frames := readSSEFrames(resp.Body)

	// The stream is established and healthy: the initial snapshot arrives.
	first := waitForFrame(t, frames, 2*time.Second)
	assertFrame(t, first, sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "")

	// Begin the graceful drain, exactly as cmd/web does right before
	// srv.Shutdown.
	startDrain()

	// The server must close the still-RUNNING stream on its own, promptly,
	// without any further frames.
	select {
	case frame, ok := <-frames:
		assert.False(t, ok, "expected the stream to close without further frames on drain, got %+v", frame)
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close after the drain context was cancelled")
	}
}

// TestStreamRunEventsBroadcastsToMultipleConnections proves the shared poll loop
// fans a single run's updates out to every connection watching it: two
// concurrent streams over the same run both see the initial snapshot and a
// mid-stream change (issue #250 review — one poller, many subscribers).
func TestStreamRunEventsBroadcastsToMultipleConnections(t *testing.T) {
	setEventPollInterval(t, 15*time.Millisecond)

	client := &dynamicTemporalClient{}
	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Load")

	server := httptest.NewServer(New(client))
	t.Cleanup(server.Close)

	respA, err := http.Get(server.URL + "/api/events/runs/run-1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = respA.Body.Close() })

	respB, err := http.Get(server.URL + "/api/events/runs/run-1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = respB.Body.Close() })

	framesA := readSSEFrames(respA.Body)
	framesB := readSSEFrames(respB.Body)

	// Both connections receive the initial snapshot; waiting for both here also
	// guarantees both are subscribed before the state change below.
	assertFrame(t, waitForFrame(t, framesA, 2*time.Second), sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Load")
	assertFrame(t, waitForFrame(t, framesB, 2*time.Second), sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Load")

	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Verify")

	assertFrame(t, waitForFrame(t, framesA, 2*time.Second), sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Verify")
	assertFrame(t, waitForFrame(t, framesB, 2*time.Second), sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Verify")
}

// TestRunBrokerCoalescesPollersPerRun is a white-box check that the SSE broker
// runs one shared poll loop per run no matter how many connections watch it, and
// tears a run's poller down once its last subscriber leaves — the mechanism that
// makes N browser tabs cost one interval of Temporal load, not N.
func TestRunBrokerCoalescesPollersPerRun(t *testing.T) {
	broker := newRunBroker(func(context.Context, string) (RunDetail, error) {
		return RunDetail{RunSummary: RunSummary{Status: runningStatus}}, nil
	}, context.Background())

	subscriberCount := func(runID string) (int, bool) {
		broker.mu.Lock()
		defer broker.mu.Unlock()

		poll, ok := broker.polls[runID]
		if !ok {
			return 0, false
		}

		return len(poll.subscribers), true
	}

	pollCount := func() int {
		broker.mu.Lock()
		defer broker.mu.Unlock()

		return len(broker.polls)
	}

	subA := broker.subscribe("run-1")
	subB := broker.subscribe("run-1")

	if count, ok := subscriberCount("run-1"); assert.True(t, ok) {
		assert.Equal(t, 2, count, "both connections to the same run share one poll loop")
	}

	assert.Equal(t, 1, pollCount(), "a second connection to the same run starts no second poller")

	subC := broker.subscribe("run-2")

	assert.Equal(t, 2, pollCount(), "a different run gets its own poller")

	broker.unsubscribe(subA)

	if count, ok := subscriberCount("run-1"); assert.True(t, ok) {
		assert.Equal(t, 1, count, "one connection leaving keeps the shared poller for the rest")
	}

	broker.unsubscribe(subB)

	_, ok := subscriberCount("run-1")
	assert.False(t, ok, "the last connection leaving tears the run's poller down")

	broker.unsubscribe(subC)
	assert.Equal(t, 0, pollCount(), "no pollers remain once every connection has left")
}
