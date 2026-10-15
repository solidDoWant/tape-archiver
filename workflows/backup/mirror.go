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

// mirrorTimeout bounds the mirror: library moves plus regenerating every
// mirrored volume.
const mirrorTimeout = 10 * time.Minute

// runMirrorLibrary makes the virtual library mirror the real one before the tape
// path runs. Gated on the dry-run memo as well as MirrorSourceChanger: the
// mirror moves tapes in the target library, so a real run must never dispatch it
// whatever its config carries (issue #382).
func runMirrorLibrary(ctx workflow.Context, cfg config.Config) error {
	if cfg.Library.MirrorSourceChanger == "" {
		return nil
	}

	if !isDryRun(ctx) {
		workflow.GetLogger(ctx).Warn("ignoring library.mirrorSourceChanger on a non-dry run",
			"mirrorSourceChanger", cfg.Library.MirrorSourceChanger)

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

// MirrorLibraryInput identifies the real library to mirror from and the virtual
// library to mirror onto.
type MirrorLibraryInput struct {
	// SourceChanger is the REAL changer device to read the inventory from.
	SourceChanger string
	// TargetChanger is the VIRTUAL (mhvtl) changer the run targets.
	TargetChanger string
}

// MirrorLibraryResult reports what the mirror produced.
type MirrorLibraryResult struct {
	// OccupiedSlots is how many storage slots hold a tape after mirroring.
	OccupiedSlots int
	// TotalSlots is how many storage slots the real library reports.
	TotalSlots int
}

// MirrorActivities hosts the MirrorLibrary activity.
type MirrorActivities struct{}

func newMirrorActivities() *MirrorActivities { return &MirrorActivities{} }

// MirrorLibrary rearranges the virtual library to match the real one's slot
// inventory. The real changer is only read. Not retried: a partly applied mirror
// is better reported than blindly redone.
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

	if err := mhvtl.New().Mirror(ctx, tape.NewChanger(input.TargetChanger), source); err != nil {
		return MirrorLibraryResult{}, fmt.Errorf("mirror onto virtual library %s: %w", input.TargetChanger, err)
	}

	slog.InfoContext(ctx, "dry run: virtual library now mirrors the real one",
		"occupiedSlots", occupied, "totalSlots", len(source.Slots))

	return MirrorLibraryResult{OccupiedSlots: occupied, TotalSlots: len(source.Slots)}, nil
}
