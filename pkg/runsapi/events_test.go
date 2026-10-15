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
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// setEventPollInterval must be called before constructing the handler, which
// captures the interval; otherwise poll goroutines race the cleanup restore.
func setEventPollInterval(t *testing.T, interval time.Duration) {
	t.Helper()

	original := eventPollInterval
	eventPollInterval = interval

	t.Cleanup(func() { eventPollInterval = original })
}

func setSSEHeartbeatInterval(t *testing.T, interval time.Duration) {
	t.Helper()

	original := sseHeartbeatInterval
	sseHeartbeatInterval = interval

	t.Cleanup(func() { sseHeartbeatInterval = original })
}

// dynamicTemporalClient is a fake whose run state a test can change while a
// stream is connected.
type dynamicTemporalClient struct {
	fakeTemporalClient

	mu            sync.Mutex
	status        enumspb.WorkflowExecutionStatus
	phase         string
	phaseErr      error
	pause         backup.CurrentPause
	pauseErr      error
	historyErr    error
	describeCalls int
}

func (c *dynamicTemporalClient) setState(status enumspb.WorkflowExecutionStatus, phase string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.status = status
	c.phase = phase
}

func (c *dynamicTemporalClient) setPause(pause backup.CurrentPause) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.pause = pause
}

func (c *dynamicTemporalClient) setPauseErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.pauseErr = err
}

func (c *dynamicTemporalClient) setPhaseErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.phaseErr = err
}

// setHistoryErr is needed to simulate an Unknown query result: with history
// readable, fetchRunDetail's fallback (issue #329) would resolve it.
func (c *dynamicTemporalClient) setHistoryErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.historyErr = err
}

func (c *dynamicTemporalClient) GetWorkflowHistory(_ context.Context, _, _ string, _ bool, _ enumspb.HistoryEventFilterType) client.HistoryEventIterator {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.historyErr != nil {
		return &fakeHistoryIterator{err: c.historyErr}
	}

	return &fakeHistoryIterator{}
}

func (c *dynamicTemporalClient) DescribeTaskQueue(context.Context, string, enumspb.TaskQueueType) (*workflowservice.DescribeTaskQueueResponse, error) {
	return &workflowservice.DescribeTaskQueueResponse{}, nil
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

func (c *dynamicTemporalClient) QueryWorkflow(_ context.Context, _, _, queryType string, _ ...interface{}) (converter.EncodedValue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if queryType == backup.CurrentPauseQuery {
		if c.pauseErr != nil {
			return nil, c.pauseErr
		}

		return fakeEncodedValue{value: c.pause}, nil
	}

	if c.phaseErr != nil {
		return nil, c.phaseErr
	}

	return fakeEncodedValue{value: c.phase}, nil
}

type sseFrame struct {
	event string
	data  string
}

// readSSEFrames closes the returned channel when the server closes the stream.
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

func assertFrame(t *testing.T, frame sseFrame, wantEvent string, wantStatus enumspb.WorkflowExecutionStatus, wantPhase string) {
	t.Helper()

	assert.Equal(t, wantEvent, frame.event)

	var detail RunDetail

	require.NoError(t, json.Unmarshal([]byte(frame.data), &detail))
	assert.Equal(t, wantStatus.String(), detail.Status)
	assert.Equal(t, wantPhase, detail.LastCompletedPhase)
}

func TestCurrentPauseEqual(t *testing.T) {
	base := CurrentPauseInfo{
		Kind:          "write-failure",
		Phase:         "Write",
		AffectedTapes: []string{"TA0001L6"},
		ReloadSlots:   []int{1},
	}

	t.Run("nil and empty slices are the same state (no spurious update)", func(t *testing.T) {
		// CurrentPauseQuery may return either for "nothing affected".
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

// TestStreamRunEventsHeartbeatKeepsIdleStreamAlive reads the raw stream because
// comment lines are not events.
func TestStreamRunEventsHeartbeatKeepsIdleStreamAlive(t *testing.T) {
	setEventPollInterval(t, time.Hour) // no state-change frames during the test
	setSSEHeartbeatInterval(t, 15*time.Millisecond)

	client := &dynamicTemporalClient{}
	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "")

	server := httptest.NewServer(New(client))
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/events/runs/run-1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)

	lines := make(chan string, 64)

	go func() {
		reader := bufio.NewReader(resp.Body)

		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				lines <- line
			}

			if err != nil {
				return
			}
		}
	}()

	deadline := time.After(2 * time.Second)

	for {
		select {
		case line := <-lines:
			if strings.HasPrefix(line, ":") {
				return // a keepalive comment arrived on the idle stream
			}
		case <-deadline:
			t.Fatal("no SSE heartbeat comment arrived on an idle running stream")
		}
	}
}

// TestStreamRunEventsInitialError requires a JSON error status, never a 200
// event stream, when the first poll fails.
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

	requireNoFrame(t, frames, 150*time.Millisecond)

	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Verify")

	second := waitForFrame(t, frames, 2*time.Second)
	assertFrame(t, second, sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Verify")

	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, "Verify")

	third := waitForFrame(t, frames, 2*time.Second)
	assertFrame(t, third, sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, "Verify")

	fourth := waitForFrame(t, frames, 2*time.Second)
	assertFrame(t, fourth, sseEventDone, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, "Verify")

	select {
	case _, ok := <-frames:
		assert.False(t, ok, "expected no further frames after the done event")
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close after the terminal done event")
	}
}

// TestStreamRunEventsPushesOnPauseChangeAlone covers pauses, which start and
// clear mid-phase without Status or LastCompletedPhase changing.
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

// TestStreamRunEventsPauseQueryFailureDoesNotFabricateHealthy: a transient pause
// query failure must not make a paused run look cleared (PR #252).
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

	// Blip the pause query, then force an event via a phase change.
	client.setPauseErr(assertError{"no worker polling"})
	// Fail history too, or the #329 fallback would resolve the pause.
	client.setHistoryErr(assertError{"history unavailable"})
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

	client.setPauseErr(nil)
	client.setHistoryErr(nil)
	client.setPause(backup.CurrentPause{})

	third := waitForFrame(t, frames, 2*time.Second)

	var recovered RunDetail
	require.NoError(t, json.Unmarshal([]byte(third.data), &recovered))
	assert.Equal(t, "", recovered.CurrentPause.Kind, "once the query recovers, a genuine pause-clear is still detected")
}

// TestStreamRunEventsPhaseQueryFailureDoesNotRegressToEmpty is the phase-side
// analogue of the pause test above (issue #323).
func TestStreamRunEventsPhaseQueryFailureDoesNotRegressToEmpty(t *testing.T) {
	setEventPollInterval(t, 15*time.Millisecond)

	client := &dynamicTemporalClient{}
	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Generate PAR2")

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
	require.Equal(t, "Generate PAR2", initial.LastCompletedPhase, "test setup: the stream must start out reporting the real phase")
	require.False(t, initial.LastCompletedPhaseUnknown)

	// Blip the phase query, then force an event via a pause.
	client.setPhaseErr(assertError{"no poller seen for task queue recently, worker may be down"})
	// Fail history too, or the #329 fallback would resolve the phase.
	client.setHistoryErr(assertError{"history unavailable"})
	client.setPause(backup.CurrentPause{Kind: backup.PauseEject, AwaitingExport: 1})

	second := waitForFrame(t, frames, 2*time.Second)
	assert.Equal(t, sseEventUpdate, second.event)

	var duringBlip RunDetail
	require.NoError(t, json.Unmarshal([]byte(second.data), &duringBlip))
	assert.Equal(t, "eject", duringBlip.CurrentPause.Kind, "the pause change that triggered this event must still be visible")
	assert.Equal(t, "Generate PAR2", duringBlip.LastCompletedPhase,
		"a transient LastCompletedPhaseQuery failure must carry the last known phase forward, never regress to empty")
	assert.False(t, duringBlip.LastCompletedPhaseUnknown,
		"the carried-forward phase is the last known GOOD state, not an unknown one, once it's been folded into an emitted event")

	client.setPhaseErr(nil)
	client.setHistoryErr(nil)
	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Verify")

	third := waitForFrame(t, frames, 2*time.Second)

	var recovered RunDetail
	require.NoError(t, json.Unmarshal([]byte(third.data), &recovered))
	assert.Equal(t, "Verify", recovered.LastCompletedPhase, "once the query recovers, a genuine phase advance is still detected")
	assert.False(t, recovered.LastCompletedPhaseUnknown)
}

// TestStreamRunEventsInitialPhaseQueryFailureReportsUnknown: with nothing to
// carry forward, the first poll must report Unknown, not an empty phase
// (issue #323).
func TestStreamRunEventsInitialPhaseQueryFailureReportsUnknown(t *testing.T) {
	setEventPollInterval(t, 15*time.Millisecond)

	client := &dynamicTemporalClient{}
	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Generate PAR2")
	client.setPhaseErr(assertError{"no poller seen for task queue recently, worker may be down"})
	client.setHistoryErr(assertError{"history unavailable"})

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
	assert.Equal(t, "", initial.LastCompletedPhase, "no last-known phase exists on the very first poll")
	assert.True(t, initial.LastCompletedPhaseUnknown,
		"an initial phase-query failure must report unknown, not a silent empty phase, so the UI shows 'unavailable' not 'not started'")
}

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

	require.NoError(t, resp.Body.Close())

	require.Eventually(t, func() bool {
		before := client.callCount()

		time.Sleep(100 * time.Millisecond)

		return client.callCount() == before
	}, 3*time.Second, 50*time.Millisecond, "server never stopped polling after the client disconnected")
}

// TestStreamRunEventsSurvivesServerWriteTimeout: Server.WriteTimeout is an
// absolute deadline, so a stream outliving it proves the per-write override.
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

// TestStreamRunEventsClosesOnDrain: open streams must not hold graceful
// shutdown open (issue #270).
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

	first := waitForFrame(t, frames, 2*time.Second)
	assertFrame(t, first, sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "")

	startDrain()

	select {
	case frame, ok := <-frames:
		assert.False(t, ok, "expected the stream to close without further frames on drain, got %+v", frame)
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close after the drain context was cancelled")
	}
}

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

	// Waiting for both snapshots ensures both are subscribed before the change.
	assertFrame(t, waitForFrame(t, framesA, 2*time.Second), sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Load")
	assertFrame(t, waitForFrame(t, framesB, 2*time.Second), sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Load")

	client.setState(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Verify")

	assertFrame(t, waitForFrame(t, framesA, 2*time.Second), sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Verify")
	assertFrame(t, waitForFrame(t, framesB, 2*time.Second), sseEventUpdate, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, "Verify")
}

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
