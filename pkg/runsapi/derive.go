// History-derived fallbacks for CurrentPauseQuery / LastCompletedPhaseQuery.
// A paused run has no workflow-task backlog, so KEDA scales the control worker
// to zero and live queries fail exactly while the pause needs an operator
// (issue #329). History needs no worker.
package runsapi

import (
	"context"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// Pause-alert activity names. Every pause site always schedules exactly one of
// these right before blocking, carrying the same detail as CurrentPause.
const (
	notifyWritePathPauseActivity = "NotifyWritePathPause"
	notifyOperatorPauseActivity  = "NotifyOperatorPause"
	notifyBurnPauseActivity      = "NotifyBurnPause"
)

func isPauseAlert(name string) bool {
	switch name {
	case notifyWritePathPauseActivity, notifyOperatorPauseActivity, notifyBurnPauseActivity:
		return true
	default:
		return false
	}
}

// derivePause reports a pause as active iff the run is open and its most
// recently scheduled activity is a pause alert: every exit from a pause either
// schedules another activity or closes the workflow.
//
// Signal events are deliberately ignored: drainStalePauseSignals consumes
// buffered signals without recording history, so they are not a reliable
// indicator of resolution.
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

	// A real activity scheduled after the last alert means that pause resolved.
	if lastAlert == -1 || lastAlert != len(history.Activities)-1 {
		return backup.CurrentPause{}
	}

	return pauseFromAlert(history.Activities[lastAlert])
}

// pauseFromAlert decodes the alert's Input. An undecodable Input (older
// workflow version) degrades to the pause Kind alone rather than dropping it.
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

// deriveLastCompletedPhase reuses the phase timeline so it stays consistent
// with GET /api/runs/{runID}/phases.
func deriveLastCompletedPhase(history runHistory) string {
	return lastCompletedPhase(buildPhaseTimeline(history, deriveTapeOutcomes(history.Activities)))
}

func lastCompletedPhase(timeline []PhaseInfo) string {
	last := ""

	for _, phase := range timeline {
		if phase.Status == PhaseCompleted {
			last = phase.Name
		}
	}

	return last
}

// RunPhases is a run's phase position derived from its workflow history.
type RunPhases struct {
	// LastCompleted is the last completed phase; empty if none has completed.
	LastCompleted string
	// Active are the in-progress phases in pipeline order; empty once the run
	// has closed. The interleaved tape path can hold more than one.
	Active []PhaseInfo
}

// DeriveRunPhases reads runID's history and derives the same phase position
// the web UI shows, for callers that cannot rely on live queries (issue #430).
func DeriveRunPhases(ctx context.Context, historyReader HistoryReader, runID string) (RunPhases, error) {
	history, err := fetchRunHistory(ctx, historyReader, runID)
	if err != nil {
		return RunPhases{}, err
	}

	timeline := buildPhaseTimeline(history, deriveTapeOutcomes(history.Activities))

	phases := RunPhases{LastCompleted: lastCompletedPhase(timeline)}

	for _, phase := range timeline {
		if phase.Status == PhaseActive {
			phases.Active = append(phases.Active, phase)
		}
	}

	return phases, nil
}
