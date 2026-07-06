package backup

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// The Burn phase (SPEC §10) is the optical analogue of the tape Load/Write/Eject
// drive-set loop (runTapePath/runDriveSet). It partitions the configured disc
// copies into burn-sets of at most len(Drives) discs, burns and independently
// verifies each set in parallel, and runs the operator disc-swap / failure
// pause-resume-abort loop between and within sets. Unlike the tape changer there
// is no optical autoloader, so every set after the first requires a manual disc
// swap plus `tapectl resume`. It is deterministic control-worker orchestration
// over the already-landed per-disc BurnDisc/VerifyDisc activities (burn.go);
// burning is a no-op when Delivery.OpticalBurn is disabled.

const (
	// burnDiscTimeout bounds a single BurnDisc activity: reading disc state, an
	// optional reclaim (blanking a rewritable disc), and streaming the tens-of-MB
	// ISO to optical media. M-DISC writes run slow (often 4×–8×), so an hour is a
	// generous ceiling while still bounding a hang. It carries the liveness
	// HeartbeatTimeout so a data-worker restart mid-burn is caught quickly.
	burnDiscTimeout = 1 * time.Hour
	// verifyDiscTimeout bounds a single VerifyDisc activity: mounting the burned
	// disc read-only and hashing every file back against the disc-content manifest.
	// Tens of MB of reads complete in minutes; 30 minutes is a generous ceiling.
	verifyDiscTimeout = 30 * time.Minute
)

// burnAssignment assigns one recovery-disc copy to a burner drive within a set.
// It is the optical analogue of TapeAssignment, minus the changer/blank-slot
// bookkeeping (a disc is loaded by hand, not moved from a storage slot).
type burnAssignment struct {
	// Device is the optical burner device node the disc is burned on (e.g.
	// /dev/sr0), drawn from Delivery.OpticalBurn.Drives.
	Device string
	// CopyIndex is the 0-based copy number among Delivery.OpticalBurn.Copies.
	CopyIndex int
}

// burnSet is one set of discs burned in parallel — at most len(Drives) discs,
// one per burner (the optical analogue of driveSet).
type burnSet []burnAssignment

// failedDisc pairs a disc whose burn or verify failed with the error that
// stopped it, so the operator alert can name the drive and the run can re-drive
// only the failed discs on resume.
type failedDisc struct {
	// Device is the burner the disc failed on.
	Device string
	// CopyIndex is the disc's 0-based copy number.
	CopyIndex int
	// Err is the burn or verify failure.
	Err error
}

// planBurnSets partitions copies recovery discs into successive burn-sets of at
// most len(drives) discs, disc j in a set assigned to drives[j] (the optical
// analogue of planDriveSets). Copies is not bounded by the drive count: two
// burners and three copies burn as {disc0,disc1} then {disc2}. It returns
// (nil,nil) for zero copies (burning disabled). It returns an error when copies
// is negative or no drives are configured — states Delivery.OpticalBurn.Enabled()
// already excludes, checked defensively so a misconfiguration never burns.
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

// burnPhase orchestrates the Burn phase (SPEC §10). It is a no-op — the run
// completes exactly as before — when optical burning is not configured. Otherwise
// it burns every configured copy through the burn-set loop and then re-renders
// the delivered report so it records the burned discs and any deliberate
// overwrite (Report → Burn → re-render → Deliver): the on-disc report predates the
// burn and cannot record it, so the delivered copy is rebuilt from the full run
// state (SPEC §10). A burn failure the operator aborts (or lets time out) fails
// the run in its defined paused state, reported by the workflow's failure alert.
func burnPhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	if !cfg.Delivery.OpticalBurn.Enabled() {
		return nil
	}

	if err := runBurnPath(ctx, cfg, state); err != nil {
		return err
	}

	return rebuildDeliveredReport(ctx, cfg, state)
}

// runBurnPath drives the burn-set loop: it partitions the copies into burn-sets
// and burns each in turn. Every set after the first pauses first for the operator
// to load fresh blank discs and resume — there is no optical autoloader, so the
// swap is manual (SPEC §10). A between-set abort (or an elapsed operator wait)
// ends the run with no further discs burned; the successfully burned sets are
// already recorded in state.burnedDiscs.
func runBurnPath(ctx workflow.Context, cfg config.Config, state *runState) error {
	burn := cfg.Delivery.OpticalBurn

	sets, err := planBurnSets(burn.Copies, burn.Drives)
	if err != nil {
		return err
	}

	for setIndex, set := range sets {
		if setIndex > 0 {
			// Every set after the first needs a manual disc swap. Pause for the
			// operator to load fresh blanks into the set's drives, then resume.
			switch waitForBurnOperator(ctx, cfg, devicesOf(set), nil) {
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

// runBurnSet burns and verifies one set, pausing for the operator on any
// burn/verify failure rather than failing the whole run (SPEC §10) — the optical
// analogue of runDriveSet. It loops until every disc in the set is burned and
// verified or the operator aborts (or the wait elapses): each round burns the
// pending discs in parallel, records the successes, and on any failure pauses;
// on resume it re-drives only the failed discs (the operator has loaded fresh
// blanks in their drives), never re-burning a disc that already verified.
func runBurnSet(ctx workflow.Context, cfg config.Config, state *runState, set burnSet) error {
	pending := set

	for {
		results, failed := burnDiscSet(ctx, cfg, state, pending)

		state.burnedDiscs = append(state.burnedDiscs, results...)

		if len(failed) == 0 {
			return nil
		}

		cause := joinFailedDiscs(failed)

		switch waitForBurnOperator(ctx, cfg, devicesOfFailed(failed), cause) {
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

// burnDiscSet burns and verifies every disc in set in parallel — one Temporal
// coroutine per burner, so the set holds at most len(Drives) discs in flight at
// once — and partitions the outcome into the discs that burned+verified (with
// their BurnResult provenance for the report) and those that failed. A failure is
// any of: a non-writable disc (IsDiscNotWritable — a non-blank disc without the
// opt-in, or a write-once disc), a burn error, or a verify mismatch. Both
// activities carry MaximumAttempts=1: a burn is physical and its failure needs an
// operator decision, not a blind retry that would waste another disc. Like
// writePhase, a partial failure is not fatal here — the failures come back as
// failedDisc values for the caller to pause on, not as an error.
func burnDiscSet(ctx workflow.Context, cfg config.Config, state *runState, set burnSet) ([]BurnResult, []failedDisc) {
	allowNonBlank := cfg.Delivery.OpticalBurn.AllowNonBlankDiscs

	// MaximumAttempts=1 on both: a physical burn/verify failure needs an operator
	// decision (swap the disc), so retrying the same attempt cannot succeed and
	// would only waste a write-once disc. IsDiscNotWritable is already
	// non-retryable at the source.
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
		i, assignment := i, assignment

		workflow.Go(ctx, func(gctx workflow.Context) {
			res := discResult{index: i}

			burnCtx := workflow.WithActivityOptions(gctx, burnOpts)
			verifyCtx := workflow.WithActivityOptions(gctx, verifyOpts)

			var acts *BurnActivities

			var burn BurnResult
			if err := workflow.ExecuteActivity(burnCtx, acts.BurnDisc, BurnDiscInput{
				Device:             assignment.Device,
				ISOPath:            state.uncompressedISOPath,
				AllowNonBlankDiscs: allowNonBlank,
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

// rebuildDeliveredReport re-renders the delivered PDF report now that the discs
// are burned, so it records them and any deliberate overwrite, and points the
// Deliver phase at the re-rendered file (SPEC §10). Only the delivered report.pdf
// changes; the recovery ISO (with its pre-burn report copy) is untouched.
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

// retryBurnSet builds the narrowed burn-set that re-drives only the failed discs
// on resume, keeping each failed disc's burner and copy identity (the operator
// has loaded a fresh blank in that drive) — the optical analogue of retrySet.
func retryBurnSet(failed []failedDisc) burnSet {
	set := make(burnSet, 0, len(failed))
	for _, disc := range failed {
		set = append(set, burnAssignment{Device: disc.Device, CopyIndex: disc.CopyIndex})
	}

	return set
}

// devicesOf lists the burner devices a burn-set uses — named in the between-set
// disc-swap alert so the operator knows which drives to load fresh blanks into.
func devicesOf(set burnSet) []string {
	out := make([]string, 0, len(set))
	for _, assignment := range set {
		out = append(out, assignment.Device)
	}

	return out
}

// devicesOfFailed lists the burner devices of the failed discs, for the failure
// pause alert.
func devicesOfFailed(failed []failedDisc) []string {
	out := make([]string, 0, len(failed))
	for _, disc := range failed {
		out = append(out, disc.Device)
	}

	return out
}
