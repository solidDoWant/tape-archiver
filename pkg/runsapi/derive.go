// This file reconstructs a run's *current* operator-pause and last-completed
// phase from raw Temporal workflow history, the fallback GET /api/runs/{runID}
// and the resume/abort handlers use when the live workflow queries
// (backup.CurrentPauseQuery / backup.LastCompletedPhaseQuery) cannot be
// answered because no control worker is polling the task queue.
//
// That "no poller" case is not exotic: the control worker is a KEDA ScaledJob
// that scales on the control task queue's *workflow-task backlog*, and a run
// paused awaiting an operator is blocked on a signal with zero pending workflow
// tasks — so KEDA scales the worker to zero for the whole (often long) idle
// wait. A Temporal query needs a live worker to replay against, so every
// CurrentPauseQuery then fails and the run's overview renders "Pause status
// unavailable" precisely while the pause needs operator action (issue #329).
//
// History, unlike a query, is served by the Temporal frontend directly and
// needs no worker. Like the #273 history endpoints (see history.go's doc
// comment) this reads the immutable event stream rather than replaying workflow
// code, so it is robust across the multiple deployed workflow versions a run's
// history can span.
package runsapi

import (
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// Pause-alert activity type names: the best-effort operator-alert activity each
// pause site schedules immediately before it blocks on the operator
// (writepause.go's notifyWritePathPause, library.go's notifyOperatorPause,
// burnpause.go's notifyBurnPause). The alert always schedules — "best-effort"
// only swallows the webhook send's error, the activity itself always runs — so
// every pause records exactly one of these in history, carrying the same detail
// in its Input that the live CurrentPause exposes.
const (
	notifyWritePathPauseActivity = "NotifyWritePathPause"
	notifyOperatorPauseActivity  = "NotifyOperatorPause"
	notifyBurnPauseActivity      = "NotifyBurnPause"
)

// isPauseAlert reports whether name is one of the three operator-pause alert
// activities.
func isPauseAlert(name string) bool {
	switch name {
	case notifyWritePathPauseActivity, notifyOperatorPauseActivity, notifyBurnPauseActivity:
		return true
	default:
		return false
	}
}

// derivePause reconstructs a run's currently-active operator pause from its raw
// workflow history, returning PauseNone when the run is not paused right now. It
// is the history-derived stand-in for backup.CurrentPauseQuery when no worker
// can answer the live query (issue #329).
//
// A pause is active iff the run is still open AND the most recently scheduled
// activity is a pause alert. Each pause site schedules its alert immediately
// before blocking on the operator, and every exit from that wait either
// schedules the next real activity — an operator resume and the Eject
// auto-resume both continue the pipeline (loadPhase / runEject / a burn retry) —
// or closes the workflow (abort and timeout fail it). So a pause alert sitting
// as the last activity of an open run is, unambiguously, the pause the run is
// blocked on now; any activity scheduled after it, or a closed workflow, means
// that pause has already resolved.
//
// This deliberately never inspects WorkflowExecutionSignaled events. The
// stale-signal drain (writepause.go's drainStalePauseSignals) consumes buffered
// resume/abort signals with a deterministic ReceiveAsync loop that records no
// history event, so signal events in history do not reliably indicate a pause
// resolved — "did a real activity follow the alert" does, and it is immune to
// buffered or drained signals in either direction.
//
// During the brief window between an operator sending resume and a worker
// actually replaying it (KEDA still spawning the worker), no follow-on activity
// exists yet, so this still reports the pause active — which is correct: the run
// is paused until the resume is processed.
func derivePause(history runHistory) backup.CurrentPause {
	if history.Closed {
		return backup.CurrentPause{}
	}

	lastAlert := -1

	for i, record := range history.Activities {
		if isPauseAlert(record.Name) {
			lastAlert = i
		}
	}

	// No pause alert, or a real activity was scheduled after the last one (the
	// run moved past that pause): not paused.
	if lastAlert == -1 || lastAlert != len(history.Activities)-1 {
		return backup.CurrentPause{}
	}

	return pauseFromAlert(history.Activities[lastAlert])
}

// pauseFromAlert rebuilds the CurrentPause the workflow set for a pause from its
// alert activity's recorded Input — the same fields the live query would report
// (the pause sites build state.currentPause from these very values). A decode
// failure (an older workflow version whose alert Input shape predates a field)
// degrades to the pause Kind alone rather than dropping the pause: the operator
// still learns the run is paused and which kind, just with less detail, matching
// history.go's decode-failure-degrades-not-fails convention.
func pauseFromAlert(record activityRecord) backup.CurrentPause {
	switch record.Name {
	case notifyWritePathPauseActivity:
		var input backup.WritePathPauseInput
		if err := decodePayloads(record.Input, &input); err != nil {
			return backup.CurrentPause{Kind: backup.PauseWriteFailure}
		}

		return backup.CurrentPause{
			Kind:          backup.PauseWriteFailure,
			Phase:         input.Phase,
			AffectedTapes: input.AffectedTapes,
			ReloadSlots:   input.ReloadSlots,
			ErrorSummary:  input.ErrorSummary,
		}

	case notifyOperatorPauseActivity:
		var input backup.OperatorPauseInput
		if err := decodePayloads(record.Input, &input); err != nil {
			return backup.CurrentPause{Kind: backup.PauseEject}
		}

		return backup.CurrentPause{
			Kind:           backup.PauseEject,
			AffectedTapes:  input.ReadyForRemoval,
			AwaitingExport: input.Awaiting,
		}

	case notifyBurnPauseActivity:
		var input backup.BurnPauseInput
		if err := decodePayloads(record.Input, &input); err != nil {
			return backup.CurrentPause{Kind: backup.PauseBurn}
		}

		return backup.CurrentPause{
			Kind:         backup.PauseBurn,
			Devices:      input.Devices,
			ErrorSummary: input.ErrorSummary,
		}

	default:
		return backup.CurrentPause{}
	}
}

// deriveLastCompletedPhase reconstructs backup.LastCompletedPhaseQuery's result
// — the name of the most recently completed pipeline phase — from history, the
// phase-side fallback for GET /api/runs/{runID} when no worker can answer the
// live query (issue #329, the analogue of derivePause). It reuses the phase
// timeline (buildPhaseTimeline) so the value stays consistent with the
// GET /api/runs/{runID}/phases endpoint, and returns the furthest phase that
// reads completed, or "" when none has completed yet.
func deriveLastCompletedPhase(history runHistory) string {
	timeline := buildPhaseTimeline(history, deriveTapeOutcomes(history.Activities))

	last := ""

	for _, phase := range timeline {
		if phase.Status == PhaseCompleted {
			last = phase.Name
		}
	}

	return last
}
