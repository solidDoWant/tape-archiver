package backup

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// Phase names (SPEC §4.3), as returned by LastCompletedPhaseQuery and shown to
// operators.
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

// PhaseHold labels the source-snapshot hold between Resolve and Prepare. It is
// not a pipeline phase and only appears in failure alerts.
const PhaseHold = "Hold"

// defaultActivityTimeout bounds a single-activity phase that sets no timeout of
// its own.
const defaultActivityTimeout = 24 * time.Hour

// phase is one entry in the ordered backup pipeline. A phase that orchestrates
// several activities sets run instead of activity.
//
// The tape path is a single entry with tapePath set: a run may need more tapes
// than the library has drives, so Load/Write/Eject interleave per drive-set.
// completes lists the names recorded when it finishes (SPEC §4.3 phases 6–8).
type phase struct {
	name      string
	queue     string
	activity  any
	run       func(workflow.Context, config.Config, *runState) error
	tapePath  bool
	completes []string
}

// backupPhases returns the pipeline phases in execution order (SPEC §4.3, §4.1).
func backupPhases() []phase {
	return []phase{
		{name: PhaseResolve, queue: TaskQueue, run: resolvePhase},
		{name: PhasePrepare, queue: DataTaskQueue, run: preparePhase},
		{name: PhasePack, queue: TaskQueue, run: packPhase},
		{name: PhaseGeneratePAR2, queue: DataTaskQueue, run: generatePAR2Phase},
		{name: PhaseVerify, queue: DataTaskQueue, run: verifyPhase},
		{name: PhaseWrite, queue: DataTaskQueue, tapePath: true, completes: []string{PhaseLoad, PhaseWrite, PhaseEject}},
		{name: PhaseReport, queue: DataTaskQueue, run: reportPhase},
		// Burn is a no-op when optical burning is disabled (SPEC §10).
		{name: PhaseBurn, queue: DataTaskQueue, run: burnPhase},
		{name: PhaseDeliver, queue: DataTaskQueue, run: deliverPhase},
	}
}

// execute runs the phase via its run func, or its single activity on its queue.
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

// Backup is the backup workflow (SPEC §4.3): it validates the config, runs the
// phases in order, and fires the failure alert (SPEC §11) on any phase error.
func Backup(ctx workflow.Context, cfg config.Config) (result Result, err error) {
	// A run submitted straight through Temporal bypasses client-side validation;
	// without this an invalid payload could stage terabytes before failing, or
	// complete as a vacuous backup. Validate is pure, so it is deterministic. It
	// runs before the alert defer: a bad request is not an operational failure.
	if validateErr := cfg.Validate(); validateErr != nil {
		return Result{}, fmt.Errorf("invalid config: %w", validateErr)
	}

	state := &runState{}

	if queryErr := workflow.SetQueryHandler(ctx, LastCompletedPhaseQuery, func() (string, error) {
		return state.lastCompletedPhase, nil
	}); queryErr != nil {
		return Result{}, fmt.Errorf("install %s query handler: %w", LastCompletedPhaseQuery, queryErr)
	}

	if queryErr := workflow.SetQueryHandler(ctx, CurrentPauseQuery, func() (CurrentPause, error) {
		return state.currentPause, nil
	}); queryErr != nil {
		return Result{}, fmt.Errorf("install %s query handler: %w", CurrentPauseQuery, queryErr)
	}

	// The staged tree is a full second copy of the run's data; leaking it fills
	// the staging dataset within a few runs. Armed before Prepare because a run
	// dying mid-Prepare strands the most data. Deferred before the alert defer so
	// it runs after it and cannot delay or influence the alert.
	defer cleanupStaging(ctx, state)

	failingPhase := ""

	defer func() {
		if err != nil {
			notifyFailure(ctx, failingPhase, err)
		}
	}()

	for _, currentPhase := range backupPhases() {
		failingPhase = currentPhase.name

		if currentPhase.tapePath {
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

		// Pin the source snapshots with a run-scoped `zfs hold` before hours of
		// Prepare work; a hold failure fails the run here. The release is armed
		// before the hold because HoldSnapshots aborts on the first failure,
		// leaving earlier holds in place that would otherwise leak past the run
		// (SPEC §4.2). Releasing a never-held snapshot is a no-op.
		if currentPhase.name == PhaseResolve {
			failingPhase = PhaseHold

			defer releaseSnapshots(ctx, state)

			if err = holdSnapshots(ctx, state); err != nil {
				err = fmt.Errorf("phase %s: %w", PhaseHold, err)

				return Result{}, err
			}
		}
	}

	return result, nil
}

// runTapePath drives the Load → Write → Eject loop one drive-set at a time,
// bounding concurrent tapes to the drive count to protect the write-rate floor
// (SPEC §4.3 phases 6–8, §14). It keeps *failingPhase set to the sub-phase in
// flight for the failure alert.
func runTapePath(ctx workflow.Context, cfg config.Config, state *runState, failingPhase *string) error {
	*failingPhase = PhaseLoad

	// Dry runs only: mirror the real library's slots into the virtual one before
	// anything reconciles against it.
	if err := runMirrorLibrary(ctx, cfg); err != nil {
		return err
	}

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
