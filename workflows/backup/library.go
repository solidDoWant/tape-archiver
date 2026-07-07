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
// tape", CLAUDE.md). It reads the live changer inventory first and reconciles to
// the desired state — idempotent for the normal case (drive already has the right
// blank tape), auto-relocating unexpected tapes, and failing clearly for
// irrecoverable states.
//
// The Eject phase (SPEC §4.3 phase 8) transfers each written tape from its drive
// to an I/O station slot for physical removal by the operator. Unmounting and
// capturing the LTFS index happen in the Write phase (FinalizeTape); Eject only
// issues the changer unload + transfer.
//
// Both activities carry MaximumAttempts: 1 — tape moves are physical and
// non-idempotent. A failure surfaces for an operator decision rather than
// blindly retrying a potentially destructive changer command.

const (
	// loadTimeout bounds the Load activity: it loads tapes and runs blank checks,
	// which include rewinds — up to several minutes each on real hardware.
	loadTimeout = 30 * time.Minute
	// ejectTimeout bounds the Eject activity: a few changer unload + transfer
	// moves, each taking seconds on real hardware.
	ejectTimeout = 10 * time.Minute
)

// LoadInput is the payload for the Load activity. It describes one drive-set: at
// most len(inv.Drives) physical tapes to load and blank-check in parallel
// (SPEC §4.3 phase 6). A run spanning more physical tapes than the library has
// drives is written as a sequence of drive-sets, each a separate Load call.
type LoadInput struct {
	// Changer is the changer device node (e.g. /dev/sch0).
	Changer string
	// Tapes are the physical tapes in this drive-set, one per drive. Tapes[i] is
	// loaded into the i-th library drive; its Drive, BlankSlot, and
	// TapeIndex/CopyIndex assignment come straight from the drive-set plan.
	Tapes []TapeAssignment
	// AllowNonBlankTapes mirrors Library.AllowNonBlankTapes: when true, a loaded
	// tape that is not blank is written over (with a warning) instead of failing
	// the run. The blank check itself always runs; this only changes the non-blank
	// outcome (SPEC §4.3 step 6).
	AllowNonBlankTapes bool
}

// LoadActivities hosts the Load activity, which moves blank tapes into drives and
// confirms each is blank before any write begins.
type LoadActivities struct{}

// newLoadActivities returns the Load activities.
func newLoadActivities() *LoadActivities { return &LoadActivities{} }

// Load moves this drive-set's blank tapes into the drives, reconciling live
// changer state with the desired state, and performs a blank check on each loaded
// tape. It returns a LoadedTape per tape in the same order as input.Tapes.
//
// MaximumAttempts is 1: tape moves are physical and non-idempotent.
func (a *LoadActivities) Load(ctx context.Context, input LoadInput) ([]LoadedTape, error) {
	setSize := len(input.Tapes)
	if setSize == 0 {
		return nil, nil
	}

	changer := tape.NewChanger(input.Changer)

	inv, err := changer.Inventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("inventory for load: %w", err)
	}

	if setSize > len(inv.Drives) {
		return nil, fmt.Errorf("drive-set has %d tapes but the library reports only %d drives",
			setSize, len(inv.Drives))
	}

	loaded := make([]LoadedTape, setSize)

	for i, assignment := range input.Tapes {
		stDev := assignment.Drive
		targetSlot := assignment.BlankSlot
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

		blank, err := blankCheckWhenReady(ctx, drive)
		if err != nil {
			return nil, fmt.Errorf("drive %d: blank check for tape %s: %w", i, barcode, err)
		}

		overwroteNonBlank := false

		if !blank {
			if !input.AllowNonBlankTapes {
				return nil, fmt.Errorf("drive %d: tape %s is not blank — refusing to write"+
					" (SPEC §4.3 step 6; reload a blank tape to continue)", i, barcode)
			}

			// The operator deliberately opted in to reclaiming used tapes
			// (Library.AllowNonBlankTapes). Record the irreversible overwrite so the
			// workflow can warn and the run report can note it; blank detection above
			// is unchanged.
			overwroteNonBlank = true
		}

		loaded[i] = LoadedTape{
			Barcode:           barcode,
			DriveIndex:        i,
			TapeIndex:         assignment.TapeIndex,
			CopyIndex:         assignment.CopyIndex,
			SourceSlot:        targetSlot,
			STDevice:          stDev,
			SGDevice:          sgDev,
			OverwroteNonBlank: overwroteNonBlank,
		}
	}

	return loaded, nil
}

const (
	// driveReadyTimeout bounds how long the blank check waits for a freshly
	// loaded drive to become ready. A MOVE MEDIUM completes before the drive has
	// threaded and calibrated the tape; real LTO drives then report NOT READY /
	// BECOMING READY for tens of seconds. This is well under loadTimeout.
	driveReadyTimeout = 3 * time.Minute
	// driveReadyPoll is the interval between blank-check retries while waiting.
	driveReadyPoll = 1 * time.Second
)

// blankCheckWhenReady runs the blank check, retrying while a freshly loaded
// drive is still becoming ready. After a load the drive is not ready instantly,
// so the blank-check rewind can transiently fail with NOT READY; polling until
// the drive answers keeps a slow-loading drive from aborting the run. The wait
// is bounded by driveReadyTimeout and honours ctx cancellation; a genuine media
// or hardware fault persists and surfaces as the final error.
func blankCheckWhenReady(ctx context.Context, drive *tape.Drive) (bool, error) {
	deadline := time.Now().Add(driveReadyTimeout)

	for {
		blank, err := drive.IsBlank(ctx)
		if err == nil {
			return blank, nil
		}

		if ctx.Err() != nil || time.Now().After(deadline) {
			return false, err
		}

		select {
		case <-ctx.Done():
			return false, err
		case <-time.After(driveReadyPoll):
		}
	}
}

// reconcileLoad ensures the drive at driveAddr (which corresponds to the i-th
// drive in the config) is loaded with the tape from targetSlot. It reads the
// current inventory state and issues only the changer moves needed. Returns the
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

// planDriveSets partitions every (logical tape, copy) pair in the plan into
// drive-sets of at most len(drives) physical tapes (SPEC §4.3 phases 6–8). Pairs
// are flattened in (tape, copy) order and chunked, so copies of one logical tape
// stay adjacent and tend to share a set — a set then reads a single staged tree
// (byte-identical copies), which the ZFS ARC coalesces to near-1× disk reads
// (SPEC §14). Drives are reused across sets (drive j holds set 0's tape j, then
// set 1's after eject); each physical tape consumes its own blank slot, so the run
// needs one blank slot per (tape × copy).
//
// It returns an error when the plan cannot be written with the configured drives
// and blank slots. An empty plan yields no sets (and no error).
func planDriveSets(plan TapePlan, drives []string, blankSlots []int) ([]driveSet, error) {
	total := len(plan.Tapes) * plan.Copies
	if total == 0 {
		return nil, nil
	}

	if plan.Copies < 1 {
		return nil, fmt.Errorf("plan has %d copies; must be at least 1", plan.Copies)
	}

	if len(drives) == 0 {
		return nil, fmt.Errorf("no drives configured; cannot write %d physical tapes", total)
	}

	if total > len(blankSlots) {
		return nil, fmt.Errorf("plan requires %d physical tapes but only %d blank slots are configured",
			total, len(blankSlots))
	}

	var (
		sets    []driveSet
		current driveSet
		slot    int
	)

	for tapeIndex := range plan.Tapes {
		for copyIndex := 0; copyIndex < plan.Copies; copyIndex++ {
			current = append(current, TapeAssignment{
				Drive:     drives[len(current)],
				BlankSlot: blankSlots[slot],
				TapeIndex: tapeIndex,
				CopyIndex: copyIndex,
			})
			slot++

			if len(current) == len(drives) {
				sets = append(sets, current)
				current = nil
			}
		}
	}

	if len(current) > 0 {
		sets = append(sets, current)
	}

	return sets, nil
}

// loadPhase orchestrates the Load phase (SPEC §4.3 phase 6) for one drive-set: it
// dispatches the Load activity on the data worker and returns the loaded tapes for
// the Write and Eject phases. runTapePath calls it once per drive-set.
func loadPhase(ctx workflow.Context, cfg config.Config, set driveSet) ([]LoadedTape, error) {
	if len(set) == 0 {
		return nil, nil
	}

	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: loadTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})

	input := LoadInput{
		Changer:            cfg.Library.Changer,
		Tapes:              set,
		AllowNonBlankTapes: cfg.Library.AllowNonBlankTapes,
	}

	var acts *LoadActivities

	var loaded []LoadedTape
	if err := workflow.ExecuteActivity(dataCtx, acts.Load, input).Get(dataCtx, &loaded); err != nil {
		return nil, err
	}

	// Overwriting a non-blank tape is deliberate (Library.AllowNonBlankTapes) but
	// irreversible, so surface it loudly in the run's durable log — naming the
	// barcode and slot whose existing data is being destroyed (SPEC §4.3 step 6).
	for _, lt := range loaded {
		if lt.OverwroteNonBlank {
			workflow.GetLogger(ctx).Warn("overwriting a NON-BLANK tape "+
				"(Library.AllowNonBlankTapes is set); existing data will be destroyed",
				"barcode", lt.Barcode, "slot", lt.SourceSlot, "drive", lt.DriveIndex)
		}
	}

	return loaded, nil
}

// EjectInput is the payload for the Eject activity.
type EjectInput struct {
	// Changer is the changer device node (e.g. /dev/sch0).
	Changer string
	// WrittenTapes are the tapes to eject, each carrying its DriveIndex and
	// SourceSlot for unload + transfer.
	WrittenTapes []WrittenTape
}

// IOStatusInput is the payload for the IOStationStatus activity.
type IOStatusInput struct {
	// Changer is the changer device node (e.g. /dev/sch0).
	Changer string
}

// EjectActivities hosts the Eject and IOStationStatus activities.
type EjectActivities struct{}

// newEjectActivities returns the Eject activities.
func newEjectActivities() *EjectActivities { return &EjectActivities{} }

// Eject unloads each written tape from its drive back to its source slot and
// transfers it to a free I/O station slot. It reads live changer state first and
// reconciles, handling the case where a tape was already unloaded or already
// transferred before this activity ran.
//
// When the I/O station has no free slot, Eject does not fail: it still unloads
// each tape from its drive to its source storage slot (so no tape is ever left in
// a drive) and returns the tapes it could not export in EjectResult.Remaining, so
// the workflow can pause for the operator and retry. Passing that Remaining set
// back on a later call resumes the export into the freed slots.
//
// MaximumAttempts is 1: tape moves are physical and non-idempotent.
func (a *EjectActivities) Eject(ctx context.Context, input EjectInput) (EjectResult, error) {
	if len(input.WrittenTapes) == 0 {
		return EjectResult{}, nil
	}

	changer := tape.NewChanger(input.Changer)

	inv, err := changer.Inventory(ctx)
	if err != nil {
		return EjectResult{}, fmt.Errorf("inventory for eject: %w", err)
	}

	var remaining []WrittenTape

	for i, wt := range input.WrittenTapes {
		// Re-read inventory before each move (after the first) so this iteration
		// sees the freed drive and consumed I/O slot from the previous move.
		if i > 0 {
			inv, err = changer.Inventory(ctx)
			if err != nil {
				return EjectResult{}, fmt.Errorf("inventory before ejecting tape %s: %w", wt.Barcode, err)
			}
		}

		exported, err := ejectTape(ctx, changer, inv, wt)
		if err != nil {
			return EjectResult{}, fmt.Errorf("eject tape %s (drive %d, index %d): %w", wt.Barcode, wt.DriveIndex, i, err)
		}

		if !exported {
			remaining = append(remaining, wt)
		}
	}

	// Re-read once more to report the tapes now resting in the I/O station.
	inv, err = changer.Inventory(ctx)
	if err != nil {
		return EjectResult{}, fmt.Errorf("inventory after eject: %w", err)
	}

	return EjectResult{
		InIOStation: barcodesInIOStation(inv),
		Remaining:   remaining,
	}, nil
}

// IOStationStatus reads the changer and returns a read-only snapshot of the
// import/export station (free slot count and access state). The workflow polls it
// while paused in Eject to decide whether the operator has cleared and closed the
// station so the run can resume automatically (SPEC §4.3 phase 8). It moves no
// media, so it carries the default retry policy — a transient read failure is
// safe to retry.
func (a *EjectActivities) IOStationStatus(ctx context.Context, input IOStatusInput) (IOStatus, error) {
	changer := tape.NewChanger(input.Changer)

	inv, err := changer.Inventory(ctx)
	if err != nil {
		return IOStatus{}, fmt.Errorf("inventory for I/O station status: %w", err)
	}

	return ioStatus(inv), nil
}

// ejectTape ejects a single written tape to an I/O station slot and reports
// whether it reached one. It reconciles the live drive state:
//   - Tape already in an I/O slot → exported, no move.
//   - Drive loaded with this tape → unload to SourceSlot, then transfer if a slot
//     is free.
//   - Tape already in SourceSlot (drive empty) → transfer if a slot is free.
//
// The unload always precedes the I/O-slot check, so a tape is moved out of its
// drive even when the station is full — it then waits in its source storage slot
// and is reported as not exported (returns false) for the workflow to retry.
func ejectTape(ctx context.Context, changer *tape.Changer, inv tape.Inventory, wt WrittenTape) (bool, error) {
	// Check if already in an I/O slot.
	for _, io := range inv.IOSlots {
		if io.Full && io.Barcode == wt.Barcode {
			return true, nil
		}
	}

	// Unload from drive to source slot if the drive still holds this tape.
	if wt.DriveIndex < len(inv.Drives) {
		drive := inv.Drives[wt.DriveIndex]
		if drive.Loaded && drive.Barcode == wt.Barcode {
			if err := changer.Unload(ctx, wt.SourceSlot, drive.Address); err != nil {
				return false, fmt.Errorf("unload tape %s from drive %d to slot %d: %w",
					wt.Barcode, wt.DriveIndex, wt.SourceSlot, err)
			}
		}
	}

	// Transfer from source slot to a free I/O slot when one is available;
	// otherwise leave the tape in its source storage slot for a later retry.
	ioSlot := findFreeIOSlot(inv)
	if ioSlot == -1 {
		return false, nil
	}

	if err := changer.Transfer(ctx, wt.SourceSlot, ioSlot); err != nil {
		return false, fmt.Errorf("transfer tape %s from slot %d to I/O slot %d: %w",
			wt.Barcode, wt.SourceSlot, ioSlot, err)
	}

	return true, nil
}

// barcodesInIOStation returns the barcodes of every tape currently occupying an
// I/O-station slot — the tapes ready for the operator to remove.
func barcodesInIOStation(inv tape.Inventory) []tape.Barcode {
	var inIO []tape.Barcode

	for _, io := range inv.IOSlots {
		if io.Full {
			inIO = append(inIO, io.Barcode)
		}
	}

	return inIO
}

// ioStatus derives the import/export station snapshot the paused Eject phase polls
// from a live inventory: the free-slot count and, when the library reports it, the
// access state (StationClosed is true only when every I/O slot is accessible to
// the changer robot — the operator has closed the station).
func ioStatus(inv tape.Inventory) IOStatus {
	free := 0
	closed := true

	for _, io := range inv.IOSlots {
		if !io.Full {
			free++
		}

		if !io.Accessible {
			closed = false
		}
	}

	return IOStatus{
		FreeSlots:      free,
		AccessReported: inv.IOAccessReported,
		StationClosed:  inv.IOAccessReported && closed,
	}
}

const (
	// ioStationPollInterval is how often a paused Eject phase re-reads the I/O
	// station to detect (on libraries that report access state) that the operator
	// has cleared and closed it, so the run resumes without an explicit signal.
	ioStationPollInterval = 30 * time.Second
	// ioStatusTimeout bounds one IOStationStatus poll — a single READ ELEMENT STATUS.
	ioStatusTimeout = 30 * time.Second
	// ioStatusMaxAttempts retries a transient poll read a few times before giving
	// up; a persistently unreadable changer during the pause fails the run.
	ioStatusMaxAttempts = 3
)

// ejectPhase orchestrates the Eject phase (SPEC §4.3 phase 8) for one drive-set:
// it dispatches the Eject activity on the data worker to transfer this set's
// written tapes from their drives to I/O station slots for physical removal, and
// frees the drives for the next drive-set. runTapePath calls it once per set.
//
// When the I/O station fills before every tape is exported, the phase becomes
// operator-in-the-loop: it notifies the operator which tapes to remove, then waits
// — resuming automatically once the library reports the station cleared and closed
// (waitForIOCleared), or on the explicit OperatorResumeSignal — and retries
// the remaining tapes into the freed slots. If the operator never responds within
// the configured wait, it fails the run; every written tape is by then in an I/O
// or storage slot and none is in a drive.
func ejectPhase(ctx workflow.Context, cfg config.Config, written []WrittenTape) error {
	if len(written) == 0 {
		return nil
	}

	remaining := written

	for {
		result, err := runEject(ctx, cfg, remaining)
		if err != nil {
			return err
		}

		remaining = result.Remaining
		if len(remaining) == 0 {
			return nil
		}

		// The I/O station is full with tapes still to export. Alert the operator
		// which tapes to remove, then pause until the station is cleared.
		notifyOperatorPause(ctx, result.InIOStation, len(remaining))

		resumed, err := waitForIOCleared(ctx, cfg)
		if err != nil {
			return err
		}

		if !resumed {
			return fmt.Errorf("operator did not clear the import/export station within %s; "+
				"%d written tape(s) remain in storage slots awaiting export (none is in a drive)",
				cfg.Library.EffectiveIOWaitTimeout(), len(remaining))
		}
	}
}

// runEject dispatches one Eject activity call on the data worker and returns its
// result. MaximumAttempts is 1: tape moves are physical and non-idempotent.
func runEject(ctx workflow.Context, cfg config.Config, tapes []WrittenTape) (EjectResult, error) {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: ejectTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})

	input := EjectInput{
		Changer:      cfg.Library.Changer,
		WrittenTapes: tapes,
	}

	var acts *EjectActivities

	var result EjectResult
	if err := workflow.ExecuteActivity(dataCtx, acts.Eject, input).Get(dataCtx, &result); err != nil {
		return EjectResult{}, err
	}

	return result, nil
}

// waitForIOCleared pauses the Eject phase until the operator has cleared the I/O
// station, returning true to resume and false when the configured wait elapses
// first. It selects over three futures: the OperatorResumeSignal (explicit
// operator resume), an overall wait-timeout timer, and a repeating poll timer. On
// each poll it reads the I/O station; libraries that report access state resume
// automatically once the station is closed with a free slot (IOStatus.CanAutoResume),
// while libraries that do not report it wait for the signal or the timeout.
func waitForIOCleared(ctx workflow.Context, cfg config.Config) (bool, error) {
	drainStaleResumeSignals(ctx)

	signalCh := workflow.GetSignalChannel(ctx, OperatorResumeSignal)
	timeoutTimer := workflow.NewTimer(ctx, cfg.Library.EffectiveIOWaitTimeout())

	for {
		var signalled, timedOut, polled bool

		selector := workflow.NewSelector(ctx)
		selector.AddReceive(signalCh, func(c workflow.ReceiveChannel, _ bool) {
			c.Receive(ctx, nil)

			signalled = true
		})
		selector.AddFuture(timeoutTimer, func(workflow.Future) {
			timedOut = true
		})
		selector.AddFuture(workflow.NewTimer(ctx, ioStationPollInterval), func(workflow.Future) {
			polled = true
		})

		selector.Select(ctx)

		switch {
		case signalled:
			return true, nil
		case timedOut:
			return false, nil
		case polled:
			status, err := runIOStationStatus(ctx, cfg)
			if err != nil {
				return false, err
			}

			if status.CanAutoResume() {
				return true, nil
			}
		}
	}
}

// runIOStationStatus dispatches one IOStationStatus poll on the data worker. It
// moves no media and is safe to retry, so it carries a small retry budget rather
// than MaximumAttempts: 1.
func runIOStationStatus(ctx workflow.Context, cfg config.Config) (IOStatus, error) {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: ioStatusTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: ioStatusMaxAttempts},
	})

	input := IOStatusInput{Changer: cfg.Library.Changer}

	var acts *EjectActivities

	var status IOStatus
	if err := workflow.ExecuteActivity(dataCtx, acts.IOStationStatus, input).Get(dataCtx, &status); err != nil {
		return IOStatus{}, err
	}

	return status, nil
}

// notifyOperatorPause alerts the operator that the Eject phase paused because the
// I/O station filled (SPEC §4.3 phase 8, §11). It runs the operational alert on
// the control worker and is best-effort: a delivery failure is logged, never
// propagated, so a webhook outage does not abort a run that is only waiting.
func notifyOperatorPause(ctx workflow.Context, readyForRemoval []tape.Barcode, awaiting int) {
	input := OperatorPauseInput{
		RunID:           workflow.GetInfo(ctx).WorkflowExecution.ID,
		ReadyForRemoval: barcodeStrings(readyForRemoval),
		Awaiting:        awaiting,
	}

	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           TaskQueue,
		StartToCloseTimeout: operatorPauseAlertTimeout,
	})

	var acts *FailureActivities
	if err := workflow.ExecuteActivity(actx, acts.NotifyOperatorPause, input).Get(actx, nil); err != nil {
		workflow.GetLogger(ctx).Error("failed to deliver operator-pause alert",
			"awaiting", awaiting,
			"error", err,
		)
	}
}

// barcodeStrings converts a slice of tape barcodes to plain strings for an
// activity payload and operator-facing message.
func barcodeStrings(barcodes []tape.Barcode) []string {
	out := make([]string, len(barcodes))
	for i, barcode := range barcodes {
		out[i] = string(barcode)
	}

	return out
}
