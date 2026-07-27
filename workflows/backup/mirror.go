package backup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/mhvtl"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// mirrorTimeout bounds the reprovision: it regenerates media for every mirrored
// barcode and then waits for vtllibrary to re-read its config.
const mirrorTimeout = 10 * time.Minute

// runMirrorLibrary makes the virtual library mirror the real one before the tape
// path runs. It is a no-op on a real run — cfg.Library.MirrorSourceChanger is set
// only by runsubmit.ApplyDryRun, and only for a dry run — so a real run never
// dispatches this activity at all.
func runMirrorLibrary(ctx workflow.Context, cfg config.Config) error {
	if cfg.Library.MirrorSourceChanger == "" {
		return nil
	}

	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: mirrorTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})

	input := MirrorLibraryInput{
		SourceChanger: cfg.Library.MirrorSourceChanger,
		TargetChanger: cfg.Library.Changer,
	}

	var acts *MirrorActivities

	var result MirrorLibraryResult
	if err := workflow.ExecuteActivity(dataCtx, acts.MirrorLibrary, input).Get(dataCtx, &result); err != nil {
		return fmt.Errorf("mirror the real library onto the virtual one for this dry run: %w", err)
	}

	workflow.GetLogger(ctx).Info("dry run: virtual library mirrored from the real library",
		"occupiedSlots", result.OccupiedSlots, "totalSlots", result.TotalSlots)

	return nil
}

// The MirrorLibrary activity (dry runs only) makes the virtual mhvtl library the
// run targets mirror the real library's slot inventory, so a dry run rehearses
// the conditions the real run would actually meet. It runs once, before the tape
// path, because every later phase (Load in particular) reconciles against the
// library's contents and must see the mirrored state.
//
// It is gated entirely on config.Library.MirrorSourceChanger, which only
// runsubmit.ApplyDryRun sets and only on a dry run. A real run leaves it empty
// and the workflow skips this activity outright.
//
// Safety: the source (real) changer is only ever read — READ ELEMENT STATUS. All
// mutation lands on mhvtl's own library_contents/media and its own daemon (see
// pkg/mhvtl), so this cannot disturb real media even if pointed at the wrong
// device.

// MirrorLibraryInput identifies the real library to mirror from and the virtual
// library to mirror onto.
type MirrorLibraryInput struct {
	// SourceChanger is the REAL changer device to read the inventory from.
	SourceChanger string
	// TargetChanger is the VIRTUAL (mhvtl) changer the run targets, polled to
	// confirm the reload landed.
	TargetChanger string
}

// MirrorLibraryResult reports what the mirror produced, for the run log and the
// report's dry-run provenance.
type MirrorLibraryResult struct {
	// OccupiedSlots is how many storage slots hold a tape after mirroring.
	OccupiedSlots int
	// TotalSlots is how many storage slots the real library reports.
	TotalSlots int
}

// MirrorActivities hosts the MirrorLibrary activity.
type MirrorActivities struct{}

// newMirrorActivities returns the mirror activities.
func newMirrorActivities() *MirrorActivities { return &MirrorActivities{} }

// MirrorLibrary reads the real library's inventory and rewrites the virtual
// library to match, then waits for the virtual changer to report the mirrored
// contents before returning — so the tape path never starts against a
// half-reloaded slot map.
//
// MaximumAttempts is 1: it rewrites media and signals a daemon, and a blind retry
// on a partially-applied reprovision is not obviously safer than failing the run.
func (a *MirrorActivities) MirrorLibrary(
	ctx context.Context,
	input MirrorLibraryInput,
) (MirrorLibraryResult, error) {
	source, err := tape.NewChanger(input.SourceChanger).Inventory(ctx)
	if err != nil {
		return MirrorLibraryResult{}, fmt.Errorf(
			"read the real library inventory from %s to mirror it: %w", input.SourceChanger, err)
	}

	occupied := 0

	for _, slot := range source.Slots {
		if slot.Full {
			occupied++
		}
	}

	slog.InfoContext(ctx, "dry run: mirroring the real library onto the virtual one",
		"sourceChanger", input.SourceChanger, "targetChanger", input.TargetChanger,
		"occupiedSlots", occupied, "totalSlots", len(source.Slots))

	reprovisioner := mhvtl.New()

	if err := reprovisioner.Mirror(ctx, source); err != nil {
		return MirrorLibraryResult{}, err
	}

	target := tape.NewChanger(input.TargetChanger)
	if err := reprovisioner.WaitReady(ctx, target, source); err != nil {
		return MirrorLibraryResult{}, fmt.Errorf(
			"virtual library %s did not come back with the mirrored contents: %w",
			input.TargetChanger, err)
	}

	slog.InfoContext(ctx, "dry run: virtual library now mirrors the real one",
		"occupiedSlots", occupied, "totalSlots", len(source.Slots))

	return MirrorLibraryResult{OccupiedSlots: occupied, TotalSlots: len(source.Slots)}, nil
}
