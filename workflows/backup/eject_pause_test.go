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

// ejectPauseTestWorkflow drives only the Eject phase so its operator-in-the-loop
// pause/resume loop can be tested with mocked Eject/IOStationStatus activities,
// signals, and the test env's time-skipping — without the full tape path.
func ejectPauseTestWorkflow(ctx workflow.Context, params ejectPauseParams) error {
	return ejectPhase(ctx, params.Cfg, params.Written)
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
	// The Eject pause auto-resume poll loop runs in this child workflow
	// (issue #168); it must be registered for the parent to start it.
	env.RegisterWorkflow(ioStationWaitWorkflow)
	env.RegisterActivity(newEjectActivities())
	env.RegisterActivity(&FailureActivities{})

	return env
}

// TestEjectPhaseSignalResume covers AC1 + AC3: when the I/O station fills, the
// phase pauses and alerts the operator instead of failing, and an explicit
// OperatorResumeSignal (the fallback for libraries that do not report the
// access bit) resumes the export of the remaining tapes.
func TestEjectPhaseSignalResume(t *testing.T) {
	env := newEjectPauseEnv(t)

	var (
		mu          sync.Mutex
		ejectCalls  int
		pauseAlerts int
	)

	// First Eject fills the station and reports one tape still remaining; the
	// second (after resume) exports it.
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

	// The library never reports access state, so the poll can never auto-resume —
	// only the signal ends the wait.
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

	// The operator clears the station and signals resume before the wait elapses.
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

// TestEjectResumeInAlertToWaitGapResumes covers issue #216 AC2 for the Eject I/O
// pause: a resume the operator sends after the pause's alert has fired but before
// the workflow task that begins the wait executes must resume the export, not be
// drained as stale. The first Eject fills the station with one tape remaining; the
// resume is delivered from the pause alert firing (buffered ahead of the wait
// task), and the second Eject exports the remainder. A drain at wait entry would
// discard the resume and the run would time out.
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

	// The library never reports access state, so only the signal ends the wait.
	env.OnActivity((&EjectActivities{}).IOStationStatus, mock.Anything, mock.Anything).Return(
		IOStatus{FreeSlots: 0, AccessReported: false}, nil)

	env.OnActivity((&FailureActivities{}).NotifyOperatorPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ OperatorPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			return nil
		})

	// The operator resumes in the alert-to-wait-entry gap.
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

// TestEjectPhaseAutoResume covers AC2: a library that reports the access bit
// resumes automatically once the station is closed with a free slot — no signal.
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

	// First poll: door still open (not closed). Second poll: cleared and closed
	// with a free slot → auto-resume.
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

	// No signal is ever sent — resume must come from the poll alone.
	env.ExecuteWorkflow(ejectPauseTestWorkflow, ejectPauseParams{
		Cfg:     ejectPauseConfig(3600),
		Written: twoWrittenTapes(),
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.Equal(t, 2, ejectCalls, "Eject resumes after the station reports closed")
	assert.GreaterOrEqual(t, polls, 2, "at least two polls: door-open then closed")
}

// TestEjectPhaseTimeout covers AC4: when the operator never responds, the wait
// elapses and the run ends in a defined, reported state (the failure names the
// tapes left in storage slots and that none is in a drive).
func TestEjectPhaseTimeout(t *testing.T) {
	env := newEjectPauseEnv(t)

	// Eject always leaves a tape remaining (station never cleared).
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

	// No signal, no auto-resume: the 100s wait elapses (polls at 30/60/90s).
	env.ExecuteWorkflow(ejectPauseTestWorkflow, ejectPauseParams{
		Cfg:     ejectPauseConfig(100),
		Written: twoWrittenTapes(),
	})

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "the run fails when the operator never clears the station")
	assert.Contains(t, err.Error(), "did not clear the import/export station")
}

// TestEjectPhaseBoundedHistory covers AC1: a pause that outlasts a single child
// execution's poll budget must still resume correctly, which is only possible if
// the poll loop's child workflow ContinueAsNew's to reset its history. It drives
// the pause past maxPollsBeforeContinue polls (so at least one continuation is
// forced) via the test env's time-skipping, then auto-resumes; observing more
// than maxPollsBeforeContinue polls with a clean completion proves the child
// continued rather than growing one unbounded execution.
func TestEjectPhaseBoundedHistory(t *testing.T) {
	env := newEjectPauseEnv(t)

	// Resume a handful of polls past the continuation bound so at least one
	// ContinueAsNew must have happened before the station clears.
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

	// The station stays open (cannot auto-resume) until well past the poll bound,
	// forcing the child to ContinueAsNew, then reports closed with a free slot.
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

	// A long wait budget (well beyond resumeAtPoll poll intervals) so the pause is
	// bounded by the child's ContinueAsNew, not the timeout.
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

// TestBoundedHistoryEventBudget covers AC1 deterministically: the per-execution
// history a single child accrues (its poll budget × the worst-case events per
// poll) stays far below Temporal's 51,200-event hard limit, so no pause length
// can exhaust one execution's history.
func TestBoundedHistoryEventBudget(t *testing.T) {
	t.Parallel()

	const (
		historyHardLimit = 51_200
		// Worst case per poll: a timer (start + fire) plus an IOStationStatus
		// activity retried the full ioStatusMaxAttempts times (schedule + start +
		// fail each). This upper bound is intentionally generous.
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
