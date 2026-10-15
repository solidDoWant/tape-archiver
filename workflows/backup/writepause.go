package backup

import (
	"errors"
	"fmt"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// failedTape pairs a tape whose write failed with the error that stopped it.
type failedTape struct {
	Tape LoadedTape
	Err  error
}

// pauseOutcome is how a write-path operator pause ended.
type pauseOutcome int

const (
	// pauseResumed: re-drive the failed tapes onto fresh blanks.
	pauseResumed pauseOutcome = iota
	// pauseAborted: end the run with no further tapes written.
	pauseAborted
	// pauseTimedOut: the operator did not respond in time; the run fails.
	pauseTimedOut
)

// runDriveSet runs one drive-set through Load, Write and Eject, pausing for the
// operator on a Load or Write failure instead of failing the run (SPEC §4.3).
// After a Write failure only the failed (tape, copy) pairs are re-driven onto
// fresh blanks; recorded tapes are never re-formatted because Load's blank check
// gates that, so the blast radius is just the failed tapes.
func runDriveSet(ctx workflow.Context, cfg config.Config, state *runState, set driveSet, failingPhase *string) error {
	pending := set

	for {
		*failingPhase = PhaseLoad

		loaded, err := loadPhase(ctx, cfg, pending)
		if err != nil {
			var affected []string

			reload := slotsOf(pending)
			if barcode, slot, ok := nonBlankTape(err); ok {
				affected, reload = []string{barcode}, []int{slot}
			}

			switch waitForOperator(ctx, cfg, state, PhaseLoad, affected, reload, err) {
			case pauseResumed:
				continue
			case pauseAborted:
				return fmt.Errorf("run aborted by operator after Load failed for a drive-set: %w", err)
			case pauseTimedOut:
				return fmt.Errorf("operator did not resume or abort within %s after Load failed for a drive-set: %w",
					cfg.Library.EffectiveWriteFailureWaitTimeout(), err)
			}
		}

		state.loaded = loaded
		state.lastCompletedPhase = PhaseLoad

		// Recorded at load, not write, so a tape that fails mid-write still has its
		// barcode-keyed staging reclaimed on exit (SPEC §4.3).
		for _, loadedTape := range loaded {
			state.mountedBarcodes = append(state.mountedBarcodes, loadedTape.Barcode)
		}

		*failingPhase = PhaseWrite

		written, failed, err := writePhase(ctx, cfg, state, loaded)
		if err != nil {
			// An orchestration fault that touched no tape: fail rather than pause.
			return err
		}

		// Eject the failed tapes too, freeing their drives and slots for fresh
		// blanks; only the written ones are recorded.
		*failingPhase = PhaseEject

		if err := ejectPhase(ctx, cfg, state, append(ejectProjection(written), failedAsWritten(failed)...)); err != nil {
			return err
		}

		state.written = append(state.written, written...)

		if len(failed) == 0 {
			state.lastCompletedPhase = PhaseEject

			return nil
		}

		*failingPhase = PhaseWrite
		cause := joinFailed(failed)

		switch waitForOperator(ctx, cfg, state, PhaseWrite, barcodesOfFailed(failed), reloadSlots(failed), cause) {
		case pauseResumed:
			pending = retrySet(cfg, failed)

			continue
		case pauseAborted:
			return fmt.Errorf("run aborted by operator after %d tape(s) failed to write: %w", len(failed), cause)
		case pauseTimedOut:
			return fmt.Errorf("operator did not resume or abort within %s after %d tape(s) failed to write: %w",
				cfg.Library.EffectiveWriteFailureWaitTimeout(), len(failed), cause)
		}
	}
}

// waitForOperator alerts the operator to a Load/Write-failure pause and waits
// for resume, abort or the wait-timeout, exposing the pause via
// state.currentPause meanwhile.
func waitForOperator(ctx workflow.Context, cfg config.Config, state *runState, phase string, affectedBarcodes []string, reloadSlots []int, cause error) pauseOutcome {
	// Drain before the alert: buffered signals predate it and are stale, while a
	// reply to the alert lands afterwards and survives (#216, #254).
	drainStalePauseSignals(ctx)

	notifyWritePathPause(ctx, phase, affectedBarcodes, reloadSlots, cause)

	errSummary := ""
	if cause != nil {
		errSummary = cause.Error()
	}

	state.currentPause = CurrentPause{
		Kind:          PauseWriteFailure,
		Phase:         phase,
		AffectedTapes: affectedBarcodes,
		ReloadSlots:   reloadSlots,
		ErrorSummary:  errSummary,
	}

	outcome := waitForWritePathCleared(ctx, cfg)

	state.currentPause = CurrentPause{}

	return outcome
}

// waitForWritePathCleared blocks until resume, abort, or the wait-timeout
// (SPEC §4.3). There is no station to poll, so resume is always explicit.
func waitForWritePathCleared(ctx workflow.Context, cfg config.Config) pauseOutcome {
	resumeCh := workflow.GetSignalChannel(ctx, OperatorResumeSignal)
	abortCh := workflow.GetSignalChannel(ctx, OperatorAbortSignal)
	timeoutTimer := workflow.NewTimer(ctx, cfg.Library.EffectiveWriteFailureWaitTimeout())

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

// drainStalePauseSignals discards resume/abort signals already buffered when a
// pause begins. Temporal buffers unconsumed signals indefinitely, so a duplicate
// or late resume/abort from an earlier pause would otherwise instantly resolve a
// later, unrelated one (#154, #254).
//
// It must run before the pause's alert is dispatched, never at wait entry: the
// alert and the wait run in different workflow tasks, so a legitimate reply to
// the alert can be buffered before the wait begins and would be lost (#216).
// The Eject pause still never listens for abort, so draining abort here does not
// change which pauses accept it.
//
// Not gated by workflow.GetVersion: in-flight histories that replayed past this
// point with a buffered abort may hit a non-determinism error, accepted for this
// dev-stage project (same precedent as #216).
func drainStalePauseSignals(ctx workflow.Context) {
	resumeCh := workflow.GetSignalChannel(ctx, OperatorResumeSignal)
	for resumeCh.ReceiveAsync(nil) {
	}

	abortCh := workflow.GetSignalChannel(ctx, OperatorAbortSignal)
	for abortCh.ReceiveAsync(nil) {
	}
}

// notifyWritePathPause sends the best-effort write-path pause alert (SPEC §11).
// Failures are only logged so a webhook outage never aborts a paused run.
func notifyWritePathPause(ctx workflow.Context, phase string, affectedBarcodes []string, reloadSlots []int, cause error) {
	summary := "unknown error"
	if cause != nil {
		summary = cause.Error()
	}

	input := WritePathPauseInput{
		RunID:         workflow.GetInfo(ctx).WorkflowExecution.ID,
		Phase:         phase,
		AffectedTapes: affectedBarcodes,
		ReloadSlots:   reloadSlots,
		ErrorSummary:  summary,
	}

	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           TaskQueue,
		StartToCloseTimeout: operatorPauseAlertTimeout,
	})

	var acts *FailureActivities
	if err := workflow.ExecuteActivity(actx, acts.NotifyWritePathPause, input).Get(actx, nil); err != nil {
		workflow.GetLogger(ctx).Error("failed to deliver write-path pause alert",
			"phase", phase,
			"error", err,
		)
	}
}

// retrySet builds the drive-set that re-drives only the failed tapes, reusing
// each one's source slot as the blank slot (the operator restocked it). It
// carries DriveIndex explicitly because the narrowed set is not a 0-based prefix
// of the library's drives (#137).
func retrySet(cfg config.Config, failed []failedTape) driveSet {
	set := make(driveSet, 0, len(failed))
	for _, tape := range failed {
		set = append(set, TapeAssignment{
			Drive:      cfg.Library.Drives[tape.Tape.DriveIndex],
			DriveIndex: tape.Tape.DriveIndex,
			BlankSlot:  tape.Tape.SourceSlot,
			TapeIndex:  tape.Tape.TapeIndex,
			CopyIndex:  tape.Tape.CopyIndex,
		})
	}

	return set
}

// failedAsWritten adapts failed tapes into the WrittenTape shape Eject needs.
// They are never recorded as written.
func failedAsWritten(failed []failedTape) []WrittenTape {
	out := make([]WrittenTape, 0, len(failed))
	for _, tape := range failed {
		out = append(out, WrittenTape{
			Barcode:    tape.Tape.Barcode,
			DriveIndex: tape.Tape.DriveIndex,
			TapeIndex:  tape.Tape.TapeIndex,
			CopyIndex:  tape.Tape.CopyIndex,
			SourceSlot: tape.Tape.SourceSlot,
		})
	}

	return out
}

// ejectProjection returns only the WrittenTape fields Eject reads, keeping the
// EjectInput payload bounded regardless of run size (#221).
func ejectProjection(written []WrittenTape) []WrittenTape {
	out := make([]WrittenTape, 0, len(written))
	for _, tape := range written {
		out = append(out, WrittenTape{
			Barcode:    tape.Barcode,
			DriveIndex: tape.DriveIndex,
			TapeIndex:  tape.TapeIndex,
			CopyIndex:  tape.CopyIndex,
			SourceSlot: tape.SourceSlot,
		})
	}

	return out
}

// reloadSlots lists the source slots the operator must restock with blanks.
func reloadSlots(failed []failedTape) []int {
	out := make([]int, 0, len(failed))
	for _, tape := range failed {
		out = append(out, tape.Tape.SourceSlot)
	}

	return out
}

// barcodesOfFailed lists the barcodes of the failed tapes for the operator alert.
func barcodesOfFailed(failed []failedTape) []string {
	out := make([]string, 0, len(failed))
	for _, tape := range failed {
		out = append(out, string(tape.Tape.Barcode))
	}

	return out
}

// slotsOf lists a drive-set's blank slots, for Load-failure alerts.
func slotsOf(set driveSet) []int {
	out := make([]int, 0, len(set))
	for _, assignment := range set {
		out = append(out, assignment.BlankSlot)
	}

	return out
}

// joinFailed renders the per-tape write failures into one error.
func joinFailed(failed []failedTape) error {
	errs := make([]error, 0, len(failed))
	for _, tape := range failed {
		errs = append(errs, fmt.Errorf("tape %s (drive %d): %w", tape.Tape.Barcode, tape.Tape.DriveIndex, tape.Err))
	}

	return errors.Join(errs...)
}

// nonBlankTapeErrorType marks a Load refused because a tape was not blank; its
// details carry the barcode and the slot it was returned to.
const nonBlankTapeErrorType = "NonBlankTape"

// nonBlankTape returns the tape Load refused as non-blank, if that is why it failed.
func nonBlankTape(err error) (barcode string, slot int, ok bool) {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != nonBlankTapeErrorType {
		return "", 0, false
	}

	if appErr.Details(&barcode, &slot) != nil || barcode == "" {
		return "", 0, false
	}

	return barcode, slot, true
}
