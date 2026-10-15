package backup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// resumeWhilePauseAlertFires sends OperatorResumeSignal as the pause-alert
// activity starts, deterministically reproducing the alert-to-wait gap (worker
// restart or backlog) in which a drain at wait entry would discard a legitimate
// resume (#216). Fires once.
func resumeWhilePauseAlertFires(env *testsuite.TestWorkflowEnvironment, alertActivity string) {
	signalWhilePauseAlertFires(env, alertActivity, OperatorResumeSignal)
}

// signalWhilePauseAlertFires delivers the named signal as the named pause-alert
// activity first starts; it backs the resume (#216) and abort (#254) gap tests.
func signalWhilePauseAlertFires(env *testsuite.TestWorkflowEnvironment, alertActivity, signalName string) {
	var once sync.Once

	// Buffered while the alert runs, so it lands before the wait's task and
	// survives only if the drain runs ahead of the alert.
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name != alertActivity {
			return
		}

		once.Do(func() {
			env.SignalWorkflow(signalName, nil)
		})
	})
}

// writePauseParams seeds the writePauseTestWorkflow.
type writePauseParams struct {
	Cfg config.Config
	Set driveSet
}

// writePauseTestWorkflow drives runDriveSet for one drive-set with archive-less
// tapes (an empty valid tree), registering CurrentPauseQuery like the real entry
// point.
func writePauseTestWorkflow(ctx workflow.Context, params writePauseParams) error {
	state := &runState{plan: TapePlan{Copies: 2, Tapes: []PlannedTape{{}, {}}}}
	failing := ""

	if err := workflow.SetQueryHandler(ctx, CurrentPauseQuery, func() (CurrentPause, error) {
		return state.currentPause, nil
	}); err != nil {
		return err
	}

	return runDriveSet(ctx, params.Cfg, state, params.Set, &failing)
}

// twoDriveConfig is a two-drive library, enough for a partial write failure.
func twoDriveConfig(writeFailureWaitSeconds int) config.Config {
	return config.Config{
		Library: config.Library{
			Changer:                        "/dev/sch0",
			Drives:                         []string{"/dev/nst0", "/dev/nst1"},
			BlankSlots:                     []int{100, 101},
			TapeCapacityBytes:              2_500_000_000_000,
			WriteFailureWaitTimeoutSeconds: &writeFailureWaitSeconds,
		},
	}
}

// twoTapeSet is one drive-set of two copies, one per drive, from slots 100-101.
// DriveIndex mirrors planDriveSets so mocked Load pairs drives correctly (#137).
func twoTapeSet() driveSet {
	return driveSet{
		{Drive: "/dev/nst0", DriveIndex: 0, BlankSlot: 100, TapeIndex: 0, CopyIndex: 0},
		{Drive: "/dev/nst1", DriveIndex: 1, BlankSlot: 101, TapeIndex: 0, CopyIndex: 1},
	}
}

// sgForDrive maps a non-rewinding node to its SCSI generic node.
func sgForDrive(nst string) string {
	switch nst {
	case "/dev/nst0":
		return "/dev/sg0"
	case "/dev/nst1":
		return "/dev/sg1"
	default:
		return "/dev/sg9"
	}
}

// barcodeForSlot gives each blank slot a stable barcode.
func barcodeForSlot(slot int) tape.Barcode {
	return tape.Barcode(fmt.Sprintf("TA%04dL6", slot))
}

// loadedFromAssignment mirrors Load's contract: the tape lands on the drive the
// assignment names, with its config drive index, not set position (#137).
func loadedFromAssignment(assignment TapeAssignment) LoadedTape {
	return LoadedTape{
		Barcode:    barcodeForSlot(assignment.BlankSlot),
		DriveIndex: assignment.DriveIndex,
		TapeIndex:  assignment.TapeIndex,
		CopyIndex:  assignment.CopyIndex,
		SourceSlot: assignment.BlankSlot,
		STDevice:   assignment.Drive,
		SGDevice:   sgForDrive(assignment.Drive),
	}
}

// mockLoadFromSet makes Load synthesize LoadedTapes from its input assignments.
func mockLoadFromSet(env *testsuite.TestWorkflowEnvironment, _ config.Config) {
	env.OnActivity((&LoadActivities{}).Load, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input LoadInput) ([]LoadedTape, error) {
			loaded := make([]LoadedTape, len(input.Tapes))
			for i, assignment := range input.Tapes {
				loaded[i] = loadedFromAssignment(assignment)
			}

			return loaded, nil
		})
}

// newWritePauseEnv registers the write-path activities with sessions enabled.
func newWritePauseEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()

	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(writePauseTestWorkflow)
	env.SetWorkerOptions(worker.Options{EnableSessionWorker: true})

	env.RegisterActivity(newLoadActivities())

	registry := newMountRegistry()
	env.RegisterActivity(newWriteActivities(registry, t.TempDir()))
	env.RegisterActivity(newTeardownActivities(registry))
	env.RegisterActivity(newWriteHealthActivities(nil))
	env.RegisterActivity(newEjectActivities())
	env.RegisterActivity(&FailureActivities{})

	return env
}

// expectHealthyWriteExceptFormat mocks every write step except FormatTape.
func expectHealthyWriteExceptFormat(env *testsuite.TestWorkflowEnvironment) {
	env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return("/stage/indexes/tape.xml", nil)
	env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(WriteHealth{}, nil)
}

// TestWritePathPauseSignalResume covers AC1, AC2, AC5: a partial failure keeps
// the good tape, pauses, and on resume re-drives only the failed tape; the good
// tape is never re-formatted.
func TestWritePathPauseSignalResume(t *testing.T) {
	cfg := twoDriveConfig(3600)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)

	var (
		mu           sync.Mutex
		formatsByDev = map[string]int{}
		ejectCounts  []int
		pauseAlerts  int
		loadInputs   []LoadInput
	)

	// sg1 fails its first format, then succeeds on the retry.
	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			mu.Lock()
			defer mu.Unlock()

			formatsByDev[input.Device]++

			if input.Device == "/dev/sg1" && formatsByDev[input.Device] == 1 {
				return errors.New("mkltfs: drive reported a hard write error")
			}

			return nil
		})

	// Record Load inputs: the retry must target the failed tape's drive and
	// config index (AC1/AC2).
	env.OnActivity((&LoadActivities{}).Load, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input LoadInput) ([]LoadedTape, error) {
			mu.Lock()
			defer mu.Unlock()

			loadInputs = append(loadInputs, input)

			loaded := make([]LoadedTape, len(input.Tapes))
			for i, assignment := range input.Tapes {
				loaded[i] = loadedFromAssignment(assignment)
			}

			return loaded, nil
		})

	// First Eject takes both tapes, the second only the retried one.
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input EjectInput) (EjectResult, error) {
			mu.Lock()
			defer mu.Unlock()

			ejectCounts = append(ejectCounts, len(input.WrittenTapes))

			return EjectResult{}, nil
		})

	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input WritePathPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			assert.Equal(t, PhaseWrite, input.Phase, "alert names the Write phase")
			assert.Equal(t, []string{string(barcodeForSlot(101))}, input.AffectedTapes,
				"alert names only the failed tape")
			assert.Equal(t, []int{101}, input.ReloadSlots, "alert names the failed tape's slot to restock")

			return nil
		})

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.Equal(t, 1, pauseAlerts, "the operator is alerted exactly once")
	assert.Equal(t, 1, formatsByDev["/dev/sg0"], "the tape that succeeded is never re-formatted (AC5)")
	assert.Equal(t, 2, formatsByDev["/dev/sg1"], "the failed tape is re-formatted once on resume")
	assert.Equal(t, []int{2, 1}, ejectCounts, "first eject frees both tapes; resume ejects only the retried one")

	// Retry set: one assignment on drive 1's node and index (#137 AC1/AC2).
	require.Len(t, loadInputs, 2, "one Load for the initial set, one for the resume retry")
	retry := loadInputs[1].Tapes
	require.Len(t, retry, 1, "the resume retry re-drives only the failed tape")
	assert.Equal(t, "/dev/nst1", retry[0].Drive, "retried on the failed tape's physical drive")
	assert.Equal(t, 1, retry[0].DriveIndex, "retried tape carries its config drive index (AC2)")
	assert.Equal(t, 101, retry[0].BlankSlot, "the fresh blank is reloaded into the failed tape's slot")
}

// TestWritePathCurrentPauseQuery covers CurrentPauseQuery for a write-failure
// pause while paused and after completion.
func TestWritePathCurrentPauseQuery(t *testing.T) {
	cfg := twoDriveConfig(3600)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)

	var (
		mu           sync.Mutex
		formatsByDev = map[string]int{}
	)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			mu.Lock()
			defer mu.Unlock()

			formatsByDev[input.Device]++

			if input.Device == "/dev/sg1" && formatsByDev[input.Device] == 1 {
				return errors.New("mkltfs: drive reported a hard write error")
			}

			return nil
		})

	mockLoadFromSet(env, cfg)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(nil)

	env.RegisterDelayedCallback(func() {
		value, err := env.QueryWorkflow(CurrentPauseQuery)
		require.NoError(t, err)

		var pause CurrentPause
		require.NoError(t, value.Get(&pause))

		assert.Equal(t, PauseWriteFailure, pause.Kind)
		assert.Equal(t, PhaseWrite, pause.Phase)
		assert.Equal(t, []string{string(barcodeForSlot(101))}, pause.AffectedTapes)
		assert.Equal(t, []int{101}, pause.ReloadSlots)
		assert.Contains(t, pause.ErrorSummary, "hard write error")

		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 10*time.Second)

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	value, err := env.QueryWorkflow(CurrentPauseQuery)
	require.NoError(t, err)

	var pause CurrentPause
	require.NoError(t, value.Get(&pause))
	assert.Equal(t, PauseNone, pause.Kind, "pause state clears once the run completes")
}

// TestWritePathResumeInAlertToWaitGapResumes covers #216 AC1: a resume sent
// between the alert and the wait must resume the run, not be drained.
func TestWritePathResumeInAlertToWaitGapResumes(t *testing.T) {
	cfg := twoDriveConfig(300)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)

	var (
		mu           sync.Mutex
		formatsByDev = map[string]int{}
		pauseAlerts  int
	)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			mu.Lock()
			defer mu.Unlock()

			formatsByDev[input.Device]++

			if input.Device == "/dev/sg1" && formatsByDev[input.Device] == 1 {
				return errors.New("mkltfs: drive reported a hard write error")
			}

			return nil
		})

	mockLoadFromSet(env, cfg)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ WritePathPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			return nil
		})

	resumeWhilePauseAlertFires(env, "NotifyWritePathPause")

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "a resume in the alert-to-wait gap must resume the run, not be drained")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, pauseAlerts, "the operator is alerted exactly once")
	assert.Equal(t, 2, formatsByDev["/dev/sg1"], "the failed tape is re-formatted on the honored resume")
}

// TestWritePathStaleResumeSignalDoesNotSatisfyLaterPause covers #154 AC1: a
// surplus resume from the first pause must not satisfy the second, so the run
// times out instead of succeeding on a third format attempt.
func TestWritePathStaleResumeSignalDoesNotSatisfyLaterPause(t *testing.T) {
	cfg := twoDriveConfig(300)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)

	var (
		mu           sync.Mutex
		formatsByDev = map[string]int{}
		pauseAlerts  int
	)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			mu.Lock()
			defer mu.Unlock()

			formatsByDev[input.Device]++

			if input.Device == "/dev/sg1" && formatsByDev[input.Device] <= 2 {
				return errors.New("mkltfs: drive reported a hard write error")
			}

			return nil
		})

	mockLoadFromSet(env, cfg)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ WritePathPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			return nil
		})

	// Double-sent resume: the surplus must not leak to the second pause.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "the surplus resume must not satisfy the second pause; it times out")
	assert.Contains(t, err.Error(), "did not resume or abort")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 2, pauseAlerts, "both write-failure pauses alert the operator")
	assert.Equal(t, 2, formatsByDev["/dev/sg1"], "the failed tape is retried once; the drained signal blocks a third attempt")
}

// TestWritePathStaleAbortSignalDoesNotAbortLaterPause covers #254 AC1: a stale
// abort racing in behind a resume must be drained at the next pause, which then
// times out rather than aborting.
func TestWritePathStaleAbortSignalDoesNotAbortLaterPause(t *testing.T) {
	cfg := twoDriveConfig(300)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)

	var (
		mu           sync.Mutex
		formatsByDev = map[string]int{}
		pauseAlerts  int
	)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			mu.Lock()
			defer mu.Unlock()

			formatsByDev[input.Device]++

			if input.Device == "/dev/sg1" && formatsByDev[input.Device] <= 2 {
				return errors.New("mkltfs: drive reported a hard write error")
			}

			return nil
		})

	mockLoadFromSet(env, cfg)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ WritePathPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			return nil
		})

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
		env.SignalWorkflow(OperatorAbortSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "the stale abort must not satisfy the second pause; it times out")
	assert.Contains(t, err.Error(), "did not resume or abort",
		"the second pause times out waiting for a fresh action")
	assert.NotContains(t, err.Error(), "aborted by operator",
		"the stale abort must not end the run at the second pause")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 2, pauseAlerts, "both write-failure pauses alert the operator")
	assert.Equal(t, 2, formatsByDev["/dev/sg1"], "the failed tape is retried exactly once; the drained abort neither ends the run nor triggers more retries")
}

// TestWritePathAbortInAlertToWaitGapAborts is the #254 counterpart of the
// resume-gap test: an abort sent between the alert and the wait must abort.
func TestWritePathAbortInAlertToWaitGapAborts(t *testing.T) {
	cfg := twoDriveConfig(300)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)

	var (
		mu          sync.Mutex
		formats     int
		pauseAlerts int
	)

	// sg1 always fails, so only an honored abort yields the aborted outcome.
	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			mu.Lock()
			defer mu.Unlock()

			formats++

			if input.Device == "/dev/sg1" {
				return errors.New("mkltfs: drive reported a hard write error")
			}

			return nil
		})

	mockLoadFromSet(env, cfg)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ WritePathPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			return nil
		})

	signalWhilePauseAlertFires(env, "NotifyWritePathPause", OperatorAbortSignal)

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "an aborted run ends with an error")
	assert.Contains(t, err.Error(), "aborted by operator",
		"an abort in the alert-to-wait gap must abort the run, not be drained")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, pauseAlerts, "the operator is alerted exactly once")
}

// TestWritePathAbortDuringEjectPauseDoesNotAbortLaterPause covers #254's
// motivating scenario: an abort sent during an Eject pause (which ignores abort)
// stays buffered and must not abort the following write-failure pause.
func TestWritePathAbortDuringEjectPauseDoesNotAbortLaterPause(t *testing.T) {
	cfg := twoDriveConfig(300)
	env := newWritePauseEnv(t)
	// The auto-resume poll loop child workflow (#168).
	env.RegisterWorkflow(ioStationWaitWorkflow)
	expectHealthyWriteExceptFormat(env)

	var (
		mu               sync.Mutex
		ejectCalls       int
		ejectPauseAlerts int
		writePauseAlerts int
	)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			if input.Device == "/dev/sg1" {
				return errors.New("mkltfs: drive reported a hard write error")
			}

			return nil
		})

	mockLoadFromSet(env, cfg)

	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input EjectInput) (EjectResult, error) {
			mu.Lock()
			defer mu.Unlock()

			ejectCalls++

			if ejectCalls == 1 {
				return EjectResult{
					InIOStation: []tape.Barcode{input.WrittenTapes[0].Barcode},
					Remaining:   input.WrittenTapes[1:],
				}, nil
			}

			return EjectResult{}, nil
		})

	// No access-state reporting: only resume ends the Eject pause.
	env.OnActivity((&EjectActivities{}).IOStationStatus, mock.Anything, mock.Anything).Return(
		IOStatus{FreeSlots: 0, AccessReported: false}, nil)

	env.OnActivity((&FailureActivities{}).NotifyOperatorPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ OperatorPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			ejectPauseAlerts++

			return nil
		})
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ WritePathPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			writePauseAlerts++

			return nil
		})

	// A stale abort lands during the Eject pause, then the operator resumes.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorAbortSignal, nil)
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "the leaked abort must not satisfy the write-failure pause; it times out")
	assert.Contains(t, err.Error(), "did not resume or abort",
		"the write-failure pause times out waiting for a fresh action")
	assert.NotContains(t, err.Error(), "aborted by operator",
		"the abort buffered at the Eject pause must not end the run at the later pause")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, ejectPauseAlerts, "the Eject I/O-station pause alerts once")
	assert.Equal(t, 2, ejectCalls, "the export resumes on the honored resume signal")
	assert.Equal(t, 1, writePauseAlerts, "the write-failure pause is reached and alerts once")
}

// TestWritePathPauseAbort covers AC3: abort ends the run with no retry.
func TestWritePathPauseAbort(t *testing.T) {
	cfg := twoDriveConfig(3600)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)

	var (
		mu      sync.Mutex
		formats int
	)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			mu.Lock()
			defer mu.Unlock()

			formats++

			if input.Device == "/dev/sg1" {
				return errors.New("mkltfs: drive reported a hard write error")
			}

			return nil
		})

	mockLoadFromSet(env, cfg)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(nil)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorAbortSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "an aborted run ends with an error")
	assert.Contains(t, err.Error(), "aborted by operator")

	mu.Lock()
	defer mu.Unlock()
	// Two format attempts only: abort stops the run rather than retrying.
	assert.Equal(t, 2, formats, "no tape is written after abort")
}

// TestWritePathPauseTimeout covers AC4: an unanswered pause fails the run.
func TestWritePathPauseTimeout(t *testing.T) {
	cfg := twoDriveConfig(100)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			if input.Device == "/dev/sg1" {
				return errors.New("mkltfs: drive reported a hard write error")
			}

			return nil
		})

	mockLoadFromSet(env, cfg)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "the run fails when the operator never responds")
	assert.Contains(t, err.Error(), "did not resume or abort")
}

// TestWritePathLoadFailurePauseResume covers AC1 for Load: retry the whole set.
func TestWritePathLoadFailurePauseResume(t *testing.T) {
	cfg := twoDriveConfig(3600)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)
	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(nil)

	var (
		mu         sync.Mutex
		loadCalls  int
		loadAlerts int
	)

	env.OnActivity((&LoadActivities{}).Load, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input LoadInput) ([]LoadedTape, error) {
			mu.Lock()
			defer mu.Unlock()

			loadCalls++

			if loadCalls == 1 {
				return nil, errors.New("move medium: source slot empty")
			}

			loaded := make([]LoadedTape, len(input.Tapes))
			for i, assignment := range input.Tapes {
				loaded[i] = loadedFromAssignment(assignment)
			}

			return loaded, nil
		})

	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input WritePathPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			loadAlerts++

			assert.Equal(t, PhaseLoad, input.Phase, "alert names the Load phase")
			assert.Empty(t, input.AffectedTapes, "a Load failure has no loaded tapes to name")
			assert.Equal(t, []int{100, 101}, input.ReloadSlots, "alert names the whole set's slots")

			return nil
		})

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.Equal(t, 1, loadAlerts, "the operator is alerted once for the Load failure")
	assert.Equal(t, 2, loadCalls, "Load runs once, pauses, then retries the whole set on resume")
}

// TestEjectProjectionDropsIndexAndHealth pins #221: EjectInput carries only
// the fields Eject reads, keeping the payload bounded regardless of run size.
func TestEjectProjectionDropsIndexAndHealth(t *testing.T) {
	t.Parallel()

	written := []WrittenTape{
		{
			Barcode:      tape.Barcode("TAPE01L6"),
			DriveIndex:   0,
			TapeIndex:    1,
			CopyIndex:    0,
			SourceSlot:   4,
			IndexXMLPath: "/stage/run-1/indexes/TAPE01L6.xml",
			WriteHealth:  WriteHealth{Repositions: 7},
		},
		{
			Barcode:      tape.Barcode("TAPE02L6"),
			DriveIndex:   1,
			TapeIndex:    1,
			CopyIndex:    1,
			SourceSlot:   5,
			IndexXMLPath: "/stage/run-1/indexes/TAPE02L6.xml",
			WriteHealth:  WriteHealth{Repositions: 9},
		},
	}

	projected := ejectProjection(written)

	require.Len(t, projected, 2)

	for i, got := range projected {
		assert.Empty(t, got.IndexXMLPath, "eject payload must not carry the staged index path (entry %d)", i)
		assert.Zero(t, got.WriteHealth, "eject payload must not carry write-health (entry %d)", i)
		assert.Equal(t, written[i].Barcode, got.Barcode)
		assert.Equal(t, written[i].DriveIndex, got.DriveIndex)
		assert.Equal(t, written[i].TapeIndex, got.TapeIndex)
		assert.Equal(t, written[i].CopyIndex, got.CopyIndex)
		assert.Equal(t, written[i].SourceSlot, got.SourceSlot)
	}

	projected[0].Barcode = tape.Barcode("MUTATED")
	assert.Equal(t, tape.Barcode("TAPE01L6"), written[0].Barcode, "projection must not alias the input")
}

// TestWritePathLoadNonBlankPauseNamesTape checks a Load refused for a non-blank
// tape alerts with that tape and only its slot.
func TestWritePathLoadNonBlankPauseNamesTape(t *testing.T) {
	cfg := twoDriveConfig(3600)
	env := newWritePauseEnv(t)

	env.OnActivity((&LoadActivities{}).Load, mock.Anything, mock.Anything).Return(nil,
		temporal.NewNonRetryableApplicationError("tape USED01L5 is not blank", nonBlankTapeErrorType, nil, "USED01L5", 101))

	var alert WritePathPauseInput

	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input WritePathPauseInput) error {
			alert = input

			return nil
		})

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorAbortSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())

	assert.Equal(t, PhaseLoad, alert.Phase)
	assert.Equal(t, []string{"USED01L5"}, alert.AffectedTapes)
	assert.Equal(t, []int{101}, alert.ReloadSlots)
}

// TestWritePathPauseCancelIsNotTimeout pins #388: cancelling during a Load or
// Write failure pause ends the run cancelled, not as an operator timeout.
func TestWritePathPauseCancelIsNotTimeout(t *testing.T) {
	tests := []struct {
		name          string
		loadFails     bool
		formatFailsOn string
	}{
		{name: "load failure", loadFails: true},
		{name: "write failure", formatFailsOn: "/dev/sg1"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := twoDriveConfig(3600)
			env := newWritePauseEnv(t)
			expectHealthyWriteExceptFormat(env)

			env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
				func(_ context.Context, input FormatInput) error {
					if input.Device == test.formatFailsOn {
						return errors.New("mkltfs: drive reported a hard write error")
					}

					return nil
				})

			if test.loadFails {
				env.OnActivity((&LoadActivities{}).Load, mock.Anything, mock.Anything).Return(
					nil, errors.New("move medium: source slot empty"))
			} else {
				mockLoadFromSet(env, cfg)
			}

			env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
			env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(nil)

			env.RegisterDelayedCallback(env.CancelWorkflow, 30*time.Second)

			env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

			require.True(t, env.IsWorkflowCompleted())

			err := env.GetWorkflowError()
			require.Error(t, err)
			assert.True(t, temporal.IsCanceledError(err), "the run ends cancelled, got: %v", err)
			assert.NotContains(t, err.Error(), "did not resume or abort")
		})
	}
}
