package backup

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// Phase names (SPEC §4.3). These are the values the LastCompletedPhaseQuery
// returns and that operators see when inspecting a run, so they are stable,
// human-readable identifiers rather than internal symbols.
const (
	PhaseResolve      = "Resolve"
	PhasePrepare      = "Prepare"
	PhasePack         = "Pack"
	PhaseGeneratePAR2 = "Generate PAR2"
	PhaseVerify       = "Verify"
	PhaseLoad         = "Load"
	PhaseWrite        = "Write"
	PhaseEject        = "Eject"
	PhaseReport       = "Report"
	PhaseDeliver      = "Deliver"
)

// defaultActivityTimeout bounds each phase activity for the scaffold. It is a
// deliberately generous placeholder; each phase sub-issue sets a timeout (and
// retry policy) suited to its real work — Write runs for hours, Resolve for
// seconds — when it replaces its stub.
const defaultActivityTimeout = 24 * time.Hour

// phase pairs a phase's stable name with the task queue its activity runs on and
// the activity itself. The phase table below is the single, ordered description
// of the backup pipeline; each phase sub-issue replaces an entry's stub activity
// (and, as needed, its queue) without disturbing the others.
type phase struct {
	name     string
	queue    string
	activity any
}

// backupPhases returns the ten backup pipeline phases in execution order
// (SPEC §4.3). Control-side phases (snapshot resolution, planning, report/ISO,
// delivery) run on the control queue; bulk-data phases (prepare, PAR2, verify,
// changer, LTFS) run on the data queue (SPEC §4.1). The activities are stubs
// today; later sub-issues fill in each body.
func backupPhases() []phase {
	return []phase{
		{PhaseResolve, TaskQueue, resolveActivity},
		{PhasePrepare, DataTaskQueue, prepareActivity},
		{PhasePack, TaskQueue, packActivity},
		{PhaseGeneratePAR2, DataTaskQueue, generatePAR2Activity},
		{PhaseVerify, DataTaskQueue, verifyActivity},
		{PhaseLoad, DataTaskQueue, loadActivity},
		{PhaseWrite, DataTaskQueue, writeActivity},
		{PhaseEject, DataTaskQueue, ejectActivity},
		{PhaseReport, TaskQueue, reportActivity},
		{PhaseDeliver, TaskQueue, deliverActivity},
	}
}

// execute runs the phase's activity on its task queue and waits for it to
// complete. Results are discarded for now; phase sub-issues that produce
// run-state thread it through runState.
func (p phase) execute(ctx workflow.Context) error {
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           p.queue,
		StartToCloseTimeout: defaultActivityTimeout,
	})

	return workflow.ExecuteActivity(actx, p.activity).Get(actx, nil)
}

// Backup is the tape-archiver backup workflow (SPEC §4.3). It sequences the ten
// pipeline phases in order, tracking the most recently completed phase for the
// LastCompletedPhaseQuery, and returns a Result listing the completed phases on
// success. On any phase failure a deferred handler fires the operational failure
// alert (SPEC §11) without masking the original error.
//
// The phase activities are stubbed in this scaffold; the per-phase logic lands
// in separate sub-issues that replace each stub. The control worker registers
// this workflow under WorkflowType via RegisterControl.
func Backup(ctx workflow.Context, cfg config.Config) (result Result, err error) {
	_ = cfg // consumed by phase sub-issues; named here so the contract is stable.

	state := &runState{}

	if queryErr := workflow.SetQueryHandler(ctx, LastCompletedPhaseQuery, func() (string, error) {
		return state.lastCompletedPhase, nil
	}); queryErr != nil {
		return Result{}, fmt.Errorf("install %s query handler: %w", LastCompletedPhaseQuery, queryErr)
	}

	// failingPhase names the phase in flight so the deferred failure alert can
	// report where the run failed. It is the last phase started; on success the
	// deferred handler is a no-op because err is nil.
	failingPhase := ""

	defer func() {
		if err != nil {
			notifyFailure(ctx, failingPhase, err)
		}
	}()

	for _, currentPhase := range backupPhases() {
		failingPhase = currentPhase.name

		if phaseErr := currentPhase.execute(ctx); phaseErr != nil {
			err = fmt.Errorf("phase %s: %w", currentPhase.name, phaseErr)

			return Result{}, err
		}

		state.lastCompletedPhase = currentPhase.name
		result.CompletedPhases = append(result.CompletedPhases, currentPhase.name)
	}

	return result, nil
}

// The phase activities below are stubs: each is a no-op that returns nil so the
// workflow backbone runs end-to-end. Each later sub-issue replaces one stub's
// body (and registration) with the real activity for that phase (SPEC §4.3).

// resolveActivity stubs the Resolve phase (SPEC §4.3 phase 1).
func resolveActivity(_ context.Context) error { return nil }

// prepareActivity stubs the Prepare phase (SPEC §4.3 phase 2).
func prepareActivity(_ context.Context) error { return nil }

// packActivity stubs the Pack phase (SPEC §4.3 phase 3).
func packActivity(_ context.Context) error { return nil }

// generatePAR2Activity stubs the Generate PAR2 phase (SPEC §4.3 phase 4).
func generatePAR2Activity(_ context.Context) error { return nil }

// verifyActivity stubs the Verify phase (SPEC §4.3 phase 5).
func verifyActivity(_ context.Context) error { return nil }

// loadActivity stubs the Load phase (SPEC §4.3 phase 6).
func loadActivity(_ context.Context) error { return nil }

// writeActivity stubs the Write phase (SPEC §4.3 phase 7).
func writeActivity(_ context.Context) error { return nil }

// ejectActivity stubs the Eject phase (SPEC §4.3 phase 8).
func ejectActivity(_ context.Context) error { return nil }

// reportActivity stubs the Report phase (SPEC §4.3 phase 9).
func reportActivity(_ context.Context) error { return nil }

// deliverActivity stubs the Deliver phase (SPEC §4.3 phase 10).
func deliverActivity(_ context.Context) error { return nil }
