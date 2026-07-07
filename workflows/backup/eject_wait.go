package backup

import (
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// IOStationWaitWorkflowType is the Temporal workflow type name of the child
// workflow that runs the Eject-pause auto-resume poll loop. It is registered on
// the control worker (RegisterControl) and started by waitForIOCleared.
const IOStationWaitWorkflowType = "ioStationWait"

// maxPollsBeforeContinue bounds how many I/O-station polls a single child
// execution performs before it calls ContinueAsNew to reset its own history.
//
// The Eject pause polls once per ioStationPollInterval (30 s) for up to
// EffectiveIOWaitTimeout (default 12 h). Left in one workflow execution that is
// ~1,440 polls, and each poll adds a timer plus an IOStationStatus activity
// (≤ ioStatusMaxAttempts) — roughly 11 history events. A fully-elapsed pause
// therefore accrues ~15k events, and a run with several such pauses crosses
// Temporal's 51,200-event hard limit and is terminated mid-run.
//
// Running the poll loop in a child workflow that ContinueAsNew's every
// maxPollsBeforeContinue polls caps each child execution's history at
// ~maxPollsBeforeContinue × 11 ≈ 2,200 events (~4% of the limit) regardless of
// how long the operator takes, and the parent run only ever records the child
// start/complete pair — so the run's own history stays O(1) per pause. 200 polls
// is a fresh execution roughly every 100 minutes: far below the limit with ample
// margin, while continuing rarely enough that continuations are cheap.
const maxPollsBeforeContinue = 200

// ioStationWaitInput is the child workflow's payload. It carries the run's
// library config (for the IOStationStatus poll) and the absolute deadline at
// which the operator wait expires. The deadline is absolute server workflow time
// so the total wait budget is preserved unchanged across every ContinueAsNew —
// each continuation re-derives its remaining time from the same Deadline.
type ioStationWaitInput struct {
	// Cfg is the run config; only Library is read (Changer + the poll retry
	// budget flow through runIOStationStatus).
	Cfg config.Config
	// Deadline is the absolute workflow time at which the operator I/O-station
	// wait expires. Preserved verbatim across ContinueAsNew so continuing the
	// poll loop never extends or shortens the total wait.
	Deadline time.Time
}

// ioStationWaitWorkflow runs the Eject-pause auto-resume poll loop as a
// self-continuing child workflow so the parent run's history stays bounded no
// matter how long the pause lasts (issue #168). It returns true when the I/O
// station reports it can auto-resume (IOStatus.CanAutoResume) and false when the
// absolute Deadline elapses first — the same two outcomes the inline loop
// produced. The parent (waitForIOCleared) owns the OperatorResumeSignal and
// cancels this child when the operator resumes explicitly, so the child never
// handles the signal itself.
//
// It selects over an overall-deadline timer (remaining = Deadline − now, so it
// survives continuations) and a repeating poll timer, exactly as the former
// inline loop did. After maxPollsBeforeContinue polls without resume or timeout
// it returns workflow.NewContinueAsNewError with the unchanged input, resetting
// its history to keep the pause's event growth bounded.
func ioStationWaitWorkflow(ctx workflow.Context, input ioStationWaitInput) (bool, error) {
	// One deadline timer for the whole remaining budget. Recomputed from the
	// absolute Deadline on each continuation, so the total wait is invariant.
	remaining := input.Deadline.Sub(workflow.Now(ctx))
	deadlineTimer := workflow.NewTimer(ctx, remaining)

	for polls := 0; polls < maxPollsBeforeContinue; polls++ {
		var timedOut, polled bool

		selector := workflow.NewSelector(ctx)
		selector.AddFuture(deadlineTimer, func(workflow.Future) {
			timedOut = true
		})
		selector.AddFuture(workflow.NewTimer(ctx, ioStationPollInterval), func(workflow.Future) {
			polled = true
		})

		selector.Select(ctx)

		// The parent cancels this child when the operator signals resume. A
		// cancelled context fires the pending timers; exit promptly rather than
		// issuing a poll on a cancelled context.
		if ctx.Err() != nil {
			return false, ctx.Err()
		}

		switch {
		case timedOut:
			return false, nil
		case polled:
			status, err := runIOStationStatus(ctx, input.Cfg)
			if err != nil {
				return false, err
			}

			if status.CanAutoResume() {
				return true, nil
			}
		}
	}

	// The poll budget for this execution is spent without resuming or timing
	// out. Continue as a fresh execution with an empty history and the same
	// absolute Deadline so polling resumes seamlessly (issue #168).
	return false, workflow.NewContinueAsNewError(ctx, ioStationWaitWorkflow, input)
}
