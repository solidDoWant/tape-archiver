package backup

import (
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// IOStationWaitWorkflowType is the child workflow type running the Eject-pause
// auto-resume poll loop.
const IOStationWaitWorkflowType = "ioStationWait"

// maxPollsBeforeContinue bounds polls per child execution before ContinueAsNew.
// A 12 h pause polled every 30 s accrues ~15k history events inline, so a few
// pauses would cross Temporal's 51,200-event limit and kill the run (#168).
// 200 polls caps each execution at ~2,200 events.
const maxPollsBeforeContinue = 200

// ioStationWaitInput is the child workflow's payload. Deadline is absolute so
// the total wait budget is unchanged across ContinueAsNew.
type ioStationWaitInput struct {
	Cfg      config.Config
	Deadline time.Time
}

// ioStationWaitWorkflow polls the I/O station as a self-continuing child
// workflow so the parent's history stays bounded however long the pause lasts
// (#168). It returns true on auto-resume and false when Deadline elapses. The
// parent owns the resume signal and cancels this child on explicit resume.
func ioStationWaitWorkflow(ctx workflow.Context, input ioStationWaitInput) (bool, error) {
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

		// Cancelled by the parent on resume: the timers fired, so don't poll.
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

	// Poll budget spent: continue with a fresh history and the same Deadline.
	return false, workflow.NewContinueAsNewError(ctx, ioStationWaitWorkflow, input)
}
