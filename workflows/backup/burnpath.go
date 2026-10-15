package backup

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// The Burn phase (SPEC §10) is the optical analogue of the tape drive-set loop.
// There is no optical autoloader, so every burn-set after the first needs a
// manual disc swap plus `tapectl resume`.

const (
	// burnDiscTimeout: M-DISC writes are slow (often 4×–8×).
	burnDiscTimeout   = 1 * time.Hour
	verifyDiscTimeout = 30 * time.Minute
)

// burnAssignment assigns one recovery-disc copy to a burner drive within a set.
type burnAssignment struct {
	Device    string
	CopyIndex int
}

// burnSet is one set of discs burned in parallel, one per burner.
type burnSet []burnAssignment

// failedDisc is a disc whose burn or verify failed, kept so a resume re-drives
// only the failures.
type failedDisc struct {
	Device    string
	CopyIndex int
	Err       error
}

// planBurnSets partitions copies into successive burn-sets of at most len(drives)
// discs, disc j assigned to drives[j]. Zero copies returns (nil, nil).
func planBurnSets(copies int, drives []string) ([]burnSet, error) {
	if copies == 0 {
		return nil, nil
	}

	if copies < 0 {
		return nil, fmt.Errorf("optical burn plan has %d copies; must not be negative", copies)
	}

	if len(drives) == 0 {
		return nil, fmt.Errorf("no optical burners configured; cannot burn %d disc(s)", copies)
	}

	var (
		sets    []burnSet
		current burnSet
	)

	for copyIndex := 0; copyIndex < copies; copyIndex++ {
		current = append(current, burnAssignment{Device: drives[len(current)], CopyIndex: copyIndex})

		if len(current) == len(drives) {
			sets = append(sets, current)
			current = nil
		}
	}

	if len(current) > 0 {
		sets = append(sets, current)
	}

	return sets, nil
}

// burnPhase burns every configured copy, then re-renders the delivered report so
// it records the discs (the on-disc copy predates the burn). No-op when optical
// burning is disabled.
func burnPhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	if !cfg.Delivery.OpticalBurn.Enabled() {
		return nil
	}

	if err := runBurnPath(ctx, cfg, state); err != nil {
		return err
	}

	return rebuildDeliveredReport(ctx, cfg, state)
}

// runBurnPath burns each burn-set in turn, pausing before every set after the
// first for a manual disc swap.
func runBurnPath(ctx workflow.Context, cfg config.Config, state *runState) error {
	burn := cfg.Delivery.OpticalBurn

	sets, err := planBurnSets(burn.Copies, burn.Drives)
	if err != nil {
		return err
	}

	for setIndex, set := range sets {
		if setIndex > 0 {
			outcome, cancelErr := waitForBurnOperator(ctx, cfg, state, devicesOf(set), nil)
			if cancelErr != nil {
				return cancelErr
			}

			switch outcome {
			case pauseResumed:
			case pauseAborted:
				return fmt.Errorf("run aborted by operator before burning disc-set %d of %d", setIndex+1, len(sets))
			case pauseTimedOut:
				return fmt.Errorf("operator did not resume or abort within %s before burning disc-set %d of %d",
					burn.EffectiveBurnWaitTimeout(), setIndex+1, len(sets))
			}
		}

		if err := runBurnSet(ctx, cfg, state, set); err != nil {
			return err
		}
	}

	return nil
}

// runBurnSet burns and verifies one set, pausing for the operator on failure and
// re-driving only the failed discs on resume, never a disc that already verified.
func runBurnSet(ctx workflow.Context, cfg config.Config, state *runState, set burnSet) error {
	pending := set

	for {
		results, failed := burnDiscSet(ctx, cfg, state, pending)

		state.burnedDiscs = append(state.burnedDiscs, results...)

		if len(failed) == 0 {
			return nil
		}

		cause := joinFailedDiscs(failed)

		outcome, cancelErr := waitForBurnOperator(ctx, cfg, state, devicesOfFailed(failed), cause)
		if cancelErr != nil {
			return cancelErr
		}

		switch outcome {
		case pauseResumed:
			pending = retryBurnSet(failed)

			continue
		case pauseAborted:
			return fmt.Errorf("run aborted by operator after %d disc(s) failed to burn: %w", len(failed), cause)
		case pauseTimedOut:
			return fmt.Errorf("operator did not resume or abort within %s after %d disc(s) failed to burn: %w",
				cfg.Delivery.OpticalBurn.EffectiveBurnWaitTimeout(), len(failed), cause)
		}
	}
}

// burnDiscSet burns and verifies every disc in set in parallel and partitions the
// outcome into successes and failures; failures are returned, not raised.
func burnDiscSet(ctx workflow.Context, cfg config.Config, state *runState, set burnSet) ([]BurnResult, []failedDisc) {
	allowNonBlank := cfg.Delivery.OpticalBurn.AllowNonBlankDiscs

	// MaximumAttempts=1: a physical failure needs an operator disc swap; a retry
	// would only waste a write-once disc.
	burnOpts := workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: burnDiscTimeout,
		HeartbeatTimeout:    activityHeartbeatTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	verifyOpts := workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: verifyDiscTimeout,
		HeartbeatTimeout:    activityHeartbeatTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}

	type discResult struct {
		index  int
		result BurnResult
		err    error
	}

	ch := workflow.NewBufferedChannel(ctx, len(set))

	for i, assignment := range set {
		// Never reclaim a disc in a drive that already verified a copy this run
		// (issue #154); only true for a drive reused across sets.
		driveHasVerifiedCopy := deviceHasVerifiedCopy(state.burnedDiscs, assignment.Device)

		workflow.Go(ctx, func(gctx workflow.Context) {
			res := discResult{index: i}

			burnCtx := workflow.WithActivityOptions(gctx, burnOpts)
			verifyCtx := workflow.WithActivityOptions(gctx, verifyOpts)

			var acts *BurnActivities

			var burn BurnResult
			if err := workflow.ExecuteActivity(burnCtx, acts.BurnDisc, BurnDiscInput{
				Device:               assignment.Device,
				ISOPath:              state.uncompressedISOPath,
				AllowNonBlankDiscs:   allowNonBlank,
				DriveHasVerifiedCopy: driveHasVerifiedCopy,
			}).Get(burnCtx, &burn); err != nil {
				res.err = fmt.Errorf("burn: %w", err)
				ch.Send(gctx, res)

				return
			}

			if err := workflow.ExecuteActivity(verifyCtx, acts.VerifyDisc, VerifyDiscInput{
				Device:       assignment.Device,
				ManifestPath: state.discManifestPath,
			}).Get(verifyCtx, nil); err != nil {
				res.err = fmt.Errorf("verify: %w", err)
				ch.Send(gctx, res)

				return
			}

			res.result = burn
			ch.Send(gctx, res)
		})
	}

	var (
		results []BurnResult
		failed  []failedDisc
	)

	for range set {
		var res discResult
		ch.Receive(ctx, &res)

		assignment := set[res.index]

		if res.err != nil {
			failed = append(failed, failedDisc{Device: assignment.Device, CopyIndex: assignment.CopyIndex, Err: res.err})
		} else {
			results = append(results, res.result)
		}
	}

	return results, failed
}

// rebuildDeliveredReport re-renders the delivered report.pdf to record the burned
// discs; the ISO's pre-burn copy is untouched.
func rebuildDeliveredReport(ctx workflow.Context, cfg config.Config, state *runState) error {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: reportTimeout,
		HeartbeatTimeout:    activityHeartbeatTimeout,
	})

	var activities *ReportActivities

	input := reportInput(ctx, cfg, state)
	input.Discs = state.burnedDiscs

	var reportPath string
	if err := workflow.ExecuteActivity(dataCtx, activities.RebuildDeliveredReport, input).Get(dataCtx, &reportPath); err != nil {
		return err
	}

	state.reportPath = reportPath

	return nil
}

// deviceHasVerifiedCopy reports whether burned holds a verified copy from device
// (the issue #154 drive guard).
func deviceHasVerifiedCopy(burned []BurnResult, device string) bool {
	for _, disc := range burned {
		if disc.Device == device {
			return true
		}
	}

	return false
}

// retryBurnSet re-drives only the failed discs, keeping their drive and copy.
func retryBurnSet(failed []failedDisc) burnSet {
	set := make(burnSet, 0, len(failed))
	for _, disc := range failed {
		set = append(set, burnAssignment{Device: disc.Device, CopyIndex: disc.CopyIndex})
	}

	return set
}

// devicesOf lists the burner devices a burn-set uses, for the disc-swap alert.
func devicesOf(set burnSet) []string {
	out := make([]string, 0, len(set))
	for _, assignment := range set {
		out = append(out, assignment.Device)
	}

	return out
}

// devicesOfFailed lists the burner devices of the failed discs.
func devicesOfFailed(failed []failedDisc) []string {
	out := make([]string, 0, len(failed))
	for _, disc := range failed {
		out = append(out, disc.Device)
	}

	return out
}
