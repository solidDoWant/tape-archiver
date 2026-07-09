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
	describeCalls int
}

func (c *dynamicTemporalClient) setState(status enumspb.WorkflowExecutionStatus, phase string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.status = status
	c.phase = phase
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

func (c *dynamicTemporalClient) QueryWorkflow(context.Context, string, string, string, ...interface{}) (converter.EncodedValue, error) {
	c.mu.Lock()
	phase := c.phase
	c.mu.Unlock()

	return fakeEncodedValue{value: phase}, nil
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
