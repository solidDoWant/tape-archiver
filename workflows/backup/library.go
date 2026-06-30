package backup

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// The Load phase (SPEC §4.3 phase 6) is the gate between staging and tape: it
// loads blank tapes from their storage slots into the drives and confirms each is
// blank before any formatting or data transfer begins ("Never write to a non-blank
// tape", CLAUDE.md). It reads live mtx status first and reconciles to the desired
// state — idempotent for the normal case (drive already has the right blank tape),
// auto-relocating unexpected tapes, and failing clearly for irrecoverable states.
//
// The Eject phase (SPEC §4.3 phase 8) transfers each written tape from its drive
// to an I/O station slot for physical removal by the operator. Unmounting and
// capturing the LTFS index happen in the Write phase (FinalizeTape); Eject only
// issues the mtx unload + transfer.
//
// Both activities carry MaximumAttempts: 1 — tape moves are physical and
// non-idempotent. A failure surfaces for an operator decision rather than
// blindly retrying a potentially destructive changer command.

const (
	// loadTimeout bounds the Load activity: it loads tapes and runs blank checks,
	// which include rewinds — up to several minutes each on real hardware.
	loadTimeout = 30 * time.Minute
	// ejectTimeout bounds the Eject activity: a few mtx unload + transfer
	// commands, each taking seconds on real hardware.
	ejectTimeout = 10 * time.Minute
)

// LoadInput is the payload for the Load activity.
type LoadInput struct {
	// Changer is the changer device node (e.g. /dev/sch0).
	Changer string
	// Drives are the non-rewinding tape device nodes (e.g. /dev/nst0, /dev/nst1),
	// one per physical tape to write (len = len(Plan.Tapes) × Plan.Copies).
	// Drives[i] is loaded with BlankSlots[i].
	Drives []string
	// BlankSlots are the storage slot addresses holding blank tapes, one per
	// physical tape to write. BlankSlots[i] is loaded into Drives[i].
	BlankSlots []int
	// Plan is the Pack plan; its Copies and Tapes fields derive the
	// TapeIndex/CopyIndex assignment for each loaded tape.
	Plan TapePlan
}

// LoadActivities hosts the Load activity, which moves blank tapes into drives and
// confirms each is blank before any write begins.
type LoadActivities struct{}

// newLoadActivities returns the Load activities.
func newLoadActivities() *LoadActivities { return &LoadActivities{} }

// Load moves blank tapes into the drives, reconciling live changer state with
// the desired state, and performs a blank check on each loaded tape. It returns
// a LoadedTape per drive in the same order as input.Drives.
//
// MaximumAttempts is 1: tape moves are physical and non-idempotent.
func (a *LoadActivities) Load(ctx context.Context, input LoadInput) ([]LoadedTape, error) {
	totalPhysical := len(input.Plan.Tapes) * input.Plan.Copies
	if len(input.Drives) != totalPhysical {
		return nil, fmt.Errorf("plan requires %d physical tapes but %d drives provided",
			totalPhysical, len(input.Drives))
	}

	if len(input.BlankSlots) < totalPhysical {
		return nil, fmt.Errorf("plan requires %d blank tapes but only %d blank slots provided",
			totalPhysical, len(input.BlankSlots))
	}

	changer := tape.NewChanger(input.Changer)

	inv, err := changer.Inventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("inventory for load: %w", err)
	}

	loaded := make([]LoadedTape, totalPhysical)

	for i := 0; i < totalPhysical; i++ {
		stDev := input.Drives[i]
		targetSlot := input.BlankSlots[i]
		driveAddr := inv.Drives[i].Address

		barcode, err := reconcileLoad(ctx, changer, inv, i, driveAddr, targetSlot)
		if err != nil {
			return nil, fmt.Errorf("load drive %d (slot %d): %w", i, targetSlot, err)
		}

		drive := tape.NewDrive(stDev)

		sgDev, err := drive.SGDevice()
		if err != nil {
			return nil, fmt.Errorf("drive %d: resolve SCSI generic node for %s: %w", i, stDev, err)
		}

		blank, err := drive.IsBlank(ctx)
		if err != nil {
			return nil, fmt.Errorf("drive %d: blank check for tape %s: %w", i, barcode, err)
		}

		if !blank {
			return nil, fmt.Errorf("drive %d: tape %s is not blank — refusing to write"+
				" (SPEC §4.3 step 6; reload a blank tape to continue)", i, barcode)
		}

		loaded[i] = LoadedTape{
			Barcode:    barcode,
			DriveIndex: i,
			TapeIndex:  i / input.Plan.Copies,
			CopyIndex:  i % input.Plan.Copies,
			SourceSlot: targetSlot,
			STDevice:   stDev,
			SGDevice:   sgDev,
		}
	}

	return loaded, nil
}

// reconcileLoad ensures the drive at driveAddr (which corresponds to the i-th
// drive in the config) is loaded with the tape from targetSlot. It reads the
// current inventory state and issues only the mtx commands needed. Returns the
// barcode of the tape that ends up in the drive.
//
// The reconciliation table (from the Load phase design in CLAUDE.md):
//   - Drive already loaded from targetSlot → skip load (idempotent).
//   - Drive empty → load from targetSlot.
//   - Drive loaded with unexpected tape → auto-relocate to a free slot, then load.
//
// The blank check always runs after this function, so non-blank tapes are caught
// regardless of which path was taken.
func reconcileLoad(ctx context.Context, changer *tape.Changer, inv tape.Inventory, driveIndex, driveAddr, targetSlot int) (tape.Barcode, error) {
	if driveIndex >= len(inv.Drives) {
		return "", fmt.Errorf("drive index %d out of range (library has %d drives)", driveIndex, len(inv.Drives))
	}

	de := inv.Drives[driveIndex]

	if de.Loaded {
		if de.SourceSlot == targetSlot {
			// Tape from the target slot is already in the drive (idempotent).
			return de.Barcode, nil
		}

		// Unexpected tape — relocate it to a free storage slot (prefer its home
		// slot), then load the blank. A tape with an active LTFS mount asserts
		// SCSI PREVENT MEDIUM REMOVAL, so the unload will fail with a clear
		// hardware error and the run aborts rather than force-yanking a live tape.
		relocateSlot := findFreeStorageSlot(inv, de.SourceSlot)
		if relocateSlot == -1 {
			return "", fmt.Errorf("drive has unexpected tape %s (from slot %d) and no free storage slot to relocate it",
				de.Barcode, de.SourceSlot)
		}

		if err := changer.Unload(ctx, relocateSlot, driveAddr); err != nil {
			return "", fmt.Errorf("relocate unexpected tape %s (from slot %d) to slot %d: %w",
				de.Barcode, de.SourceSlot, relocateSlot, err)
		}
	}

	// Drive is now empty — confirm the target slot has a tape and load it.
	targetBarcode, ok := slotBarcode(inv, targetSlot)
	if !ok {
		return "", fmt.Errorf("target slot %d is empty; cannot load a blank tape", targetSlot)
	}

	if err := changer.Load(ctx, targetSlot, driveAddr); err != nil {
		return "", fmt.Errorf("load tape %s from slot %d: %w", targetBarcode, targetSlot, err)
	}

	return targetBarcode, nil
}

// findFreeStorageSlot returns the address of an empty storage slot, preferring
// preferSlot if it is empty. Returns -1 if no empty storage slot exists.
func findFreeStorageSlot(inv tape.Inventory, preferSlot int) int {
	for _, slot := range inv.Slots {
		if slot.Address == preferSlot && !slot.Full {
			return slot.Address
		}
	}

	for _, slot := range inv.Slots {
		if !slot.Full {
			return slot.Address
		}
	}

	return -1
}

// findFreeIOSlot returns the address of an empty I/O station slot, or -1 if all
// I/O slots are occupied.
func findFreeIOSlot(inv tape.Inventory) int {
	for _, slot := range inv.IOSlots {
		if !slot.Full {
			return slot.Address
		}
	}

	return -1
}

// slotBarcode returns the barcode of the tape in the given storage slot address
// and whether the slot is occupied. It scans the full slot list because slot
// addresses are not guaranteed to be contiguous or zero-indexed.
func slotBarcode(inv tape.Inventory, addr int) (tape.Barcode, bool) {
	for _, slot := range inv.Slots {
		if slot.Address == addr {
			return slot.Barcode, slot.Full
		}
	}

	return "", false
}

// loadPhase orchestrates the Load phase (SPEC §4.3 phase 6): it dispatches the
// Load activity on the data worker and stores the loaded tapes in runState for the
// Write and Eject phases.
func loadPhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	totalPhysical := len(state.plan.Tapes) * state.plan.Copies

	if totalPhysical == 0 {
		return nil
	}

	if totalPhysical > len(cfg.Library.Drives) {
		return fmt.Errorf("plan requires %d physical tapes but only %d drives are configured",
			totalPhysical, len(cfg.Library.Drives))
	}

	if totalPhysical > len(cfg.Library.BlankSlots) {
		return fmt.Errorf("plan requires %d blank tapes but only %d blank slots are configured",
			totalPhysical, len(cfg.Library.BlankSlots))
	}

	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: loadTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})

	input := LoadInput{
		Changer:    cfg.Library.Changer,
		Drives:     cfg.Library.Drives[:totalPhysical],
		BlankSlots: cfg.Library.BlankSlots[:totalPhysical],
		Plan:       state.plan,
	}

	var acts *LoadActivities

	var loaded []LoadedTape
	if err := workflow.ExecuteActivity(dataCtx, acts.Load, input).Get(dataCtx, &loaded); err != nil {
		return err
	}

	state.loaded = loaded

	return nil
}

// EjectInput is the payload for the Eject activity.
type EjectInput struct {
	// Changer is the changer device node (e.g. /dev/sch0).
	Changer string
	// WrittenTapes are the tapes to eject, each carrying its DriveIndex and
	// SourceSlot for unload + transfer.
	WrittenTapes []WrittenTape
}

// EjectActivities hosts the Eject activity.
type EjectActivities struct{}

// newEjectActivities returns the Eject activities.
func newEjectActivities() *EjectActivities { return &EjectActivities{} }

// Eject unloads each written tape from its drive back to its source slot and
// transfers it to a free I/O station slot. It reads live changer state first and
// reconciles, handling the case where a tape was already unloaded or already
// transferred before this activity ran.
//
// MaximumAttempts is 1: tape moves are physical and non-idempotent.
func (a *EjectActivities) Eject(ctx context.Context, input EjectInput) error {
	if len(input.WrittenTapes) == 0 {
		return nil
	}

	changer := tape.NewChanger(input.Changer)

	inv, err := changer.Inventory(ctx)
	if err != nil {
		return fmt.Errorf("inventory for eject: %w", err)
	}

	for i, wt := range input.WrittenTapes {
		if err := ejectTape(ctx, changer, inv, wt); err != nil {
			return fmt.Errorf("eject tape %s (drive %d, index %d): %w", wt.Barcode, wt.DriveIndex, i, err)
		}

		// Re-read inventory after each move so the next iteration sees accurate state.
		if i < len(input.WrittenTapes)-1 {
			inv, err = changer.Inventory(ctx)
			if err != nil {
				return fmt.Errorf("inventory after ejecting tape %s: %w", wt.Barcode, err)
			}
		}
	}

	return nil
}

// ejectTape ejects a single written tape to an I/O station slot. It reconciles
// the live drive state:
//   - Drive loaded and SourceSlot matches → unload to SourceSlot, transfer to I/O.
//   - Tape already in SourceSlot (drive empty) → transfer to I/O.
//   - Tape already in an I/O slot → no-op.
func ejectTape(ctx context.Context, changer *tape.Changer, inv tape.Inventory, wt WrittenTape) error {
	// Check if already in an I/O slot.
	for _, io := range inv.IOSlots {
		if io.Full && io.Barcode == wt.Barcode {
			return nil
		}
	}

	driveAddr := -1

	if wt.DriveIndex < len(inv.Drives) {
		driveAddr = inv.Drives[wt.DriveIndex].Address
	}

	// Unload from drive to source slot if the drive still holds the tape.
	if driveAddr >= 0 && wt.DriveIndex < len(inv.Drives) && inv.Drives[wt.DriveIndex].Loaded &&
		inv.Drives[wt.DriveIndex].Barcode == wt.Barcode {
		if err := changer.Unload(ctx, wt.SourceSlot, driveAddr); err != nil {
			return fmt.Errorf("unload tape %s from drive %d to slot %d: %w",
				wt.Barcode, wt.DriveIndex, wt.SourceSlot, err)
		}
	}

	// Transfer from source slot to a free I/O slot.
	ioSlot := findFreeIOSlot(inv)
	if ioSlot == -1 {
		return fmt.Errorf("no free I/O slot to transfer tape %s", wt.Barcode)
	}

	if err := changer.Transfer(ctx, wt.SourceSlot, ioSlot); err != nil {
		return fmt.Errorf("transfer tape %s from slot %d to I/O slot %d: %w",
			wt.Barcode, wt.SourceSlot, ioSlot, err)
	}

	return nil
}

// ejectPhase orchestrates the Eject phase (SPEC §4.3 phase 8): it dispatches the
// Eject activity on the data worker to transfer each written tape from its drive
// to an I/O station slot for physical removal.
func ejectPhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	if len(state.written) == 0 {
		return nil
	}

	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: ejectTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})

	input := EjectInput{
		Changer:      cfg.Library.Changer,
		WrittenTapes: state.written,
	}

	var acts *EjectActivities

	return workflow.ExecuteActivity(dataCtx, acts.Eject, input).Get(dataCtx, nil)
}
