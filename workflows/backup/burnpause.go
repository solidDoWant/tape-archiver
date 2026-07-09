package backup

import (
	"errors"
	"fmt"

	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// The Burn phase's operator pause reuses the same signals and pauseOutcome as the
// tape write path (writepause.go): OperatorResumeSignal / OperatorAbortSignal and
// pauseResumed / pauseAborted / pauseTimedOut. Two situations pause the Burn
// phase, both waiting on the same signals: a burn/verify failure within a set
// (retry the failed discs on resume) and a between-set disc swap (no optical
// autoloader, so every later set is a manual load). The wait mirrors
// waitForWritePathCleared, swapping in the optical-burn operator-wait timeout.

// waitForBurnOperator alerts the operator to a Burn-phase pause and waits for
// their decision (SPEC §10, §11). cause is the burn/verify failure for a
// within-set pause, or nil for a between-set disc swap; devices names the burners
// the operator loads fresh blanks into. It fires the best-effort pause alert,
// then blocks on resume, abort, or the configured burn-wait timeout, returning
// which fired.
//
// state.currentPause (read by CurrentPauseQuery) is set to PauseBurn for the
// duration of the wait and cleared as soon as it returns — a plain struct-field
// assignment around the pre-existing wait call, with no effect on its timing or
// signal handling.
func waitForBurnOperator(ctx workflow.Context, cfg config.Config, state *runState, devices []string, cause error) pauseOutcome {
	// Drain before the alert is dispatched so only genuinely-stale (pre-alert)
	// resumes are discarded and a resume prompted by this alert survives (issue #216).
	drainStaleResumeSignals(ctx)

	notifyBurnPause(ctx, devices, cause)

	errSummary := ""
	if cause != nil {
		errSummary = cause.Error()
	}

	state.currentPause = CurrentPause{
		Kind:         PauseBurn,
		Devices:      devices,
		ErrorSummary: errSummary,
	}

	outcome := waitForBurnCleared(ctx, cfg)

	state.currentPause = CurrentPause{}

	return outcome
}

// waitForBurnCleared blocks until the operator resumes or aborts the run, or the
// configured burn-wait timeout elapses (SPEC §10). Like the write-path pause and
// unlike the Eject pause there is no library state to poll, so resume is always an
// explicit operator signal (there is no optical autoloader).
func waitForBurnCleared(ctx workflow.Context, cfg config.Config) pauseOutcome {
	resumeCh := workflow.GetSignalChannel(ctx, OperatorResumeSignal)
	abortCh := workflow.GetSignalChannel(ctx, OperatorAbortSignal)
	timeoutTimer := workflow.NewTimer(ctx, cfg.Delivery.OpticalBurn.EffectiveBurnWaitTimeout())

	outcome := pauseTimedOut

	selector := workflow.NewSelector(ctx)
	selector.AddReceive(resumeCh, func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, nil)

		outcome = pauseResumed
	})
	selector.AddReceive(abortCh, func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, nil)

		outcome = pauseAborted
	})
	selector.AddFuture(timeoutTimer, func(workflow.Future) {
		outcome = pauseTimedOut
	})

	selector.Select(ctx)

	return outcome
}

// notifyBurnPause runs the best-effort burn-pause alert activity on the control
// worker (SPEC §11). Like notifyWritePathPause it never propagates an error: a
// dispatch failure is logged so a webhook outage never aborts a paused run.
func notifyBurnPause(ctx workflow.Context, devices []string, cause error) {
	summary := "the burn-set is complete; load fresh blank recovery discs for the next set"
	if cause != nil {
		summary = "a burn or verify failed: " + cause.Error()
	}

	input := BurnPauseInput{
		RunID:        workflow.GetInfo(ctx).WorkflowExecution.ID,
		Devices:      devices,
		ErrorSummary: summary,
	}

	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           TaskQueue,
		StartToCloseTimeout: operatorPauseAlertTimeout,
	})

	var acts *FailureActivities
	if err := workflow.ExecuteActivity(actx, acts.NotifyBurnPause, input).Get(actx, nil); err != nil {
		workflow.GetLogger(ctx).Error("failed to deliver burn pause alert",
			"devices", devices,
			"error", err,
		)
	}
}

// joinFailedDiscs renders the per-disc burn/verify failures into one error for
// the operator alert and the aborted/timed-out run result.
func joinFailedDiscs(failed []failedDisc) error {
	errs := make([]error, 0, len(failed))
	for _, disc := range failed {
		errs = append(errs, fmt.Errorf("disc copy %d (drive %s): %w", disc.CopyIndex, disc.Device, disc.Err))
	}

	return errors.Join(errs...)
}
