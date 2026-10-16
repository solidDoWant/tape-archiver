// Run details are reconstructed from a run's raw Temporal event history, never a
// persisted catalog (SPEC §4.2). Raw events are parsed rather than using a
// replay-based query because replaying a closed run against newer (or stub)
// workflow code would hit nondeterminism panics.
package runsapi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	workflowservicepb "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// phaseOrder is the 11 pipeline phases in execution order (SPEC §4.3), mirroring
// backupPhases() with Load/Write/Eject expanded.
var phaseOrder = []string{
	backup.PhaseResolve,
	backup.PhasePrepare,
	backup.PhasePack,
	backup.PhaseGeneratePAR2,
	backup.PhaseVerify,
	backup.PhaseLoad,
	backup.PhaseWrite,
	backup.PhaseEject,
	backup.PhaseReport,
	backup.PhaseBurn,
	backup.PhaseDeliver,
}

// activityRecord is one activity invocation joined from its scheduled, started,
// and terminal history events.
type activityRecord struct {
	ScheduledEventID int64
	ActivityID       string
	Name             string
	TaskQueue        string
	ScheduledTime    time.Time
	Input            *commonpb.Payloads

	// Started is unreliable for in-flight activities: Temporal writes STARTED
	// only when a retried attempt closes. Use pending-activity state instead
	// (waiting.go).
	Started bool

	Completed bool
	Failed    bool
	TimedOut  bool
	Canceled  bool
	EndTime   time.Time
	Result    *commonpb.Payloads
	ErrorText string

	// Barcode is pre-extracted for write-pipeline activities; empty otherwise.
	Barcode string
}

// Terminal reports whether the activity reached any terminal state.
func (a activityRecord) Terminal() bool {
	return a.Completed || a.Failed || a.TimedOut || a.Canceled
}

// Failure reports whether the activity ended in a non-success terminal state.
func (a activityRecord) Failure() bool {
	return a.Failed || a.TimedOut || a.Canceled
}

// runHistory is a run's parsed workflow history.
type runHistory struct {
	// StartInput is the submitted config.Config.
	StartInput *commonpb.Payloads
	StartTime  time.Time
	// StartMemo carries the dry-run flag, which the config Input does not
	// record; restart preloads need it.
	StartMemo *commonpb.Memo

	// Activities are in schedule order.
	Activities []activityRecord

	// Closed/Succeeded are false while the run is open (a pause is not terminal).
	Closed    bool
	Succeeded bool
	// CloseTime bounds drive-metric queries to when the run actually wrote.
	CloseTime time.Time

	// FailureMessage is the terminal failure text; only a fallback signal for
	// populateFailingPhase.
	FailureMessage string

	// FailingPhase/FailingSummary say which phase failed and why, preferably
	// from NotifyFailure's structured input. Empty if the run failed before
	// any phase ran.
	FailingPhase   string
	FailingSummary string
}

// HistoryReader is the slice of the Temporal client needed to read a run's
// history; client.Client satisfies it.
type HistoryReader interface {
	GetWorkflowHistory(ctx context.Context, workflowID string, runID string, isLongPoll bool, filterType enumspb.HistoryEventFilterType) client.HistoryEventIterator
}

// fetchRunHistory reads and parses runID's full history. Errors are returned
// unwrapped so writeHistoryError can classify them.
func fetchRunHistory(ctx context.Context, historyReader HistoryReader, runID string) (runHistory, error) {
	iterator := historyReader.GetWorkflowHistory(ctx, backup.WorkflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)

	var history runHistory

	indexByScheduled := make(map[int64]int)

	for iterator.HasNext() {
		event, err := iterator.Next()
		if err != nil {
			return runHistory{}, err
		}

		applyHistoryEvent(&history, indexByScheduled, event)
	}

	populateFailingPhase(&history)

	return history, nil
}

// applyHistoryEvent folds one event into history. indexByScheduled stores
// indices, not pointers, because appending to Activities can reallocate.
func applyHistoryEvent(history *runHistory, indexByScheduled map[int64]int, event *historypb.HistoryEvent) {
	eventTime := event.GetEventTime().AsTime()

	switch event.GetEventType() {
	case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED:
		attrs := event.GetWorkflowExecutionStartedEventAttributes()
		history.StartInput = attrs.GetInput()
		history.StartMemo = attrs.GetMemo()
		history.StartTime = eventTime

	case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
		attrs := event.GetActivityTaskScheduledEventAttributes()
		name := attrs.GetActivityType().GetName()
		input := attrs.GetInput()

		history.Activities = append(history.Activities, activityRecord{
			ScheduledEventID: event.GetEventId(),
			ActivityID:       attrs.GetActivityId(),
			Name:             name,
			TaskQueue:        attrs.GetTaskQueue().GetName(),
			ScheduledTime:    eventTime,
			Input:            input,
			Barcode:          extractBarcode(name, input),
		})
		indexByScheduled[event.GetEventId()] = len(history.Activities) - 1

	case enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED:
		attrs := event.GetActivityTaskStartedEventAttributes()
		if idx, ok := indexByScheduled[attrs.GetScheduledEventId()]; ok {
			history.Activities[idx].Started = true
		}

	case enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED:
		attrs := event.GetActivityTaskCompletedEventAttributes()
		if idx, ok := indexByScheduled[attrs.GetScheduledEventId()]; ok {
			history.Activities[idx].Completed = true
			history.Activities[idx].EndTime = eventTime
			history.Activities[idx].Result = attrs.GetResult()
		}

	case enumspb.EVENT_TYPE_ACTIVITY_TASK_FAILED:
		attrs := event.GetActivityTaskFailedEventAttributes()
		if idx, ok := indexByScheduled[attrs.GetScheduledEventId()]; ok {
			history.Activities[idx].Failed = true
			history.Activities[idx].EndTime = eventTime
			history.Activities[idx].ErrorText = attrs.GetFailure().GetMessage()
		}

	case enumspb.EVENT_TYPE_ACTIVITY_TASK_TIMED_OUT:
		attrs := event.GetActivityTaskTimedOutEventAttributes()
		if idx, ok := indexByScheduled[attrs.GetScheduledEventId()]; ok {
			history.Activities[idx].TimedOut = true
			history.Activities[idx].EndTime = eventTime
			history.Activities[idx].ErrorText = attrs.GetFailure().GetMessage()
		}

	case enumspb.EVENT_TYPE_ACTIVITY_TASK_CANCELED:
		attrs := event.GetActivityTaskCanceledEventAttributes()
		if idx, ok := indexByScheduled[attrs.GetScheduledEventId()]; ok {
			history.Activities[idx].Canceled = true
			history.Activities[idx].EndTime = eventTime
		}

	case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED:
		history.Closed = true
		history.Succeeded = true
		history.CloseTime = eventTime

	case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_FAILED:
		history.Closed = true
		history.CloseTime = eventTime
		history.FailureMessage = event.GetWorkflowExecutionFailedEventAttributes().GetFailure().GetMessage()

	case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TIMED_OUT:
		history.Closed = true
		history.CloseTime = eventTime
		history.FailureMessage = "workflow execution timed out"

	case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TERMINATED:
		history.Closed = true
		history.CloseTime = eventTime
		history.FailureMessage = "workflow execution terminated"

	case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CANCELED:
		history.Closed = true
		history.CloseTime = eventTime
		history.FailureMessage = "workflow execution canceled"
	}
}

// extractBarcode decodes the Barcode field from a write-pipeline activity's
// input, returning "" for other activities or on decode failure (e.g. older
// workflow versions).
func extractBarcode(name string, input *commonpb.Payloads) string {
	switch name {
	case "FormatTape", "WriteTree", "FinalizeTape", "MeasureWriteHealth":
	default:
		return ""
	}

	var payload struct{ Barcode string }
	if err := decodePayloads(input, &payload); err != nil {
		return ""
	}

	return payload.Barcode
}

// decodePayloads uses the default data converter, which matches the client's
// (pkg/temporalclient sets no custom converter).
func decodePayloads(payloads *commonpb.Payloads, target interface{}) error {
	if payloads == nil {
		return errors.New("no payload")
	}

	return converter.GetDefaultDataConverter().FromPayloads(payloads, target)
}

// normalizeFailingPhase reports a Snapshot or Hold failure as Resolve, since
// neither is one of the 11 reported phases (issue #273); the summary still
// names it.
func normalizeFailingPhase(phase string) string {
	if phase == backup.PhaseHold || phase == backup.PhaseSnapshot {
		return backup.PhaseResolve
	}

	return phase
}

// notifyFailureActivityName is scheduled once by every failed run.
const notifyFailureActivityName = "NotifyFailure"

// populateFailingPhase prefers NotifyFailure's input (which runs even without a
// webhook configured), falling back to the "phase NAME: ..." prefix of the
// terminal failure message for older workflow versions or runs that failed
// before NotifyFailure was deferred.
func populateFailingPhase(history *runHistory) {
	for _, record := range history.Activities {
		if record.Name != notifyFailureActivityName {
			continue
		}

		var input struct {
			Phase        string
			ErrorSummary string
		}

		if err := decodePayloads(record.Input, &input); err != nil {
			continue
		}

		// Older versions lack Phase: keep the summary and recover the phase
		// from the failure message below.
		if input.Phase == "" {
			history.FailingSummary = input.ErrorSummary

			break
		}

		history.FailingPhase = normalizeFailingPhase(input.Phase)
		history.FailingSummary = input.ErrorSummary

		return
	}

	if !history.Closed || history.Succeeded || history.FailureMessage == "" {
		return
	}

	rest, ok := strings.CutPrefix(history.FailureMessage, "phase ")
	if !ok {
		return
	}

	name, _, ok := strings.Cut(rest, ":")
	if !ok {
		return
	}

	history.FailingPhase = normalizeFailingPhase(strings.TrimSpace(name))

	if history.FailingSummary == "" {
		history.FailingSummary = history.FailureMessage
	}
}

// runExistsInVisibility reports whether runID is listed in Temporal visibility,
// distinguishing an aged-out run (410) from one that never existed (404).
func runExistsInVisibility(ctx context.Context, temporalClient TemporalClient, runID string) (bool, error) {
	var (
		token   []byte
		scanned int
		pages   int
	)

	// Scan all pages (a run on page 2+ must still be found), bounded by row and
	// page caps; the page cap covers sparse/empty tokened pages.
	for {
		response, err := temporalClient.ListWorkflow(ctx, &workflowservicepb.ListWorkflowExecutionsRequest{
			Query:         workflowIDQuery(),
			PageSize:      listPageSize,
			NextPageToken: token,
		})
		if err != nil {
			return false, err
		}

		for _, execution := range response.GetExecutions() {
			if execution.GetExecution().GetRunId() == runID {
				return true, nil
			}
		}

		scanned += len(response.GetExecutions())
		pages++

		token = response.GetNextPageToken()
		if len(token) == 0 || scanned >= maxVisibilityScan || pages >= maxVisibilityPages {
			break
		}
	}

	return false, nil
}

// workflowIDQuery matches every execution of the singleton backup workflow.
func workflowIDQuery() string {
	return fmt.Sprintf("WorkflowId = %q", backup.WorkflowID)
}

func intFact(key, label string, n int) PhaseFact {
	return PhaseFact{Key: key, Label: label, Value: strconv.Itoa(n)}
}

func sortByScheduleOrder(records []activityRecord) {
	sort.Slice(records, func(i, j int) bool { return records[i].ScheduledEventID < records[j].ScheduledEventID })
}
