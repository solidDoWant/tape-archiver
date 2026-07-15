// This file implements the shared machinery behind GET /api/runs/{runID}/phases,
// GET /api/runs/{runID}/config, GET /api/runs/{runID}/tapes, and GET /api/tapes
// (issue #273): everything is reconstructed on demand by walking a run's raw
// Temporal workflow event history — never from a persisted catalog (SPEC §4.2's
// "no cross-run state, no online catalog").
//
// This deliberately reads raw HistoryEvents (GetWorkflowHistory) rather than a
// replay-based Temporal query: a query against a *closed* workflow execution is
// answered by replaying its history against the querying worker's *currently
// registered* workflow code, and this repo's history can span multiple deployed
// versions of workflows/backup (feature work, bug fixes) as well as, in tests,
// entirely foreign/stub workflows sharing the same fixed WorkflowID/TaskQueue.
// Replaying an old run's history against newer code — or a stub's history
// against the real Backup workflow code — is exactly the nondeterminism replay
// panics guard against. Parsing the immutable event stream directly has no such
// hazard: it only ever reads what already happened, never re-executes workflow
// code.
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
	"go.temporal.io/sdk/converter"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// phaseOrder is the 11 pipeline phases in execution order (SPEC §4.3), the
// authoritative source being workflows/backup/workflow.go's backupPhases()
// (with its single Load/Write/Eject table entry expanded to the three phase
// names it completes, matching LastCompletedPhaseQuery's granularity).
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

// activityRecord is one activity invocation reconstructed from a run's history:
// the ACTIVITY_TASK_SCHEDULED event that created it, joined with whatever
// terminal event (COMPLETED/FAILED/TIMED_OUT/CANCELED) followed, if any — none
// of Completed/Failed/TimedOut/Canceled is true while the activity is still in
// flight (or the run ended before it finished).
type activityRecord struct {
	ScheduledEventID int64
	Name             string
	ScheduledTime    time.Time
	Input            *commonpb.Payloads

	Completed bool
	Failed    bool
	TimedOut  bool
	Canceled  bool
	EndTime   time.Time
	Result    *commonpb.Payloads
	ErrorText string

	// Barcode is the tape barcode this activity's input names, pre-extracted
	// at parse time for the write-pipeline activities (FormatTape/WriteTree/
	// FinalizeTape/MeasureWriteHealth) so deriveTapeOutcomes can correlate
	// them by barcode without re-decoding every Input by hand. Empty for
	// activities with no barcode field, or when decoding failed.
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

// runHistory is a run's raw workflow history, parsed once by fetchRunHistory
// into the shapes the phase-timeline, config, and tape-outcome derivations all
// share.
type runHistory struct {
	// StartInput is WorkflowExecutionStarted's Input: the single config.Config
	// argument runsubmit.Submit passed to ExecuteWorkflow, i.e. the exact run
	// configuration originally submitted for this run.
	StartInput *commonpb.Payloads
	StartTime  time.Time
	// StartMemo is WorkflowExecutionStarted's Memo: the submit-time metadata
	// runsubmit.Submit attached (currently the dry-run flag, MemoKeyDryRun).
	// Read back so a restart preload can faithfully carry the dry-run intent,
	// which the config Input alone does not record.
	StartMemo *commonpb.Memo

	// Activities are every activity this run ever scheduled, in schedule
	// order (append order during the history walk, which is monotonic in
	// ScheduledEventID since history events are strictly ordered).
	Activities []activityRecord

	// Closed/Succeeded describe the workflow's terminal state, both zero
	// values while the run is still open (RUNNING, or paused — a pause is not
	// a terminal Temporal status).
	Closed    bool
	Succeeded bool

	// FailureMessage is the terminal failure's rendered text, when Closed &&
	// !Succeeded. Used only as populateFailingPhase's fallback signal.
	FailureMessage string

	// FailingPhase/FailingSummary identify which of the 11 phases (PhaseHold
	// folded onto PhaseResolve, see normalizeFailingPhase) failed and why,
	// primarily extracted from the NotifyFailure activity's own recorded
	// input (failure.go's FailureInput) — the same structured phase name and
	// error summary the SPEC §11 Discord alert carries — rather than by
	// pattern-matching the workflow's own terminal failure message. Empty
	// when the run has not failed, or failed before any phase (or Hold) had
	// a chance to run (e.g. run-config validation, workflow.go's very first
	// statement).
	FailingPhase   string
	FailingSummary string
}

// fetchRunHistory drains runID's complete event history via
// TemporalClient.GetWorkflowHistory and parses it into a runHistory. Every
// error GetWorkflowHistory's iterator can return (serviceerror.NotFound,
// InvalidArgument, ...) is returned unwrapped so writeHistoryError's
// classification (statusForTemporalError plus the aged-out/never-existed
// distinction) applies uniformly to every caller.
func fetchRunHistory(ctx context.Context, temporalClient TemporalClient, runID string) (runHistory, error) {
	iterator := temporalClient.GetWorkflowHistory(ctx, backup.WorkflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)

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

// applyHistoryEvent folds one history event into history, tracking each
// activity's ScheduledEventID -> index in history.Activities via
// indexByScheduled so later COMPLETED/FAILED/TIMED_OUT/CANCELED events can
// find and update the record its SCHEDULED event created. indexByScheduled
// (rather than a pointer captured at append time) is required because
// appending to history.Activities can reallocate its backing array.
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
			Name:             name,
			ScheduledTime:    eventTime,
			Input:            input,
			Barcode:          extractBarcode(name, input),
		})
		indexByScheduled[event.GetEventId()] = len(history.Activities) - 1

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

	case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_FAILED:
		history.Closed = true
		history.FailureMessage = event.GetWorkflowExecutionFailedEventAttributes().GetFailure().GetMessage()

	case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TIMED_OUT:
		history.Closed = true
		history.FailureMessage = "workflow execution timed out"

	case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TERMINATED:
		history.Closed = true
		history.FailureMessage = "workflow execution terminated"

	case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CANCELED:
		history.Closed = true
		history.FailureMessage = "workflow execution canceled"
	}
}

// extractBarcode decodes the tape barcode out of a write-pipeline activity's
// scheduled Input (FormatInput/WriteTreeInput/FinalizeInput/
// MeasureWriteHealthInput, session.go/writehealth.go — none of which are
// exported as a shared interface, hence the anonymous decode target below,
// matched by field name against whichever of the four this actually is).
// Returns "" for any other activity name, or when decoding fails (e.g. an
// older workflow code version whose input shape does not carry a Barcode
// field at all — degrade to an unattributed record rather than fail the
// whole history walk).
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

// decodePayloads decodes payloads into target using Temporal's default data
// converter — the same one client.Dial uses absent a custom
// client.Options.DataConverter (pkg/temporalclient.buildOptions sets none), so
// this always matches how the value was actually encoded on the wire.
func decodePayloads(payloads *commonpb.Payloads, target interface{}) error {
	if payloads == nil {
		return errors.New("no payload")
	}

	return converter.GetDefaultDataConverter().FromPayloads(payloads, target)
}

// normalizeFailingPhase maps PhaseHold (workflow.go) onto PhaseResolve for the
// phase timeline. PhaseHold is the run-scoped zfs hold placed immediately after
// Resolve produces its work list (SPEC §4.3 phase 1) — a continuation of
// finishing Resolve's job, not one of the 11 pipeline phases this API reports
// (the issue's phase list omits it, matching backupPhases()). A design
// decision (documented on issue #273): rather than inventing a 12th phase or
// silently dropping a Hold failure from the timeline, it surfaces as a Resolve
// failure — the FailingSummary text ("phase Hold: ...") still names the real
// failing step precisely.
func normalizeFailingPhase(phase string) string {
	if phase == backup.PhaseHold {
		return backup.PhaseResolve
	}

	return phase
}

// notifyFailureActivityName is the Temporal activity type name for
// (*backup.FailureActivities).NotifyFailure — the deferred failure-alert
// activity every failed run schedules exactly once (workflow.go), carrying
// the failing phase and error summary as its own recorded Input
// (failure.go's FailureInput).
const notifyFailureActivityName = "NotifyFailure"

// populateFailingPhase fills history.FailingPhase/FailingSummary. It prefers
// decoding the NotifyFailure activity's own Input (FailureInput{Phase,
// ErrorSummary}), the most direct, structured record of why a run failed
// (SPEC §11's Discord alert reads the exact same fields) — present regardless
// of whether DISCORD_FAILURE_WEBHOOK_URL is configured, since the activity
// itself always runs (failure.go: an empty URL only makes the webhook client
// a no-op, not the activity).
//
// Falls back to parsing the workflow-level terminal failure message when no
// NotifyFailure activity is found in history at all: an older deployed
// version of workflows/backup predating FailureInput's Phase field (this
// API's history can span multiple code versions, issue #273), or a run whose
// NotifyFailure activity itself never got scheduled (e.g. cfg.Validate()
// rejected the config before the failure-alert defer was even installed,
// workflow.go's first statement). workflow.go always wraps a phase failure as
// `fmt.Errorf("phase %s: %w", name, err)`, so the fallback recovers the same
// phase name from that prefix.
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
	history.FailingSummary = history.FailureMessage
}

// runExistsInVisibility reports whether runID appears in Temporal visibility
// as an execution of the singleton backup workflow — the same ListWorkflow
// query listRuns (runsapi.go) already issues for GET /api/runs, reused here
// (not a new catalog, SPEC §4.2) to distinguish a run that once existed from
// one that never did (writeHistoryError).
func runExistsInVisibility(ctx context.Context, temporalClient TemporalClient, runID string) (bool, error) {
	response, err := temporalClient.ListWorkflow(ctx, &workflowservicepb.ListWorkflowExecutionsRequest{
		Query:    workflowIDQuery(),
		PageSize: listPageSize,
	})
	if err != nil {
		return false, err
	}

	for _, execution := range response.GetExecutions() {
		if execution.GetExecution().GetRunId() == runID {
			return true, nil
		}
	}

	return false, nil
}

// workflowIDQuery is the visibility query every listing/existence check in
// this package issues: every execution of the singleton backup workflow ID
// (listRuns in runsapi.go builds the identical query inline; this named
// helper exists so listTapes and runExistsInVisibility, added by issue #273,
// use the exact same query text rather than two independently hand-written
// copies).
func workflowIDQuery() string {
	return fmt.Sprintf("WorkflowId = %q", backup.WorkflowID)
}

// intFact renders an integer fact as a PhaseFact with the value formatted in
// base 10 — shared by every phaseFacts implementation in facts.go.
func intFact(key, label string, n int) PhaseFact {
	return PhaseFact{Key: key, Label: label, Value: strconv.Itoa(n)}
}

// sortByScheduleOrder sorts records by ScheduledEventID ascending — the order
// they actually happened in, since history events are strictly ordered by
// EventId.
func sortByScheduleOrder(records []activityRecord) {
	sort.Slice(records, func(i, j int) bool { return records[i].ScheduledEventID < records[j].ScheduledEventID })
}
