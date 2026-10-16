package runsapi

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// historyFromEvents parses events the same way fetchRunHistory does.
func historyFromEvents(events []*historypb.HistoryEvent) runHistory {
	var history runHistory

	indexByScheduled := make(map[int64]int)

	for _, event := range events {
		applyHistoryEvent(&history, indexByScheduled, event)
	}

	populateFailingPhase(&history)

	return history
}

// pausedRunHistory builds an open run that completed Load and then blocked on
// alertName's pause, as seen while no worker is polling (issue #329).
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
				// Resumed: a real activity follows the alert.
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

func TestDerivePauseDecodeFailureDegradesToKind(t *testing.T) {
	builder := newEventBuilder()
	builder.started(t, testConfig)
	// A bare string cannot decode into WritePathPauseInput.
	alertID := builder.scheduled(t, notifyWritePathPauseActivity, "not-a-write-path-pause-input")
	builder.completed(t, alertID, struct{}{})

	got := derivePause(historyFromEvents(builder.events))

	assert.Equal(t, backup.PauseWriteFailure, got.Kind, "the pause kind must survive an undecodable alert input")
	assert.Empty(t, got.AffectedTapes, "details are simply absent when the input cannot be decoded")
}

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

func TestDeriveRunPhases(t *testing.T) {
	openPrepare := newEventBuilder()
	openPrepare.started(t, testConfig)
	resolveID := openPrepare.scheduled(t, "ResolveAndCheck", struct{}{})
	openPrepare.completed(t, resolveID, struct{}{})
	openPrepare.scheduled(t, "PrepareArchives", struct{}{})

	tests := []struct {
		name              string
		iterator          *fakeHistoryIterator
		wantLastCompleted string
		wantActive        []string
		errorAssertion    require.ErrorAssertionFunc
	}{
		{
			name:              "an open run in Prepare",
			iterator:          &fakeHistoryIterator{events: openPrepare.events},
			wantLastCompleted: backup.PhaseResolve,
			wantActive:        []string{backup.PhasePrepare},
		},
		{
			name:           "an unreadable history",
			iterator:       &fakeHistoryIterator{err: errors.New("history unavailable")},
			errorAssertion: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.errorAssertion == nil {
				test.errorAssertion = require.NoError
			}

			temporalClient := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator { return test.iterator }}

			phases, err := DeriveRunPhases(t.Context(), temporalClient, "run-1")
			test.errorAssertion(t, err)

			assert.Equal(t, test.wantLastCompleted, phases.LastCompleted)

			var active []string
			for _, phase := range phases.Active {
				active = append(active, phase.Name)
			}

			assert.Equal(t, test.wantActive, active)
		})
	}
}
