package backup

import (
	"errors"
	"fmt"

	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// waitForBurnOperator alerts the operator to a Burn-phase pause and waits for
// resume, abort, or the burn-wait timeout. cause is nil for a between-set disc swap.
func waitForBurnOperator(ctx workflow.Context, cfg config.Config, state *runState, devices []string, cause error) pauseOutcome {
	// Drain before the alert is dispatched so only genuinely-stale (pre-alert)
	// resumes/aborts are discarded and a resume or abort prompted by this alert
	// survives (issues #216, #254).
	drainStalePauseSignals(ctx)

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

// waitForBurnCleared blocks until the operator resumes or aborts, or the burn-wait
// timeout elapses. There is no library state to poll, so resume is always explicit.
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

// notifyBurnPause sends the best-effort burn-pause alert; failures are only logged
// so a webhook outage never aborts a paused run.
func notifyBurnPause(ctx workflow.Context, devices []string, cause error) {
	summary := "the burn-set is complete; load fresh blank recovery discs for the next set"
	if cause != nil {
		summary = "a burn or verify failed: " + cause.Error()
	}

	input := BurnPauseInput{
		DryRun:       isDryRun(ctx),
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

// joinFailedDiscs joins the per-disc failures into one error.
func joinFailedDiscs(failed []failedDisc) error {
	errs := make([]error, 0, len(failed))
	for _, disc := range failed {
		errs = append(errs, fmt.Errorf("disc copy %d (drive %s): %w", disc.CopyIndex, disc.Device, disc.Err))
	}

	return errors.Join(errs...)
}
