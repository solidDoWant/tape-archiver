package backup

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

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
// returns the discs it recorded.
func burnPauseTestWorkflow(ctx workflow.Context, cfg config.Config) (burnPauseResult, error) {
	state := &runState{uncompressedISOPath: stagedISO, discManifestPath: stagedManifest}
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
				return BurnResult{}, discNotWritableError(input.Device, 0, false)
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
