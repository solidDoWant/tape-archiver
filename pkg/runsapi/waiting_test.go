package runsapi

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// resumedLoadHistory is issue #344's incident: a write-path pause on Load that
// the operator resumed, after which Load was scheduled on the "data" queue and
// never started. It returns the events and the waiting Load's activity ID.
func resumedLoadHistory(t *testing.T) ([]*historypb.HistoryEvent, string) {
	t.Helper()

	builder := newEventBuilder()
	builder.started(t, testConfig)

	verifyID := builder.scheduled(t, "Verify", struct{}{})
	builder.completed(t, verifyID, struct{}{})

	firstLoadID := builder.scheduled(t, "Load", struct{}{})
	builder.failed(firstLoadID, "load failed")

	alertID := builder.scheduled(t, notifyWritePathPauseActivity, backup.WritePathPauseInput{Phase: backup.PhaseLoad})
	builder.completed(t, alertID, struct{}{})

	loadID := builder.scheduled(t, "Load", struct{}{})
	builder.events[len(builder.events)-1].GetActivityTaskScheduledEventAttributes().TaskQueue = &taskqueuepb.TaskQueue{Name: "data"}

	return builder.events, strconv.FormatInt(loadID, 10)
}

func mustParseID(t *testing.T, id string) int64 {
	t.Helper()

	parsed, err := strconv.ParseInt(id, 10, 64)
	require.NoError(t, err)

	return parsed
}

func pendingActivity(activityID string, state enumspb.PendingActivityState, scheduled time.Time) *workflowpb.PendingActivityInfo {
	return &workflowpb.PendingActivityInfo{
		ActivityId:    activityID,
		State:         state,
		ScheduledTime: timestamppb.New(scheduled),
	}
}

func TestWaitingByPhase(t *testing.T) {
	events, loadID := resumedLoadHistory(t)
	history := historyFromEvents(events)
	scheduled := history.Activities[len(history.Activities)-1].ScheduledTime

	startedEvents, startedLoadID := resumedLoadHistory(t)
	startedEvents = append(startedEvents, &historypb.HistoryEvent{
		EventId:   int64(len(startedEvents) + 1),
		EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED,
		Attributes: &historypb.HistoryEvent_ActivityTaskStartedEventAttributes{
			ActivityTaskStartedEventAttributes: &historypb.ActivityTaskStartedEventAttributes{ScheduledEventId: mustParseID(t, startedLoadID)},
		},
	})

	closedEvents, closedLoadID := resumedLoadHistory(t)
	closedBuilder := &eventBuilder{events: closedEvents, nextID: int64(len(closedEvents) + 1), now: scheduled}
	closedBuilder.runFailed("canceled")

	backoff := pendingActivity(loadID, enumspb.PENDING_ACTIVITY_STATE_SCHEDULED, scheduled)
	backoff.NextAttemptScheduleTime = timestamppb.New(scheduled.Add(time.Minute))

	paused := pendingActivity(loadID, enumspb.PENDING_ACTIVITY_STATE_SCHEDULED, scheduled)
	paused.Paused = true

	tests := []struct {
		name    string
		history runHistory
		pending []*workflowpb.PendingActivityInfo
		now     time.Time
		want    map[string]*PhaseWaiting
	}{
		{
			name:    "an unstarted activity within the cold-start grace is waiting but not stalled",
			history: history,
			pending: []*workflowpb.PendingActivityInfo{pendingActivity(loadID, enumspb.PENDING_ACTIVITY_STATE_SCHEDULED, scheduled)},
			now:     scheduled.Add(28 * time.Second),
			want: map[string]*PhaseWaiting{
				backup.PhaseLoad: {Activity: "Load", TaskQueue: "data", Since: scheduled, Stalled: false},
			},
		},
		{
			name:    "an unstarted activity past the grace is stalled",
			history: history,
			pending: []*workflowpb.PendingActivityInfo{pendingActivity(loadID, enumspb.PENDING_ACTIVITY_STATE_SCHEDULED, scheduled)},
			now:     scheduled.Add(workerWaitGrace),
			want: map[string]*PhaseWaiting{
				backup.PhaseLoad: {Activity: "Load", TaskQueue: "data", Since: scheduled, Stalled: true},
			},
		},
		{
			name:    "an activity a worker has started is not waiting",
			history: history,
			pending: []*workflowpb.PendingActivityInfo{pendingActivity(loadID, enumspb.PENDING_ACTIVITY_STATE_STARTED, scheduled)},
			now:     scheduled.Add(time.Hour),
		},
		{
			name:    "an activity history records as started is not waiting",
			history: historyFromEvents(startedEvents),
			pending: []*workflowpb.PendingActivityInfo{pendingActivity(startedLoadID, enumspb.PENDING_ACTIVITY_STATE_SCHEDULED, scheduled)},
			now:     scheduled.Add(time.Hour),
		},
		{
			name:    "an activity in retry backoff is not waiting on a worker",
			history: history,
			pending: []*workflowpb.PendingActivityInfo{backoff},
			now:     scheduled.Add(time.Hour),
		},
		{
			name:    "a paused activity is not waiting on a worker",
			history: history,
			pending: []*workflowpb.PendingActivityInfo{paused},
			now:     scheduled.Add(time.Hour),
		},
		{
			name:    "a closed run has nothing waiting",
			history: historyFromEvents(closedBuilder.events),
			pending: []*workflowpb.PendingActivityInfo{pendingActivity(closedLoadID, enumspb.PENDING_ACTIVITY_STATE_SCHEDULED, scheduled)},
			now:     scheduled.Add(time.Hour),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := waitingByPhase(test.history, test.pending, test.now)

			if test.want == nil {
				assert.Empty(t, got)

				return
			}

			assert.Equal(t, test.want, got)
		})
	}
}

// TestGetRunPhasesWaitingForWorker covers issue #344 end to end through the
// HTTP API: after a resume, the run reads as no longer paused and its Load
// phase reads as waiting for a "data" worker rather than executing.
func TestGetRunPhasesWaitingForWorker(t *testing.T) {
	zero := 0
	two := 2

	tests := []struct {
		name          string
		since         time.Duration
		state         enumspb.PendingActivityState
		pollers       *workflowservice.DescribeTaskQueueResponse
		pollersErr    error
		wantWaiting   bool
		wantStalled   bool
		wantPollers   *int
		wantQueueCall bool
	}{
		{
			name:          "an activity no worker has picked up for minutes is stalled, with no pollers",
			since:         5 * time.Minute,
			state:         enumspb.PENDING_ACTIVITY_STATE_SCHEDULED,
			pollers:       &workflowservice.DescribeTaskQueueResponse{},
			wantWaiting:   true,
			wantStalled:   true,
			wantPollers:   &zero,
			wantQueueCall: true,
		},
		{
			name:          "a cold start within the grace is waiting but not stalled",
			since:         10 * time.Second,
			state:         enumspb.PENDING_ACTIVITY_STATE_SCHEDULED,
			pollers:       &workflowservice.DescribeTaskQueueResponse{Pollers: []*taskqueuepb.PollerInfo{{Identity: "a"}, {Identity: "b"}}},
			wantWaiting:   true,
			wantPollers:   &two,
			wantQueueCall: true,
		},
		{
			name:          "a failed task-queue lookup still reports the wait, without a poller count",
			since:         5 * time.Minute,
			state:         enumspb.PENDING_ACTIVITY_STATE_SCHEDULED,
			pollersErr:    errors.New("unavailable"),
			wantWaiting:   true,
			wantStalled:   true,
			wantQueueCall: true,
		},
		{
			name:  "a started activity is plain active",
			since: 5 * time.Minute,
			state: enumspb.PENDING_ACTIVITY_STATE_STARTED,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			events, loadID := resumedLoadHistory(t)
			fake := &fakeTemporalClient{
				historyFunc: func(string) client.HistoryEventIterator { return &fakeHistoryIterator{events: events} },
				describeResponse: &workflowservice.DescribeWorkflowExecutionResponse{
					WorkflowExecutionInfo: executionInfo("run-1", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, time.Now().Add(-time.Hour), nil),
					PendingActivities:     []*workflowpb.PendingActivityInfo{pendingActivity(loadID, test.state, time.Now().Add(-test.since))},
				},
				// No control worker to answer the live queries: the run detail
				// falls back to history, as in the incident.
				queryErr:          errors.New("no poller"),
				currentPauseErr:   errors.New("no poller"),
				taskQueueResponse: test.pollers,
				taskQueueErr:      test.pollersErr,
			}
			handler := newMux(newHandler(fake, emptyEnv))

			var detail RunDetail

			require.Equal(t, http.StatusOK, doJSON(t, handler, http.MethodGet, "/api/runs/run-1", &detail).Code)
			assert.Empty(t, detail.CurrentPause.Kind, "the resume must read as having taken effect")
			assert.False(t, detail.CurrentPause.Unknown)

			var body RunPhasesResponse

			require.Equal(t, http.StatusOK, doJSON(t, handler, http.MethodGet, "/api/runs/run-1/phases", &body).Code)

			phases := make(map[string]PhaseInfo, len(body.Phases))
			for _, phase := range body.Phases {
				phases[phase.Name] = phase
			}

			load := phases[backup.PhaseLoad]
			assert.Equal(t, PhaseActive, load.Status)

			if test.wantQueueCall {
				assert.Equal(t, []string{"data"}, fake.taskQueueCalls)
			} else {
				assert.Empty(t, fake.taskQueueCalls)
			}

			if !test.wantWaiting {
				assert.Nil(t, load.Waiting)

				return
			}

			require.NotNil(t, load.Waiting)
			assert.Equal(t, "Load", load.Waiting.Activity)
			assert.Equal(t, "data", load.Waiting.TaskQueue)
			assert.Equal(t, test.wantStalled, load.Waiting.Stalled)
			assert.Equal(t, test.wantPollers, load.Waiting.Pollers)

			for name, phase := range phases {
				if name != backup.PhaseLoad {
					assert.Nil(t, phase.Waiting, "only the waiting activity's phase carries the wait: %s", name)
				}
			}
		})
	}
}

// TestGetRunPhasesDescribeFailure proves the pending-activity lookup is
// best-effort: when it fails the timeline is still served from history.
func TestGetRunPhasesDescribeFailure(t *testing.T) {
	events, _ := resumedLoadHistory(t)
	fake := &fakeTemporalClient{
		historyFunc: func(string) client.HistoryEventIterator { return &fakeHistoryIterator{events: events} },
		describeErr: errors.New("unavailable"),
	}
	handler := newMux(newHandler(fake, emptyEnv))

	var body RunPhasesResponse

	require.Equal(t, http.StatusOK, doJSON(t, handler, http.MethodGet, "/api/runs/run-1/phases", &body).Code)
	require.Len(t, body.Phases, len(phaseOrder))

	for _, phase := range body.Phases {
		assert.Nil(t, phase.Waiting)

		if phase.Name == backup.PhaseLoad {
			assert.Equal(t, PhaseActive, phase.Status)
		}
	}
}
