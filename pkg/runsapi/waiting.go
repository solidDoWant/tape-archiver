package runsapi

import (
	"context"
	"log/slog"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// workerWaitGrace is how long an activity may sit scheduled-but-unstarted
// before it is reported as stalled. It must comfortably absorb a KEDA worker
// cold start (~28s observed, issue #344).
const workerWaitGrace = 60 * time.Second

// taskQueueDescribeTimeout bounds the best-effort DescribeTaskQueue lookup so a
// slow call cannot hold up the phase timeline.
const taskQueueDescribeTimeout = 3 * time.Second

// PhaseWaiting describes a phase whose activity has been scheduled but not yet
// picked up by any worker.
type PhaseWaiting struct {
	// Activity is the waiting activity's type name, e.g. "Load".
	Activity string `json:"activity"`
	// TaskQueue is the task queue the activity was scheduled on, e.g. "data".
	TaskQueue string `json:"taskQueue"`
	// Since is when the current attempt was scheduled.
	Since time.Time `json:"since"`
	// Stalled is true once the activity has waited longer than a worker cold
	// start can explain (60s). Clients should only flag the wait once it is set.
	Stalled bool `json:"stalled"`
	// Pollers is how many workers Temporal reports polling TaskQueue for
	// activities. Omitted when that lookup failed.
	Pollers *int `json:"pollers,omitempty"`
}

// waitingByPhase returns, per phase, the longest-waiting activity that a worker
// has not yet picked up. pending is DescribeWorkflowExecution's pending-activity
// list, the only reliable "has a worker started this" source: history records
// ACTIVITY_TASK_STARTED only once a retried attempt closes. Activities in retry
// backoff or paused are not waiting on a worker and are skipped.
func waitingByPhase(history runHistory, pending []*workflowpb.PendingActivityInfo, now time.Time) map[string]*PhaseWaiting {
	if history.Closed || len(pending) == 0 {
		return nil
	}

	byActivityID := make(map[string]activityRecord, len(history.Activities))

	for _, record := range history.Activities {
		if record.ActivityID != "" && !record.Terminal() {
			byActivityID[record.ActivityID] = record
		}
	}

	waits := make(map[string]*PhaseWaiting)

	for _, activity := range pending {
		if activity.GetState() != enumspb.PENDING_ACTIVITY_STATE_SCHEDULED ||
			activity.GetPaused() || activity.GetNextAttemptScheduleTime() != nil {
			continue
		}

		record, ok := byActivityID[activity.GetActivityId()]
		if !ok || record.Started {
			continue
		}

		phase, ok := phaseForActivity(record.Name, record.Input)
		if !ok {
			continue
		}

		since := record.ScheduledTime
		if activity.GetScheduledTime() != nil {
			since = activity.GetScheduledTime().AsTime()
		}

		if since.After(now) {
			continue
		}

		taskQueue := record.TaskQueue
		if taskQueue == "" {
			taskQueue = activity.GetActivityOptions().GetTaskQueue().GetName()
		}

		if existing := waits[phase]; existing != nil && !since.Before(existing.Since) {
			continue
		}

		waits[phase] = &PhaseWaiting{
			Activity:  record.Name,
			TaskQueue: taskQueue,
			Since:     since,
			Stalled:   now.Sub(since) >= workerWaitGrace,
		}
	}

	return waits
}

// annotateWaiting sets PhaseInfo.Waiting on an open run's timeline, with each
// waiting queue's poller count. It is best-effort: any Temporal failure leaves
// the timeline as history alone describes it.
func annotateWaiting(ctx context.Context, temporalClient TemporalClient, runID string, history runHistory, timeline []PhaseInfo, now time.Time) {
	if history.Closed {
		return
	}

	description, err := temporalClient.DescribeWorkflowExecution(ctx, backup.WorkflowID, runID)
	if err != nil {
		slog.WarnContext(ctx, "runsapi: describe for pending activities failed", "run_id", runID, "error", err)

		return
	}

	waits := waitingByPhase(history, description.GetPendingActivities(), now)
	if len(waits) == 0 {
		return
	}

	pollers := make(map[string]*int)

	for i := range timeline {
		wait, ok := waits[timeline[i].Name]
		if !ok || timeline[i].Status != PhaseActive {
			continue
		}

		if wait.TaskQueue != "" {
			count, looked := pollers[wait.TaskQueue]
			if !looked {
				count = countActivityPollers(ctx, temporalClient, wait.TaskQueue)
				pollers[wait.TaskQueue] = count
			}

			wait.Pollers = count
		}

		timeline[i].Waiting = wait
	}
}

// countActivityPollers returns how many workers are polling taskQueue for
// activities, or nil when Temporal could not say.
func countActivityPollers(ctx context.Context, temporalClient TemporalClient, taskQueue string) *int {
	ctx, cancel := context.WithTimeout(ctx, taskQueueDescribeTimeout)
	defer cancel()

	response, err := temporalClient.DescribeTaskQueue(ctx, taskQueue, enumspb.TASK_QUEUE_TYPE_ACTIVITY)
	if err != nil {
		slog.WarnContext(ctx, "runsapi: describe task queue failed", "task_queue", taskQueue, "error", err)

		return nil
	}

	count := len(response.GetPollers())

	return &count
}
