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
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/optical"
)

// isBurnFailurePause reports whether a burn-pause alert is a within-set failure
// rather than a between-set disc swap.
func isBurnFailurePause(input BurnPauseInput) bool {
	return strings.Contains(input.ErrorSummary, "a burn or verify failed")
}

const (
	stagedISO      = "/staging/run/recovery.iso"
	stagedManifest = "/staging/run/disc-manifest.sha256"
)

// burnPauseResult surfaces the discs runBurnPath recorded.
type burnPauseResult struct {
	Burned []BurnResult
}

// burnPauseTestWorkflow drives runBurnPath with mocked activities and registers
// CurrentPauseQuery like the real Backup workflow.
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

// burnConfig builds a run config with the given optical-burn settings.
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

// newBurnPauseEnv registers the burn/verify and failure-alert activities.
func newBurnPauseEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()

	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(burnPauseTestWorkflow)
	env.RegisterActivity(newBurnActivities())
	env.RegisterActivity(&FailureActivities{})

	return env
}

// TestBurnSetFailurePauseResume: a failed disc pauses the run and only that disc
// is re-burned on resume.
func TestBurnSetFailurePauseResume(t *testing.T) {
	cfg := burnConfig(2, []string{"/dev/sr0", "/dev/sr1"}, false, 3600)
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

// TestBurnCurrentPauseQuery: CurrentPauseQuery reports PauseBurn while paused and
// nothing once the run completes.
func TestBurnCurrentPauseQuery(t *testing.T) {
	cfg := burnConfig(2, []string{"/dev/sr0", "/dev/sr1"}, false, 3600)
	env := newBurnPauseEnv(t)

	var (
		mu         sync.Mutex
		burnsByDev = map[string]int{}
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

// TestBurnResumeInAlertToWaitGapResumes (issue #216): a resume sent between the
// pause alert and the wait's start must resume the run, not be drained as stale.
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

	resumeWhilePauseAlertFires(env, "NotifyBurnPause")

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "a resume in the alert-to-wait gap must resume the run, not be drained")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, pauseAlerts, "the operator is alerted exactly once")
	assert.Equal(t, 2, burnsByDev["/dev/sr1"], "the failed disc is re-burned on the honored resume")
}

// TestBurnSingleSetAllVerified: copies within the burner count burn and verify in
// one set without pausing.
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

// TestBurnSetVerifyFailurePauseResume: a verify mismatch pauses and re-burns the
// disc on resume.
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

// TestBurnSetPauseAbort: an operator abort ends the run with no further burns.
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

// TestBurnSetPauseTimeout: with no operator response the burn-wait elapses and the
// run fails.
func TestBurnSetPauseTimeout(t *testing.T) {
	cfg := burnConfig(1, []string{"/dev/sr0"}, false, 100)
	env := newBurnPauseEnv(t)

	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		BurnResult{}, errors.New("optical burn failed"))
	env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "the run fails when the operator never responds")
	assert.Contains(t, err.Error(), "did not resume or abort")
}

// TestBurnBetweenSetSwapPause: copies beyond the burner count burn in successive
// sets with a disc-swap pause between them.
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

// TestBurnRecordsOverwrite: a reclaimed rewritable disc is recorded and does not
// pause the run.
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

// TestBurnNonWritableDiscPauses: a refused disc pauses the run instead of being
// overwritten, and the burn succeeds on resume.
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

// TestBurnStaleResumeSignalDoesNotSatisfyLaterPause (issue #154): a surplus resume
// from an earlier pause must not satisfy the later swap pause, which times out.
func TestBurnStaleResumeSignalDoesNotSatisfyLaterPause(t *testing.T) {
	cfg := burnConfig(3, []string{"/dev/sr0", "/dev/sr1"}, false, 300)
	env := newBurnPauseEnv(t)

	var (
		mu         sync.Mutex
		burnsByDev = map[string]int{}
		swapAlerts int
		failAlerts int
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

	// A double `tapectl resume` during the set-0 failure pause.
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
	assert.Equal(t, 1, burnsByDev["/dev/sr0"], "set 1 never burns; no disc-set-2 copy")
}

// TestBurnStaleAbortSignalDoesNotAbortLaterPause (issue #254): an abort racing in
// behind a resume must not abort the later swap pause, which times out instead.
func TestBurnStaleAbortSignalDoesNotAbortLaterPause(t *testing.T) {
	cfg := burnConfig(3, []string{"/dev/sr0", "/dev/sr1"}, false, 300)
	env := newBurnPauseEnv(t)

	var (
		mu         sync.Mutex
		burnsByDev = map[string]int{}
		swapAlerts int
		failAlerts int
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

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
		env.SignalWorkflow(OperatorAbortSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "the stale abort must not satisfy the swap pause; it times out")
	assert.Contains(t, err.Error(), "did not resume or abort")
	assert.Contains(t, err.Error(), "disc-set 2", "the run stalls at the between-set swap pause")
	assert.NotContains(t, err.Error(), "aborted", "the stale abort must not end the run at the swap pause")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, failAlerts, "set 0's within-set failure pauses once")
	assert.Equal(t, 1, swapAlerts, "the between-set swap pause is reached and alerts once")
	assert.Equal(t, 1, burnsByDev["/dev/sr0"], "set 1 never burns; no disc-set-2 copy")
}

// TestBurnForgotSwapDoesNotOverwriteVerifiedCopy (issue #154): even with
// allowNonBlankDiscs, a reused drive still holding its verified copy pauses rather
// than reclaiming it.
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

	// Runs the real decideBurn; a drive that verified a copy still holds it.
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
				assert.Equal(t, []string{"/dev/sr0"}, input.Devices)
				assert.Contains(t, input.ErrorSummary, "already burned and verified")

				guardPause++
			}

			return nil
		})

	// Resume the swap pause without swapping, then abort after the guard pause.
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

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, guardPause, "the reused drive's verified copy triggers exactly one guard pause")
	assert.Equal(t, 2, verified, "only the two distinct set-0 discs are burned+verified")
	assert.Equal(t, 0, reclaims, "the run's own verified copy is never blanked/reclaimed (issue #154)")
	assert.GreaterOrEqual(t, burns, 3, "set 0 burns twice, set 1 attempts the reused drive at least once")
}

// TestBurnPauseAlertBestEffort: a failed pause alert never aborts the run.
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

// burnPhaseTestWorkflow drives the top-level burnPhase gate.
func burnPhaseTestWorkflow(ctx workflow.Context, cfg config.Config) error {
	state := &runState{uncompressedISOPath: stagedISO, discManifestPath: stagedManifest}

	return burnPhase(ctx, cfg, state)
}

// TestBurnPhaseDisabledIsNoOp: with burning disabled (as a dry-run leaves it) no
// burner activity is dispatched, so a dry-run never drives real hardware.
func TestBurnPhaseDisabledIsNoOp(t *testing.T) {
	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(burnPhaseTestWorkflow)
	env.RegisterActivity(newBurnActivities())

	env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ BurnDiscInput) (BurnResult, error) {
			require.FailNow(t, "burnPhase must not burn when optical burning is disabled")

			return BurnResult{}, nil
		})

	env.ExecuteWorkflow(burnPhaseTestWorkflow, config.Config{})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

// TestBurnPauseCancelIsNotTimeout pins #388: cancelling during a burn-failure or
// between-set swap pause ends the run cancelled, not as a burn-wait timeout.
func TestBurnPauseCancelIsNotTimeout(t *testing.T) {
	tests := []struct {
		name      string
		copies    int
		burnFails bool
	}{
		{name: "burn failure", copies: 1, burnFails: true},
		{name: "between-set swap", copies: 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := burnConfig(test.copies, []string{"/dev/sr0"}, false, 3600)
			env := newBurnPauseEnv(t)

			var burnErr error
			if test.burnFails {
				burnErr = errors.New("optical burn failed")
			}

			env.OnActivity((&BurnActivities{}).BurnDisc, mock.Anything, mock.Anything).Return(BurnResult{}, burnErr)
			env.OnActivity((&BurnActivities{}).VerifyDisc, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity((&FailureActivities{}).NotifyBurnPause, mock.Anything, mock.Anything).Return(nil)

			env.RegisterDelayedCallback(env.CancelWorkflow, 30*time.Second)

			env.ExecuteWorkflow(burnPauseTestWorkflow, cfg)

			require.True(t, env.IsWorkflowCompleted())

			err := env.GetWorkflowError()
			require.Error(t, err)
			assert.True(t, temporal.IsCanceledError(err), "the run ends cancelled, got: %v", err)
			assert.NotContains(t, err.Error(), "did not resume or abort")
		})
	}
}
