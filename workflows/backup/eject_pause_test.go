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
	env.RegisterActivity(newEjectActivities())
	env.RegisterActivity(&FailureActivities{})

	return env
}

// TestEjectPhaseSignalResume covers AC1 + AC3: when the I/O station fills, the
// phase pauses and alerts the operator instead of failing, and an explicit
// OperatorEjectClearedSignal (the fallback for libraries that do not report the
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
		env.SignalWorkflow(OperatorEjectClearedSignal, nil)
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
