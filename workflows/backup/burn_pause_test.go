package backup

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/optical"
)

// isBurnFailurePause reports whether a burn-pause alert is a within-set burn/verify
// failure (as opposed to a between-set disc-swap prompt), distinguished by the
// summary notifyBurnPause renders.
func isBurnFailurePause(input BurnPauseInput) bool {
	return strings.Contains(input.ErrorSummary, "a burn or verify failed")
}

// These tests drive runBurnPath — the burn-set / disc-swap / failure
// pause-resume-abort loop (SPEC §10) — with mocked BurnDisc/VerifyDisc/
// NotifyBurnPause activities and the test env's time-skipping, so the whole
// orchestration is exercised without any optical hardware. They mirror
// write_pause_test.go for the tape path.

// stagedISO and stagedManifest stand in for the artifacts the Report phase stages
// for the Burn phase; the mocked activities only assert they are threaded through.
const (
	stagedISO      = "/staging/run/recovery.iso"
	stagedManifest = "/staging/run/disc-manifest.sha256"
)

// burnPauseResult surfaces the discs runBurnPath recorded so a test can assert
// what was burned (and any overwrite), since the workflow otherwise returns only
// an error.
type burnPauseResult struct {
	Burned []BurnResult
}

// burnPauseTestWorkflow drives runBurnPath for a config so its burn-set loop can
// be tested with mocked activities, signals, and time-skipping — without the full
// pipeline. It seeds the staged ISO/manifest paths the Burn phase consumes and
// returns the discs it recorded. It registers CurrentPauseQuery against its own
// local state, mirroring the real Backup entry point (workflow.go), so tests can
// query pause state mid-pause.
func burnPauseTestWorkflow(ctx workflow.Context, cfg config.Config) (burnPauseResult, error) {
	state := &runState{uncompressedISOPath: stagedISO, discManifestPath: stagedManifest}

	if err := workflow.SetQueryHandler(ctx, CurrentPauseQuery, func() (CurrentPause, error) {
		return state.currentPause, nil
	}); err != nil {
		return burnPauseResult{}, err
	}

	err := runBurnPath(ctx, cfg, state)

	return burnPauseResult{Burned: state.burnedDiscs}, err
}

// burnConfig builds a run config whose optical-burn section has the given burners,
// copy count, non-blank opt-in, and operator-wait bound.
func burnConfig(copies int, drives []string, allowNonBlank bool, waitSeconds int) config.Config {
	return config.Config{
		Delivery: config.Delivery{
			OpticalBurn: &config.OpticalBurn{
				Drives:                 drives,
				Copies:                 copies,
				AllowNonBlankDiscs:     allowNonBlank,
				BurnWaitTimeoutSeconds: &waitSeconds,
			},
		},
	}
}

// newBurnPauseEnv registers the burn/verify and failure-alert activities so the
// test workflow can dispatch them; tests override the specific behavior they
// exercise with OnActivity.
func newBurnPauseEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()

	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(burnPauseTestWorkflow)
	env.RegisterActivity(newBurnActivities())
	env.RegisterActivity(&FailureActivities{})

	return env
}

// TestBurnSetFailurePauseResume covers AC7: when one disc in a set fails to burn,
// the run keeps the disc that succeeded, pauses and alerts the operator naming the
// failed drive, and on resume re-burns only the failed disc — the disc that
// already burned is never re-burned.
func TestBurnSetFailurePauseResume(t *testing.T) {
	cfg := burnConfig(2, []string{"/dev/sr0", "/dev/sr1"}, false, 3600)
	env := newBurnPauseEnv(t)

	var (
		mu          sync.Mutex
		burnsByDev  = map[string]int{}
		pauseAlerts int
	)

	// /dev/sr1 fails to burn on its first attempt, then succeeds on the resume
	// retry; /dev/sr0 always succeeds.
	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnDiscInput) (BurnResult, error) {
			mu.Lock()
			defer mu.Unlock()

			burnsByDev[input.Device]++

			if input.Device == "/dev/sr1" && burnsByDev[input.Device] == 1 {
				return BurnResult{}, errors.New("optical burn failed: drive reported a write error")
			}

			return BurnResult{Device: input.Device}, nil
		})
	env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(nil)

	env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			assert.Equal(t, []string{"/dev/sr1"}, input.Devices, "alert names only the failed burner")
			assert.Contains(t, input.ErrorSummary, "a burn or verify failed", "alert names the failure")

			return nil
		})

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result burnPauseResult
	require.NoError(t, env.GetWorkflowResult(&result))

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, pauseAlerts, "the operator is alerted exactly once")
	assert.Equal(t, 1, burnsByDev["/dev/sr0"], "the disc that succeeded is never re-burned")
	assert.Equal(t, 2, burnsByDev["/dev/sr1"], "the failed disc is re-burned once on resume")
	assert.Len(t, result.Burned, 2, "both copies are recorded once burned and verified")
}

// TestBurnCurrentPauseQuery covers the CurrentPauseQuery contract for a Burn
// pause: while paused it reports PauseBurn with the burner device needing
// attention and an error summary, and once resumed to completion it reports no
// pause.
func TestBurnCurrentPauseQuery(t *testing.T) {
	cfg := burnConfig(2, []string{"/dev/sr0", "/dev/sr1"}, false, 3600)
	env := newBurnPauseEnv(t)

	var (
		mu         sync.Mutex
		burnsByDev = map[string]int{}
	)

	// /dev/sr1 fails its first burn attempt, then succeeds on the resume retry.
	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnDiscInput) (BurnResult, error) {
			mu.Lock()
			defer mu.Unlock()

			burnsByDev[input.Device]++

			if input.Device == "/dev/sr1" && burnsByDev[input.Device] == 1 {
				return BurnResult{}, errors.New("optical burn failed: drive reported a write error")
			}

			return BurnResult{Device: input.Device}, nil
		})
	env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(nil)

	env.RegisterDelayedCallback(func() {
		value, err := env.QueryWorkflow(CurrentPauseQuery)
		require.NoError(t, err)

		var pause CurrentPause
		require.NoError(t, value.Get(&pause))

		assert.Equal(t, PauseBurn, pause.Kind)
		assert.Equal(t, []string{"/dev/sr1"}, pause.Devices)
		assert.Contains(t, pause.ErrorSummary, "drive reported a write error",
			"ErrorSummary carries the raw failure cause, not the alert's human-phrased summary")

		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 10*time.Second)

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	value, err := env.QueryWorkflow(CurrentPauseQuery)
	require.NoError(t, err)

	var pause CurrentPause
	require.NoError(t, value.Get(&pause))
	assert.Equal(t, PauseNone, pause.Kind, "pause state clears once the run completes")
}

// TestBurnResumeInAlertToWaitGapResumes covers issue #216 AC2 for the burn path: a
// resume the operator sends after a burn-set pause's alert has fired but before the
// workflow task that begins the wait executes must resume the run, not be drained
// as stale. /dev/sr1 fails its first burn and succeeds on the resume retry; the
// resume is delivered from the pause alert firing, so it is buffered ahead of
// the wait task. A drain at wait entry would discard it and the run would time out.
func TestBurnResumeInAlertToWaitGapResumes(t *testing.T) {
	cfg := burnConfig(2, []string{"/dev/sr0", "/dev/sr1"}, false, 300)
	env := newBurnPauseEnv(t)

	var (
		mu          sync.Mutex
		burnsByDev  = map[string]int{}
		pauseAlerts int
	)

	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnDiscInput) (BurnResult, error) {
			mu.Lock()
			defer mu.Unlock()

			burnsByDev[input.Device]++

			if input.Device == "/dev/sr1" && burnsByDev[input.Device] == 1 {
				return BurnResult{}, errors.New("optical burn failed: drive reported a write error")
			}

			return BurnResult{Device: input.Device}, nil
		})
	env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ BurnPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			return nil
		})

	// The operator resumes in the alert-to-wait-entry gap.
	resumeWhilePauseAlertFires(env, "NotifyBurnPause")

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "a resume in the alert-to-wait gap must resume the run, not be drained")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, pauseAlerts, "the operator is alerted exactly once")
	assert.Equal(t, 2, burnsByDev["/dev/sr1"], "the failed disc is re-burned on the honored resume")
}

// TestBurnSingleSetAllVerified covers AC2: with copies at or below the burner
// count and a blank disc in each drive, every copy burns and independently
// verifies in a single burn-set — no pause, one burn and one verify per disc.
func TestBurnSingleSetAllVerified(t *testing.T) {
	cfg := burnConfig(2, []string{"/dev/sr0", "/dev/sr1"}, false, 3600)
	env := newBurnPauseEnv(t)

	var (
		mu          sync.Mutex
		burns       int
		verifies    int
		pauseAlerts int
	)

	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnDiscInput) (BurnResult, error) {
			mu.Lock()
			defer mu.Unlock()

			burns++

			return BurnResult{Device: input.Device}, nil
		})
	env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ VerifyDiscInput) error {
			mu.Lock()
			defer mu.Unlock()

			verifies++

			return nil
		})
	env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ BurnPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			return nil
		})

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result burnPauseResult
	require.NoError(t, env.GetWorkflowResult(&result))

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 2, burns, "both copies burn in the single set")
	assert.Equal(t, 2, verifies, "each burned disc is independently verified")
	assert.Equal(t, 0, pauseAlerts, "a fully-blank single set never pauses")
	assert.Len(t, result.Burned, 2, "both copies are recorded burned and verified")
}

// TestBurnSetVerifyFailurePauseResume covers AC7 for a verify mismatch: a disc
// that burns but fails read-back verification is treated as a failure — the run
// pauses and re-burns it on resume.
func TestBurnSetVerifyFailurePauseResume(t *testing.T) {
	cfg := burnConfig(2, []string{"/dev/sr0", "/dev/sr1"}, false, 3600)
	env := newBurnPauseEnv(t)

	var (
		mu          sync.Mutex
		verifyByDev = map[string]int{}
		burnsByDev  = map[string]int{}
		pauseAlerts int
	)

	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnDiscInput) (BurnResult, error) {
			mu.Lock()
			defer mu.Unlock()

			burnsByDev[input.Device]++

			return BurnResult{Device: input.Device}, nil
		})
	// /dev/sr1 fails verification on the first burn, then verifies on the retry.
	env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input VerifyDiscInput) error {
			mu.Lock()
			defer mu.Unlock()

			verifyByDev[input.Device]++

			if input.Device == "/dev/sr1" && verifyByDev[input.Device] == 1 {
				return errors.New("optical: disc does not match manifest (mismatched: report.pdf)")
			}

			return nil
		})

	env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			assert.Equal(t, []string{"/dev/sr1"}, input.Devices)

			return nil
		})

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, pauseAlerts, "a verify mismatch pauses once")
	assert.Equal(t, 2, burnsByDev["/dev/sr1"], "the failed disc is re-burned on resume")
}

// TestBurnSetPauseAbort covers AC7's abort path: an operator abort ends the run in
// a defined, reported state with no further discs burned.
func TestBurnSetPauseAbort(t *testing.T) {
	cfg := burnConfig(1, []string{"/dev/sr0"}, false, 3600)
	env := newBurnPauseEnv(t)

	var (
		mu    sync.Mutex
		burns int
	)

	// The only burner always fails, so a resume would loop; the abort must stop it.
	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ BurnDiscInput) (BurnResult, error) {
			mu.Lock()
			defer mu.Unlock()

			burns++

			return BurnResult{}, errors.New("optical burn failed")
		})
	env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(nil)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorAbortSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "an aborted run ends with an error")
	assert.Contains(t, err.Error(), "aborted by operator")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, burns, "no disc is burned after abort")
}

// TestBurnSetPauseTimeout covers AC8: when the operator neither resumes nor
// aborts, the burn-wait elapses and the run fails in its defined paused state.
func TestBurnSetPauseTimeout(t *testing.T) {
	cfg := burnConfig(1, []string{"/dev/sr0"}, false, 100)
	env := newBurnPauseEnv(t)

	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		BurnResult{}, errors.New("optical burn failed"))
	env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(nil)

	// No signal is ever sent: the 100s wait elapses.
	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "the run fails when the operator never responds")
	assert.Contains(t, err.Error(), "did not resume or abort")
}

// TestBurnBetweenSetSwapPause covers AC3: when copies exceed the burner count the
// discs burn in successive sets, and the run pauses between sets for the operator
// to load fresh blanks (there is no optical autoloader) before the next set burns.
func TestBurnBetweenSetSwapPause(t *testing.T) {
	cfg := burnConfig(3, []string{"/dev/sr0", "/dev/sr1"}, false, 3600)
	env := newBurnPauseEnv(t)

	var (
		mu          sync.Mutex
		burns       int
		swapAlerts  int
		swapDevices []string
	)

	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnDiscInput) (BurnResult, error) {
			mu.Lock()
			defer mu.Unlock()

			burns++

			return BurnResult{Device: input.Device}, nil
		})
	env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(nil)

	env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			swapAlerts++
			swapDevices = input.Devices

			assert.Contains(t, input.ErrorSummary, "burn-set is complete", "a between-set pause is a disc swap, not a failure")

			return nil
		})

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result burnPauseResult
	require.NoError(t, env.GetWorkflowResult(&result))

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 3, burns, "every copy is burned across the two sets")
	assert.Equal(t, 1, swapAlerts, "exactly one between-set swap pause for two sets")
	assert.Equal(t, []string{"/dev/sr0"}, swapDevices, "the swap alert names the next set's burner")
	assert.Len(t, result.Burned, 3, "all three copies are recorded burned and verified")
}

// TestBurnRecordsOverwrite covers AC5: a reclaimed non-blank rewritable disc is
// recorded in the run (its BurnResult.OverwroteNonBlank flows into the report),
// and the run does not pause because the burn succeeded.
func TestBurnRecordsOverwrite(t *testing.T) {
	cfg := burnConfig(1, []string{"/dev/sr0"}, true, 3600)
	env := newBurnPauseEnv(t)

	var pauseAlerts int

	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnDiscInput) (BurnResult, error) {
			assert.True(t, input.AllowNonBlankDiscs, "the opt-in is threaded to the activity")
			assert.Equal(t, stagedISO, input.ISOPath, "the staged ISO is threaded to the activity")

			return BurnResult{Device: input.Device, OverwroteNonBlank: true}, nil
		})
	env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input VerifyDiscInput) error {
			assert.Equal(t, stagedManifest, input.ManifestPath, "the staged disc manifest is threaded to verify")

			return nil
		})
	env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ BurnPauseInput) error {
			pauseAlerts++

			return nil
		})

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result burnPauseResult
	require.NoError(t, env.GetWorkflowResult(&result))

	require.Len(t, result.Burned, 1)
	assert.True(t, result.Burned[0].OverwroteNonBlank, "the deliberate overwrite is recorded for the report")
	assert.Equal(t, 0, pauseAlerts, "a successful reclaim burn does not pause")
}

// TestBurnNonWritableDiscPauses covers AC4/AC6: a disc the activity refuses as
// non-writable (a non-blank disc without the opt-in, or any write-once disc)
// surfaces as IsDiscNotWritable and pauses the run for the operator rather than
// overwriting anything; on resume with a blank disc the burn succeeds.
func TestBurnNonWritableDiscPauses(t *testing.T) {
	cfg := burnConfig(1, []string{"/dev/sr0"}, false, 3600)
	env := newBurnPauseEnv(t)

	var (
		mu          sync.Mutex
		burns       int
		pauseAlerts int
	)

	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnDiscInput) (BurnResult, error) {
			mu.Lock()
			defer mu.Unlock()

			burns++

			if burns == 1 {
				// The loaded disc is non-blank and cannot be written: the activity
				// returns the typed operator-pause error, touching nothing.
				return BurnResult{}, discNotWritableError(input.Device, 0, false, false)
			}

			return BurnResult{Device: input.Device}, nil
		})
	env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ BurnPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			return nil
		})

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, pauseAlerts, "a non-writable disc pauses for the operator")
	assert.Equal(t, 2, burns, "the burn is retried on resume after a blank disc is loaded")
}

// TestBurnStaleResumeSignalDoesNotSatisfyLaterPause covers issue #154 AC1: a
// surplus resume signal buffered during an earlier pause must NOT instantly satisfy
// a later pause. A 3-copy/2-drive run fails one disc in set 0 (a within-set pause);
// during that pause the operator sends TWO resume signals. The first resumes set 0;
// with the fix the surplus is drained at the entry of the between-set swap pause, so
// that pause waits for a fresh action and — none arriving — times out. Without the
// drain the stale signal would satisfy the swap pause and the run would complete.
func TestBurnStaleResumeSignalDoesNotSatisfyLaterPause(t *testing.T) {
	cfg := burnConfig(3, []string{"/dev/sr0", "/dev/sr1"}, false, 300)
	env := newBurnPauseEnv(t)

	var (
		mu         sync.Mutex
		burnsByDev = map[string]int{}
		swapAlerts int
		failAlerts int
	)

	// /dev/sr1 fails its first burn (within-set failure pause), then succeeds on the
	// resume retry; /dev/sr0 always succeeds.
	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnDiscInput) (BurnResult, error) {
			mu.Lock()
			defer mu.Unlock()

			burnsByDev[input.Device]++

			if input.Device == "/dev/sr1" && burnsByDev[input.Device] == 1 {
				return BurnResult{}, errors.New("optical burn failed: drive reported a write error")
			}

			return BurnResult{Device: input.Device}, nil
		})
	env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(nil)

	env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			if isBurnFailurePause(input) {
				failAlerts++
			} else {
				swapAlerts++
			}

			return nil
		})

	// During the set-0 within-set failure pause the operator sends TWO resume
	// signals (a double `tapectl resume`). The first clears that pause; the surplus
	// stays buffered and must not leak forward to the between-set swap pause.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "the surplus resume must not satisfy the swap pause; it times out")
	assert.Contains(t, err.Error(), "did not resume or abort")
	assert.Contains(t, err.Error(), "disc-set 2", "the run stalls at the between-set swap pause")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, failAlerts, "set 0's within-set failure pauses once")
	assert.Equal(t, 1, swapAlerts, "the between-set swap pause is reached and alerts once")
	// Set 1 (disc-set 2) never burns: the swap pause was not satisfied by the stale
	// signal, so /dev/sr0 burns only its set-0 copy.
	assert.Equal(t, 1, burnsByDev["/dev/sr0"], "set 1 never burns; no disc-set-2 copy")
}

// TestBurnForgotSwapDoesNotOverwriteVerifiedCopy covers issue #154 AC2: with
// allowNonBlankDiscs enabled on rewritable media, a reused drive whose just-verified
// copy is still loaded (the operator resumed the between-set swap without swapping
// that drive's disc) must pause for a fresh blank rather than blank and overwrite the
// copy — so the run never reports more burned copies than distinct physical discs.
func TestBurnForgotSwapDoesNotOverwriteVerifiedCopy(t *testing.T) {
	// 3 copies, 2 drives, opt-in ON: set 0 = {sr0:copy0, sr1:copy1}, set 1 = {sr0:copy2}.
	cfg := burnConfig(3, []string{"/dev/sr0", "/dev/sr1"}, true, 300)
	env := newBurnPauseEnv(t)

	var (
		mu         sync.Mutex
		burns      int
		reclaims   int
		verified   int
		guardPause int
	)

	// The mocked activity stands in for the real BurnDisc: it drives the loaded
	// disc's state through the true decideBurn. A drive that already verified a copy
	// this run (DriveHasVerifiedCopy) still has that non-blank rewritable disc loaded
	// (operator forgot to swap it); any other drive holds a fresh blank.
	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnDiscInput) (BurnResult, error) {
			mu.Lock()
			defer mu.Unlock()

			burns++

			state := optical.StateBlank
			if input.DriveHasVerifiedCopy {
				state = optical.StateNonBlankRewritable // this run's own copy, still loaded
			}

			action, decErr := decideBurn(state, input.AllowNonBlankDiscs, input.DriveHasVerifiedCopy)
			require.NoError(t, decErr)

			if action == burnPause {
				return BurnResult{}, discNotWritableError(input.Device, state, input.AllowNonBlankDiscs, input.DriveHasVerifiedCopy)
			}

			if action == burnReclaimWrite {
				reclaims++
			}

			verified++

			return BurnResult{Device: input.Device, OverwroteNonBlank: action == burnReclaimWrite}, nil
		})
	env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(nil)

	env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			if isBurnFailurePause(input) {
				// A failure pause naming the reused drive: the guard refused to
				// overwrite the verified copy.
				assert.Equal(t, []string{"/dev/sr0"}, input.Devices)
				assert.Contains(t, input.ErrorSummary, "already burned and verified")

				guardPause++
			}

			return nil
		})

	// t=30s: resume the between-set swap pause (set 1 starts, but /dev/sr0 still holds
	// its verified copy0). t=90s: abort after the guard pause fires.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorAbortSignal, nil)
	}, 90*time.Second)

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "the run ends aborted after the guard refuses the overwrite")
	assert.Contains(t, err.Error(), "aborted by operator")

	// The workflow returns an error (aborted), so its result value is discarded; the
	// mock's counters record what physically happened instead.
	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, guardPause, "the reused drive's verified copy triggers exactly one guard pause")
	assert.Equal(t, 2, verified, "only the two distinct set-0 discs are burned+verified")
	assert.Equal(t, 0, reclaims, "the run's own verified copy is never blanked/reclaimed (issue #154)")
	assert.GreaterOrEqual(t, burns, 3, "set 0 burns twice, set 1 attempts the reused drive at least once")
}

// TestBurnPauseAlertBestEffort covers AC9: when the pause alert delivery itself
// fails, the failure never masks or aborts the run — the run still pauses, resumes,
// and completes.
func TestBurnPauseAlertBestEffort(t *testing.T) {
	cfg := burnConfig(1, []string{"/dev/sr0"}, false, 3600)
	env := newBurnPauseEnv(t)

	var (
		mu    sync.Mutex
		burns int
	)

	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input BurnDiscInput) (BurnResult, error) {
			mu.Lock()
			defer mu.Unlock()

			burns++

			if burns == 1 {
				return BurnResult{}, errors.New("optical burn failed")
			}

			return BurnResult{Device: input.Device}, nil
		})
	env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(nil)

	// The alert delivery fails; it must be logged, not propagated.
	env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(
		errors.New("discord webhook unreachable"))

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "a failed pause alert must not abort the run")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 2, burns, "the run still resumed and re-burned the failed disc")
}

// burnPhaseTestWorkflow drives the top-level burnPhase gate so its disabled no-op
// path can be exercised in the workflow test environment.
func burnPhaseTestWorkflow(ctx workflow.Context, cfg config.Config) error {
	state := &runState{uncompressedISOPath: stagedISO, discManifestPath: stagedManifest}

	return burnPhase(ctx, cfg, state)
}

// TestBurnPhaseDisabledIsNoOp covers the state a tapectl --dry-run produces: when
// optical burning is disabled (OpticalBurn nil / not Enabled), burnPhase is a no-op
// — the run reaches a defined end state (the workflow completes with no error) and
// no burner activity is ever dispatched, so a dry-run whose burn section was
// neutralized never drives real hardware.
func TestBurnPhaseDisabledIsNoOp(t *testing.T) {
	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(burnPhaseTestWorkflow)
	env.RegisterActivity(newBurnActivities())

	// A config with no optical-burn section — exactly what applyDryRun leaves behind.
	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ BurnDiscInput) (BurnResult, error) {
			require.FailNow(t, "burnPhase must not burn when optical burning is disabled")

			return BurnResult{}, nil
		})

	env.ExecuteWorkflow(burnPhaseTestWorkflow, config.Config{})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}
