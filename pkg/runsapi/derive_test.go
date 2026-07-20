package runsapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	historypb "go.temporal.io/api/history/v1"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// historyFromEvents parses a synthetic event slice into a runHistory the same
// way fetchRunHistory does (applyHistoryEvent + populateFailingPhase), without a
// fake Temporal client — so the derivation unit tests can drive derivePause /
// deriveLastCompletedPhase against a runHistory directly.
func historyFromEvents(events []*historypb.HistoryEvent) runHistory {
	var history runHistory

	indexByScheduled := make(map[int64]int)

	for _, event := range events {
		applyHistoryEvent(&history, indexByScheduled, event)
	}

	populateFailingPhase(&history)

	return history
}

// pausedRunHistory builds the history of a run that has progressed through a
// completed Load and then blocked on an operator pause, with alertName's alert
// activity (carrying input) as the last thing scheduled and the workflow still
// open — the exact shape a run awaiting the operator has in Temporal history
// while no worker is polling (issue #329). Returned as raw events so it doubles
// as a fakeTemporalClient.historyFunc fixture.
func pausedRunHistory(t *testing.T, alertName string, input interface{}) []*historypb.HistoryEvent {
	t.Helper()

	builder := newEventBuilder()
	builder.started(t, testConfig)

	loadID := builder.scheduled(t, "Load", struct{}{})
	builder.completed(t, loadID, struct{}{})

	alertID := builder.scheduled(t, alertName, input)
	builder.completed(t, alertID, struct{}{})

	return builder.events
}

// TestDerivePause covers reconstructing a run's currently-active operator pause
// from raw history — the fallback GET /api/runs/{runID} and the resume/abort
// handlers use when the live CurrentPauseQuery cannot be answered (issue #329).
func TestDerivePause(t *testing.T) {
	tests := []struct {
		name   string
		events []*historypb.HistoryEvent
		want   backup.CurrentPause
	}{
		{
			name: "a write-failure pause is reconstructed with full detail from its alert input",
			events: pausedRunHistory(t, notifyWritePathPauseActivity, backup.WritePathPauseInput{
				Phase:         backup.PhaseWrite,
				AffectedTapes: []string{"TA0001L6", "TA0002L6"},
				ReloadSlots:   []int{3, 4},
				ErrorSummary:  "drive 0 write failed",
			}),
			want: backup.CurrentPause{
				Kind:          backup.PauseWriteFailure,
				Phase:         backup.PhaseWrite,
				AffectedTapes: []string{"TA0001L6", "TA0002L6"},
				ReloadSlots:   []int{3, 4},
				ErrorSummary:  "drive 0 write failed",
			},
		},
		{
			name: "an eject pause is reconstructed from its alert input",
			events: pausedRunHistory(t, notifyOperatorPauseActivity, backup.OperatorPauseInput{
				ReadyForRemoval: []string{"TA0001L6"},
				Awaiting:        2,
			}),
			want: backup.CurrentPause{
				Kind:           backup.PauseEject,
				AffectedTapes:  []string{"TA0001L6"},
				AwaitingExport: 2,
			},
		},
		{
			name: "a burn pause is reconstructed from its alert input",
			events: pausedRunHistory(t, notifyBurnPauseActivity, backup.BurnPauseInput{
				Devices:      []string{"/dev/sr0"},
				ErrorSummary: "load fresh blank recovery discs for the next set",
			}),
			want: backup.CurrentPause{
				Kind:         backup.PauseBurn,
				Devices:      []string{"/dev/sr0"},
				ErrorSummary: "load fresh blank recovery discs for the next set",
			},
		},
		{
			name: "an alert followed by a resumed activity is not paused",
			events: func() []*historypb.HistoryEvent {
				builder := newEventBuilder()
				builder.started(t, testConfig)
				alertID := builder.scheduled(t, notifyWritePathPauseActivity, backup.WritePathPauseInput{Phase: backup.PhaseWrite})
				builder.completed(t, alertID, struct{}{})
				// The operator resumed: the run re-drove the failed tapes, so a
				// real activity is scheduled after the alert.
				builder.scheduled(t, "Load", struct{}{})

				return builder.events
			}(),
			want: backup.CurrentPause{},
		},
		{
			name: "a closed run is never paused",
			events: func() []*historypb.HistoryEvent {
				builder := newEventBuilder()
				builder.started(t, testConfig)
				alertID := builder.scheduled(t, notifyBurnPauseActivity, backup.BurnPauseInput{Devices: []string{"/dev/sr0"}})
				builder.completed(t, alertID, struct{}{})
				builder.runFailed("phase Burn: operator did not resume")

				return builder.events
			}(),
			want: backup.CurrentPause{},
		},
		{
			name: "a run with no pause alert at all is not paused",
			events: func() []*historypb.HistoryEvent {
				builder := newEventBuilder()
				builder.started(t, testConfig)
				id := builder.scheduled(t, "PrepareArchives", struct{}{})
				builder.completed(t, id, struct{}{})

				return builder.events
			}(),
			want: backup.CurrentPause{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := derivePause(historyFromEvents(test.events))
			assert.Equal(t, test.want, got)
		})
	}
}

// TestDerivePauseDecodeFailureDegradesToKind proves that an alert input this
// version of the code cannot decode (e.g. an older workflow build whose payload
// shape differs) still surfaces the pause Kind — the operator learns the run is
// paused and which kind, rather than the pause vanishing — matching history.go's
// decode-failure-degrades-not-fails convention.
func TestDerivePauseDecodeFailureDegradesToKind(t *testing.T) {
	builder := newEventBuilder()
	builder.started(t, testConfig)
	// Encode a payload with a shape the WritePathPauseInput decode target
	// rejects (a bare string where a struct is expected).
	alertID := builder.scheduled(t, notifyWritePathPauseActivity, "not-a-write-path-pause-input")
	builder.completed(t, alertID, struct{}{})

	got := derivePause(historyFromEvents(builder.events))

	assert.Equal(t, backup.PauseWriteFailure, got.Kind, "the pause kind must survive an undecodable alert input")
	assert.Empty(t, got.AffectedTapes, "details are simply absent when the input cannot be decoded")
}

// TestDeriveLastCompletedPhase covers the phase-side history fallback: it must
// report the furthest completed pipeline phase, consistent with the
// GET /api/runs/{runID}/phases timeline.
func TestDeriveLastCompletedPhase(t *testing.T) {
	t.Run("a write-failure pause reports Load as the last completed phase", func(t *testing.T) {
		events := pausedRunHistory(t, notifyWritePathPauseActivity, backup.WritePathPauseInput{Phase: backup.PhaseWrite})

		assert.Equal(t, backup.PhaseLoad, deriveLastCompletedPhase(historyFromEvents(events)))
	})

	t.Run("a run that has completed no phase reports empty", func(t *testing.T) {
		builder := newEventBuilder()
		builder.started(t, testConfig)

		assert.Equal(t, "", deriveLastCompletedPhase(historyFromEvents(builder.events)))
	})
}
