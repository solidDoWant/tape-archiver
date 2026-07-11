package backup

import (
	"errors"
	"fmt"

	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// failedTape pairs a tape whose write failed with the error that stopped it. The
// tape carries the drive, slot, and (logical tape, copy) provenance needed to
// report it to the operator and re-drive it onto a fresh blank on resume.
type failedTape struct {
	Tape LoadedTape
	Err  error
}

// pauseOutcome is how a write-path operator pause ended.
type pauseOutcome int

const (
	// pauseResumed: the operator signalled resume; the run re-drives the failed
	// tapes onto fresh blanks.
	pauseResumed pauseOutcome = iota
	// pauseAborted: the operator signalled abort; the run ends with no further
	// tapes written.
	pauseAborted
	// pauseTimedOut: the operator did not respond within the configured wait; the
	// run fails in its defined paused state.
	pauseTimedOut
)

// runDriveSet processes one drive-set through Load → Write → Eject, pausing for
// the operator on a Load or Write failure rather than failing the whole run
// (SPEC §4.3). It loops until the set completes or the operator aborts (or the
// wait elapses):
//
//   - Load failure: the whole set failed to load; the run pauses and, on resume,
//     retries the same set (the operator restores its blanks to their slots).
//   - Write failure: the tapes that wrote successfully are ejected and recorded;
//     the failed tapes are ejected too (freeing their drives and emptying their
//     slots), then the run pauses and, on resume, re-drives only the failed
//     (tape, copy) pairs onto fresh blanks in the same slots. Already-recorded
//     tapes are never re-formatted (the Load blank-check gates that, SPEC §4.3
//     step 6), so the blast radius is the failed tapes, not the whole set.
//
// It advances *failingPhase to the sub-phase in flight so a caller's failure
// alert names where the run stopped, and sets state.lastCompletedPhase to Eject
// only once the whole set is done.
func runDriveSet(ctx workflow.Context, cfg config.Config, state *runState, set driveSet, failingPhase *string) error {
	pending := set

	for {
		*failingPhase = PhaseLoad

		loaded, err := loadPhase(ctx, cfg, pending)
		if err != nil {
			// The whole set failed to load. Pause; on resume retry the same set.
			switch waitForOperator(ctx, cfg, state, PhaseLoad, nil, slotsOf(pending), err) {
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

		*failingPhase = PhaseWrite

		written, failed, err := writePhase(ctx, cfg, state, loaded)
		if err != nil {
			// An unrecoverable orchestration fault (e.g. session creation) that
			// touched no tape — fail the run rather than pause.
			return err
		}

		// Eject every tape that vacated a drive: the written tapes for physical
		// removal and the run record, and the failed tapes so their drives free
		// and their blank slots empty for fresh blanks. Only the written tapes are
		// recorded in the run.
		*failingPhase = PhaseEject

		if err := ejectPhase(ctx, cfg, state, append(ejectProjection(written), failedAsWritten(failed)...)); err != nil {
			return err
		}

		state.written = append(state.written, written...)

		if len(failed) == 0 {
			state.lastCompletedPhase = PhaseEject

			return nil
		}

		// Some tapes failed to write. Pause for the operator to swap the suspect
		// tapes for fresh blanks in the same slots, then re-drive just those.
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

// waitForOperator alerts the operator to a Load/Write-failure pause and waits for
// their decision. It fires the best-effort pause alert (SPEC §11), then blocks on
// the resume signal, the abort signal, or the configured wait-timeout, returning
// which one fired.
//
// state.currentPause (read by CurrentPauseQuery) is set to PauseWriteFailure for
// the duration of the wait and cleared as soon as it returns — a plain
// struct-field assignment around the pre-existing wait call, with no effect on
// its timing or signal handling.
func waitForOperator(ctx workflow.Context, cfg config.Config, state *runState, phase string, affectedBarcodes []string, reloadSlots []int, cause error) pauseOutcome {
	// Drain before the alert is dispatched: any resume or abort buffered now
	// predates this pause's alert and is therefore stale, while a resume or abort
	// the operator sends in response to the alert lands strictly afterwards and
	// survives (issues #216, #254).
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

// waitForWritePathCleared blocks until the operator resumes or aborts the run, or
// the configured wait-timeout elapses (SPEC §4.3). Unlike the Eject pause there
// is no station state to poll, so resume is always an explicit operator signal.
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

// drainStalePauseSignals discards every OperatorResumeSignal and
// OperatorAbortSignal already buffered at the moment an operator pause begins, so
// a stale resume or abort — a double `tapectl resume`/`abort`, one that raced an
// Eject auto-resume, or a web-API abort whose CurrentPauseQuery check and
// SignalWorkflow call straddled the pause resolving by other means — cannot
// instantly satisfy this pause (issues #154, #254). Temporal buffers unconsumed
// signals indefinitely, and both `tapectl resume`/`abort` and the web API's abort
// handler can deliver a signal after the pause it was meant for has already
// resolved, so without this drain a surplus signal from an earlier pause leaks
// forward onto a later, unrelated pause — resuming it (and blanking a
// just-verified disc between burn-sets) or, worse, silently aborting a run the
// operator never meant to abort there.
//
// It must be called *before this pause's alert is dispatched*, never at wait entry.
// The alert (an activity completion) and the wait selector run in different workflow
// tasks; a resume or abort the operator sends in response to the alert is appended
// to history after the alert but can be buffered ahead of the later task that begins
// the wait (during a control-worker restart/deploy or task-queue backlog). Draining
// at wait entry would discard that legitimate signal and fail an otherwise-
// recoverable run (issue #216), or silently swallow a deliberate abort. Draining
// before the alert instead makes the two cases disjoint: any signal buffered before
// the alert predates it and is stale; any signal prompted by the alert lands
// strictly afterwards and survives.
//
// Draining the abort channel here does not change which pauses *accept* abort: the
// Eject I/O-station pause's own wait (waitForIOCleared) still never listens on the
// abort channel (PauseEject rejects abort, enforced independently by
// pkg/runsapi.pauseAcceptsAbort), so a buffered abort still cannot resolve that
// pause — this only stops a stale one from surviving *past* the pause to land on a
// later, unrelated one. The drain is a deterministic ReceiveAsync loop (no history
// event), so it is a no-op on any pause with no stale signal and never changes the
// behavior of a single, correctly-timed resume or abort.
//
// Compatibility note: this changes workflow code at a location existing in-flight
// run histories may already have replayed past with a since-buffered, un-drained
// abort signal recorded (i.e. before this drain existed at that point in the code).
// Replaying such a run against the new binary can diverge in which pause consumes
// that signal and fail with a non-deterministic-workflow error. This mirrors the
// precedent set by issue #216's move of the resume drain (also shipped un-versioned)
// and is accepted here for this dev-stage project rather than gated behind
// workflow.GetVersion, which this codebase does not otherwise use.
func drainStalePauseSignals(ctx workflow.Context) {
	resumeCh := workflow.GetSignalChannel(ctx, OperatorResumeSignal)
	for resumeCh.ReceiveAsync(nil) {
	}

	abortCh := workflow.GetSignalChannel(ctx, OperatorAbortSignal)
	for abortCh.ReceiveAsync(nil) {
	}
}

// notifyWritePathPause runs the best-effort write-path pause alert activity on the
// control worker (SPEC §11). Like notifyFailure it never propagates an error: a
// dispatch failure is logged so a webhook outage never aborts a paused run.
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

// retrySet builds the narrowed drive-set that re-drives only the failed tapes on
// resume. Each retry assignment keeps the failed tape's drive and (tape, copy)
// identity and reuses its source slot as the blank slot — the operator has
// reloaded a fresh blank there (SPEC §4.3, decision B1). It carries the drive's
// config index explicitly (DriveIndex) so the Load phase, which pairs by drive
// identity, re-drives the tape onto the same physical drive it failed on even
// though this narrowed set is not a 0-based prefix of the library's drives
// (issue #137).
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

// failedAsWritten adapts the failed tapes into the minimal WrittenTape shape the
// Eject activity needs (barcode, drive, source slot) to unload them from their
// drives and export them for removal. They are never recorded as written.
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

// ejectProjection returns the minimal WrittenTape shape the Eject activity needs
// (barcode, drive, indices, source slot) for each written tape, mirroring
// failedAsWritten. It drops the staged-index path and write-health that Eject
// never reads, keeping the EjectInput payload bounded and independent of run
// size (issue #221), and — being a fresh slice of fresh values — it never
// aliases or mutates the slice the caller records in the run.
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

// reloadSlots lists the storage slots the operator must restock with fresh blanks
// for the failed tapes — their source slots (SPEC §4.3, decision B1).
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

// slotsOf lists the blank slots a pending drive-set draws from — used to name the
// slots in a Load-failure alert, where no tape barcodes exist yet.
func slotsOf(set driveSet) []int {
	out := make([]int, 0, len(set))
	for _, assignment := range set {
		out = append(out, assignment.BlankSlot)
	}

	return out
}

// joinFailed renders the per-tape write failures into one error for the operator
// alert and the aborted/timed-out run result.
func joinFailed(failed []failedTape) error {
	errs := make([]error, 0, len(failed))
	for _, tape := range failed {
		errs = append(errs, fmt.Errorf("tape %s (drive %d): %w", tape.Tape.Barcode, tape.Tape.DriveIndex, tape.Err))
	}

	return errors.Join(errs...)
}
