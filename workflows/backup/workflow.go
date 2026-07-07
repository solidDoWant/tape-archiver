package backup

import (
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
	PhaseBurn         = "Burn"
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
//
// A phase that orchestrates more than one activity (e.g. Resolve runs a control
// activity then a data activity) sets run instead of activity; when run is set it
// is used in place of the generic single-activity execution, and activity is nil.
//
// The tape path (Load → Write → Eject) is a single table entry with tapePath set:
// it cannot be three independent phases because a run may need more physical tapes
// than the library has drives, so those phases interleave per drive-set (load,
// write, eject a set to free the drives, then the next set). The workflow drives it
// via runTapePath and records the phase names in completes so operators still see
// Load, Write, and Eject complete in order (SPEC §4.3 phases 6–8).
type phase struct {
	name      string
	queue     string
	activity  any
	run       func(workflow.Context, config.Config, *runState) error
	tapePath  bool
	completes []string
}

// backupPhases returns the backup pipeline phases in execution order
// (SPEC §4.3). Control-side phases (snapshot resolution, planning, report/ISO,
// delivery) run on the control queue; bulk-data phases (prepare, PAR2, verify,
// changer, LTFS, optical burn) run on the data queue (SPEC §4.1). The activities
// are stubs today; later sub-issues fill in each body.
func backupPhases() []phase {
	return []phase{
		{name: PhaseResolve, queue: TaskQueue, run: resolvePhase},
		{name: PhasePrepare, queue: DataTaskQueue, run: preparePhase},
		{name: PhasePack, queue: TaskQueue, run: packPhase},
		{name: PhaseGeneratePAR2, queue: DataTaskQueue, run: generatePAR2Phase},
		{name: PhaseVerify, queue: DataTaskQueue, run: verifyPhase},
		// The Load → Write → Eject drive-set loop (SPEC §4.3 phases 6–8).
		{name: PhaseWrite, queue: DataTaskQueue, tapePath: true, completes: []string{PhaseLoad, PhaseWrite, PhaseEject}},
		{name: PhaseReport, queue: DataTaskQueue, run: reportPhase},
		// Burn runs between Report and Deliver: it burns the recovery disc from the
		// uncompressed ISO Report staged (SPEC §10), then re-renders the delivered
		// report to record the discs. A no-op when optical burning is disabled.
		{name: PhaseBurn, queue: DataTaskQueue, run: burnPhase},
		{name: PhaseDeliver, queue: DataTaskQueue, run: deliverPhase},
	}
}

// execute runs the phase and waits for it to complete. An implemented phase with
// a custom orchestrator (run) is driven through it, threading cfg and runState; a
// stub phase runs its single activity on its task queue, discarding the result.
func (p phase) execute(ctx workflow.Context, cfg config.Config, state *runState) error {
	if p.run != nil {
		return p.run(ctx, cfg, state)
	}

	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           p.queue,
		StartToCloseTimeout: defaultActivityTimeout,
	})

	return workflow.ExecuteActivity(actx, p.activity).Get(actx, nil)
}

// Backup is the tape-archiver backup workflow (SPEC §4.3). It validates the run
// config at entry, then sequences the pipeline phases in order, tracking the most
// recently completed phase for the LastCompletedPhaseQuery, and returns a Result
// listing the completed phases on success. On any phase failure a deferred handler
// fires the operational failure alert (SPEC §11) without masking the original
// error.
//
// The phase activities are stubbed in this scaffold; the per-phase logic lands
// in separate sub-issues that replace each stub. The control worker registers
// this workflow under WorkflowType via RegisterControl.
func Backup(ctx workflow.Context, cfg config.Config) (result Result, err error) {
	// Validate the run config before any work (SPEC §4.2, §4.3). A run submitted
	// directly through the Temporal UI/CLI bypasses the client-side validation in
	// tapectl and config-load, so without this gate an invalid payload (no sources,
	// copies < 1, ...) would pass Resolve and the entire terabyte-scale Prepare
	// phase before a late error — or, worse, complete as a silently vacuous backup
	// that wrote nothing, violating the run config's role as the single source of
	// truth. Validate is pure and deterministic (it only reads struct fields and
	// iterates slices — no I/O, time, or randomness), so it is safe to call directly
	// in the workflow with no activity. The check sits before the failure-alert
	// defer below: a rejected payload is a bad request, not an operational run
	// failure, so it does not fire the SPEC §11 Discord alert — the submitter sees
	// the workflow error directly.
	if validateErr := cfg.Validate(); validateErr != nil {
		return Result{}, fmt.Errorf("invalid config: %w", validateErr)
	}

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

		if currentPhase.tapePath {
			// The tape path interleaves Load/Write/Eject per drive-set; it reports
			// which sub-phase failed through failingPhase (for the failure alert)
			// and, on success, records Load/Write/Eject as completed in order.
			if phaseErr := runTapePath(ctx, cfg, state, &failingPhase); phaseErr != nil {
				err = fmt.Errorf("phase %s: %w", failingPhase, phaseErr)

				return Result{}, err
			}

			for _, name := range currentPhase.completes {
				state.lastCompletedPhase = name
				result.CompletedPhases = append(result.CompletedPhases, name)
			}

			continue
		}

		if phaseErr := currentPhase.execute(ctx, cfg, state); phaseErr != nil {
			err = fmt.Errorf("phase %s: %w", currentPhase.name, phaseErr)

			return Result{}, err
		}

		state.lastCompletedPhase = currentPhase.name
		result.CompletedPhases = append(result.CompletedPhases, currentPhase.name)
	}

	return result, nil
}

// runTapePath drives the interleaved Load → Write → Eject drive-set loop (SPEC
// §4.3 phases 6–8). It partitions the plan into drive-sets of at most len(Drives)
// physical tapes, then loads, writes, and ejects each set in turn — ejecting a set
// frees the drives for the next. Processing sets sequentially bounds the tapes
// loaded and read concurrently to the drive count, protecting the write-rate floor
// (SPEC §14). An empty plan yields no sets and is a no-op.
//
// Each set runs through runDriveSet, which pauses for the operator on a Load or
// Write failure instead of failing the whole run (SPEC §4.3): on resume it
// re-drives only the failed tapes onto fresh blanks, leaving already-completed
// sets — and the tapes that succeeded within the failing set — untouched. A set
// that the operator aborts (or that exhausts the operator wait) returns an error,
// so no later set is loaded. runDriveSet updates *failingPhase to the sub-phase in
// flight so the caller's failure alert names where the run stopped, and advances
// state.lastCompletedPhase as each set completes so the progress query reflects
// the live phase.
func runTapePath(ctx workflow.Context, cfg config.Config, state *runState, failingPhase *string) error {
	*failingPhase = PhaseLoad

	sets, err := planDriveSets(state.plan, cfg.Library.Drives, cfg.Library.BlankSlots)
	if err != nil {
		return err
	}

	for _, set := range sets {
		if err := runDriveSet(ctx, cfg, state, set, failingPhase); err != nil {
			return err
		}
	}

	return nil
}

// Each phase is implemented in its own file, orchestrated by a run func in the
// phase table above (SPEC §4.3).

// The Resolve phase (SPEC §4.3 phase 1) is implemented in resolve.go; it
// orchestrates a control and a data activity rather than a single stub.

// The Prepare phase (SPEC §4.3 phase 2) is implemented in prepare.go; it
// orchestrates the data-side staging activity rather than a single stub.

// The Pack phase (SPEC §4.3 phase 3) is implemented in plan.go; it orchestrates
// the control-side bin-packing activity rather than a single stub.

// The Generate PAR2 phase (SPEC §4.3 phase 4) is implemented in par2.go; it
// orchestrates the data-side parity activity rather than a single stub.

// The Verify phase (SPEC §4.3 phase 5) is implemented in verify.go; it
// orchestrates the data-side re-read/checksum activity rather than a single stub.

// The Load, Write, and Eject phases (SPEC §4.3 phases 6–8) form the tape path,
// driven per drive-set by runTapePath above. loadPhase (library.go: blank-tape
// gate + changer moves) and ejectPhase (library.go: unload + transfer to I/O station)
// bracket writePhase (session.go: a session-pinned FormatTape → WriteTree →
// FinalizeTape sequence). They interleave per set rather than running once each
// so a run can span more physical tapes than the library has drives.

// The Report phase (SPEC §4.3 phase 9) is implemented in report.go; it
// orchestrates the data-side report/ISO build activity.

// The Burn phase (SPEC §10) is implemented in burnpath.go / burnpause.go, driven
// per burn-set by burnPhase → runBurnPath (the optical analogue of the tape
// Load/Write/Eject drive-set loop). It burns the recovery disc from the
// uncompressed ISO Report staged, pausing for the operator between burn-sets (a
// manual disc swap — there is no optical autoloader) and on any burn/verify
// failure, then re-renders the delivered report to record the discs. It is a
// no-op when optical burning is disabled, so the run completes exactly as before.

// The Deliver phase (SPEC §4.3 phase 11) is implemented in deliver.go; it
// orchestrates the data-side Discord delivery activity.
