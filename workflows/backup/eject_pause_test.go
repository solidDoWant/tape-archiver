package backup

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// ejectPauseParams seeds the ejectPauseTestWorkflow.
type ejectPauseParams struct {
	Cfg     config.Config
	Written []WrittenTape
}

// ejectPauseTestWorkflow drives only the Eject phase, registering
// CurrentPauseQuery like the real entry point so tests can query mid-pause.
func ejectPauseTestWorkflow(ctx workflow.Context, params ejectPauseParams) error {
	state := &runState{}

	if err := workflow.SetQueryHandler(ctx, CurrentPauseQuery, func() (CurrentPause, error) {
		return state.currentPause, nil
	}); err != nil {
		return err
	}

	return ejectPhase(ctx, params.Cfg, state, params.Written)
}

// ejectPauseConfig returns a library config with the given operator wait bound.
func ejectPauseConfig(ioWaitSeconds int) config.Config {
	return config.Config{
		Library: config.Library{
			Changer:              "/dev/sch0",
			Drives:               []string{"/dev/nst0"},
			BlankSlots:           []int{100, 101},
			TapeCapacityBytes:    2_500_000_000_000,
			IOWaitTimeoutSeconds: &ioWaitSeconds,
		},
	}
}

// twoWrittenTapes returns two written tapes for the Eject phase to export.
func twoWrittenTapes() []WrittenTape {
	return []WrittenTape{
		{Barcode: "TA0001L6", DriveIndex: 0, SourceSlot: 100},
		{Barcode: "TA0002L6", DriveIndex: 0, SourceSlot: 101},
	}
}

func newEjectPauseEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()

	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(ejectPauseTestWorkflow)
	// The auto-resume poll loop runs in this child workflow (#168).
	env.RegisterWorkflow(ioStationWaitWorkflow)
	env.RegisterActivity(newEjectActivities())
	env.RegisterActivity(&FailureActivities{})

	return env
}

// TestEjectPhaseSignalResume covers AC1 + AC3: a full station pauses instead of
// failing, and OperatorResumeSignal resumes the remaining exports.
func TestEjectPhaseSignalResume(t *testing.T) {
	env := newEjectPauseEnv(t)

	var (
		mu          sync.Mutex
		ejectCalls  int
		pauseAlerts int
	)

	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input EjectInput) (EjectResult, error) {
			mu.Lock()
			defer mu.Unlock()

			ejectCalls++

			if ejectCalls == 1 {
				return EjectResult{
					InIOStation: []tape.Barcode{"TA0001L6"},
					Remaining:   input.WrittenTapes[1:],
				}, nil
			}

			return EjectResult{InIOStation: []tape.Barcode{"TA0001L6", "TA0002L6"}}, nil
		})

	// No access-state reporting, so only the signal can end the wait.
	env.OnActivity((&EjectActivities{}).IOStationStatus, mock.Anything, mock.Anything).Return(
		IOStatus{FreeSlots: 0, AccessReported: false}, nil)

	env.OnActivity((&FailureActivities{}).NotifyOperatorPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input OperatorPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			assert.Equal(t, []string{"TA0001L6"}, input.ReadyForRemoval, "alert names the exported tape")
			assert.Equal(t, 1, input.Awaiting, "alert reports one tape awaiting export")

			return nil
		})

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 45*time.Second)

	env.ExecuteWorkflow(ejectPauseTestWorkflow, ejectPauseParams{
		Cfg:     ejectPauseConfig(3600),
		Written: twoWrittenTapes(),
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.Equal(t, 2, ejectCalls, "Eject runs once, pauses, then resumes")
	assert.Equal(t, 1, pauseAlerts, "the operator is alerted exactly once")
}

// TestEjectPhaseCurrentPauseQuery covers CurrentPauseQuery for the Eject pause
// while paused and after completion.
func TestEjectPhaseCurrentPauseQuery(t *testing.T) {
	env := newEjectPauseEnv(t)

	var ejectCalls int

	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input EjectInput) (EjectResult, error) {
			ejectCalls++

			if ejectCalls == 1 {
				return EjectResult{
					InIOStation: []tape.Barcode{"TA0001L6"},
					Remaining:   input.WrittenTapes[1:],
				}, nil
			}

			return EjectResult{InIOStation: []tape.Barcode{"TA0001L6", "TA0002L6"}}, nil
		})

	env.OnActivity((&EjectActivities{}).IOStationStatus, mock.Anything, mock.Anything).Return(
		IOStatus{FreeSlots: 0, AccessReported: false}, nil)

	env.OnActivity((&FailureActivities{}).NotifyOperatorPause, mock.Anything, mock.Anything).Return(nil)

	env.RegisterDelayedCallback(func() {
		value, err := env.QueryWorkflow(CurrentPauseQuery)
		require.NoError(t, err)

		var pause CurrentPause
		require.NoError(t, value.Get(&pause))

		assert.Equal(t, PauseEject, pause.Kind)
		assert.Equal(t, []string{"TA0001L6"}, pause.AffectedTapes)
		assert.Equal(t, 1, pause.AwaitingExport)

		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 10*time.Second)

	env.ExecuteWorkflow(ejectPauseTestWorkflow, ejectPauseParams{
		Cfg:     ejectPauseConfig(3600),
		Written: twoWrittenTapes(),
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	value, err := env.QueryWorkflow(CurrentPauseQuery)
	require.NoError(t, err)

	var pause CurrentPause
	require.NoError(t, value.Get(&pause))
	assert.Equal(t, PauseNone, pause.Kind, "pause state clears once the run completes")
}

// TestEjectResumeInAlertToWaitGapResumes covers #216 AC2: a resume sent after
// the alert but before the wait begins must not be drained as stale.
func TestEjectResumeInAlertToWaitGapResumes(t *testing.T) {
	env := newEjectPauseEnv(t)

	var (
		mu          sync.Mutex
		ejectCalls  int
		pauseAlerts int
	)

	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input EjectInput) (EjectResult, error) {
			mu.Lock()
			defer mu.Unlock()

			ejectCalls++

			if ejectCalls == 1 {
				return EjectResult{
					InIOStation: []tape.Barcode{"TA0001L6"},
					Remaining:   input.WrittenTapes[1:],
				}, nil
			}

			return EjectResult{InIOStation: []tape.Barcode{"TA0001L6", "TA0002L6"}}, nil
		})

	env.OnActivity((&EjectActivities{}).IOStationStatus, mock.Anything, mock.Anything).Return(
		IOStatus{FreeSlots: 0, AccessReported: false}, nil)

	env.OnActivity((&FailureActivities{}).NotifyOperatorPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ OperatorPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			return nil
		})

	resumeWhilePauseAlertFires(env, "NotifyOperatorPause")

	env.ExecuteWorkflow(ejectPauseTestWorkflow, ejectPauseParams{
		Cfg:     ejectPauseConfig(300),
		Written: twoWrittenTapes(),
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "a resume in the alert-to-wait gap must resume the export, not be drained")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 2, ejectCalls, "Eject runs once, pauses, then resumes on the honored signal")
	assert.Equal(t, 1, pauseAlerts, "the operator is alerted exactly once")
}

// TestEjectPhaseAutoResume covers AC2: a library reporting the access bit
// resumes with no signal once the station is closed with a free slot.
func TestEjectPhaseAutoResume(t *testing.T) {
	env := newEjectPauseEnv(t)

	var (
		mu         sync.Mutex
		ejectCalls int
		polls      int
	)

	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input EjectInput) (EjectResult, error) {
			mu.Lock()
			defer mu.Unlock()

			ejectCalls++

			if ejectCalls == 1 {
				return EjectResult{
					InIOStation: []tape.Barcode{"TA0001L6"},
					Remaining:   input.WrittenTapes[1:],
				}, nil
			}

			return EjectResult{}, nil
		})

	env.OnActivity((&EjectActivities{}).IOStationStatus, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ IOStatusInput) (IOStatus, error) {
			mu.Lock()
			defer mu.Unlock()

			polls++

			if polls == 1 {
				return IOStatus{FreeSlots: 0, AccessReported: true, StationClosed: false}, nil
			}

			return IOStatus{FreeSlots: 1, AccessReported: true, StationClosed: true}, nil
		})

	env.OnActivity((&FailureActivities{}).NotifyOperatorPause, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(ejectPauseTestWorkflow, ejectPauseParams{
		Cfg:     ejectPauseConfig(3600),
		Written: twoWrittenTapes(),
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.Equal(t, 2, ejectCalls, "Eject resumes after the station reports closed")
	assert.GreaterOrEqual(t, polls, 2, "at least two polls: door-open then closed")
}

// TestEjectPhaseTimeout covers AC4: an unanswered pause ends in a defined,
// reported state.
func TestEjectPhaseTimeout(t *testing.T) {
	env := newEjectPauseEnv(t)

	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input EjectInput) (EjectResult, error) {
			return EjectResult{
				InIOStation: []tape.Barcode{"TA0001L6"},
				Remaining:   input.WrittenTapes[1:],
			}, nil
		})

	env.OnActivity((&EjectActivities{}).IOStationStatus, mock.Anything, mock.Anything).Return(
		IOStatus{FreeSlots: 0, AccessReported: false}, nil)

	env.OnActivity((&FailureActivities{}).NotifyOperatorPause, mock.Anything, mock.Anything).Return(nil)

	// The 100s wait elapses (polls at 30/60/90s).
	env.ExecuteWorkflow(ejectPauseTestWorkflow, ejectPauseParams{
		Cfg:     ejectPauseConfig(100),
		Written: twoWrittenTapes(),
	})

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "the run fails when the operator never clears the station")
	assert.Contains(t, err.Error(), "did not clear the import/export station")
}

// TestEjectPhaseBoundedHistory covers AC1: a pause outlasting
// maxPollsBeforeContinue polls still resumes, proving the child continued as
// new rather than growing one unbounded execution.
func TestEjectPhaseBoundedHistory(t *testing.T) {
	env := newEjectPauseEnv(t)

	resumeAtPoll := maxPollsBeforeContinue + 5

	var (
		mu         sync.Mutex
		ejectCalls int
		polls      int
	)

	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input EjectInput) (EjectResult, error) {
			mu.Lock()
			defer mu.Unlock()

			ejectCalls++

			if ejectCalls == 1 {
				return EjectResult{
					InIOStation: []tape.Barcode{"TA0001L6"},
					Remaining:   input.WrittenTapes[1:],
				}, nil
			}

			return EjectResult{}, nil
		})

	env.OnActivity((&EjectActivities{}).IOStationStatus, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ IOStatusInput) (IOStatus, error) {
			mu.Lock()
			defer mu.Unlock()

			polls++

			if polls < resumeAtPoll {
				return IOStatus{FreeSlots: 0, AccessReported: true, StationClosed: false}, nil
			}

			return IOStatus{FreeSlots: 1, AccessReported: true, StationClosed: true}, nil
		})

	env.OnActivity((&FailureActivities{}).NotifyOperatorPause, mock.Anything, mock.Anything).Return(nil)

	// Wait budget well beyond resumeAtPoll intervals.
	env.ExecuteWorkflow(ejectPauseTestWorkflow, ejectPauseParams{
		Cfg:     ejectPauseConfig(4 * 60 * 60),
		Written: twoWrittenTapes(),
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 2, ejectCalls, "Eject resumes and exports the remaining tape after the long pause")
	assert.GreaterOrEqual(t, polls, resumeAtPoll,
		"the pause polled past the continuation bound, so the child workflow must have ContinueAsNew'd at least once")
}

// TestBoundedHistoryEventBudget covers AC1: one child execution's worst-case
// history stays far below Temporal's 51,200-event limit.
func TestBoundedHistoryEventBudget(t *testing.T) {
	t.Parallel()

	const (
		historyHardLimit = 51_200
		// Worst case: timer start+fire plus ioStatusMaxAttempts activity
		// attempts (schedule + start + fail each).
		worstCaseEventsPerPoll = 3 + 3*ioStatusMaxAttempts
	)

	perExecution := maxPollsBeforeContinue * worstCaseEventsPerPoll

	assert.Less(t, perExecution, historyHardLimit/4,
		"a single child execution's history must stay well under the server limit before ContinueAsNew resets it")
}

func TestIOStatusCanAutoResume(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status IOStatus
		want   bool
	}{
		{name: "closed with free slot", status: IOStatus{FreeSlots: 1, AccessReported: true, StationClosed: true}, want: true},
		{name: "closed but full", status: IOStatus{FreeSlots: 0, AccessReported: true, StationClosed: true}, want: false},
		{name: "free but open", status: IOStatus{FreeSlots: 1, AccessReported: true, StationClosed: false}, want: false},
		{name: "access not reported", status: IOStatus{FreeSlots: 1, AccessReported: false, StationClosed: true}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, tc.status.CanAutoResume())
		})
	}
}

func TestIOStatusFromInventory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		inv  tape.Inventory
		want IOStatus
	}{
		{
			name: "no access reported, one free slot",
			inv: tape.Inventory{IOSlots: []tape.IOElement{
				{Address: 48, Full: true},
				{Address: 49},
			}},
			want: IOStatus{FreeSlots: 1, AccessReported: false, StationClosed: false},
		},
		{
			name: "access reported, all accessible, free slot",
			inv: tape.Inventory{IOAccessReported: true, IOSlots: []tape.IOElement{
				{Address: 48, Full: true, Accessible: true},
				{Address: 49, Accessible: true},
			}},
			want: IOStatus{FreeSlots: 1, AccessReported: true, StationClosed: true},
		},
		{
			name: "access reported but one slot open",
			inv: tape.Inventory{IOAccessReported: true, IOSlots: []tape.IOElement{
				{Address: 48, Accessible: true},
				{Address: 49, Accessible: false},
			}},
			want: IOStatus{FreeSlots: 2, AccessReported: true, StationClosed: false},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, ioStatus(tc.inv))
		})
	}
}

func TestBarcodesInIOStation(t *testing.T) {
	t.Parallel()

	inv := tape.Inventory{IOSlots: []tape.IOElement{
		{Address: 48, Full: true, Barcode: "TA0001L6"},
		{Address: 49},
		{Address: 50, Full: true, Barcode: "TA0002L6"},
	}}

	assert.Equal(t, []tape.Barcode{"TA0001L6", "TA0002L6"}, barcodesInIOStation(inv))
}
