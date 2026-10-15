package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// Load (SPEC §4.3 phase 6) moves blank tapes into the drives and confirms each
// is blank before any write; it reconciles live changer state, so it is
// idempotent. Eject (phase 8) moves written tapes to the I/O station. Both run a
// single attempt: tape moves are physical and non-idempotent.

const (
	// loadTimeout covers loads plus blank-check rewinds, minutes each on real
	// hardware.
	loadTimeout  = 30 * time.Minute
	ejectTimeout = 10 * time.Minute
)

// LoadInput is the payload for the Load activity: one drive-set of at most
// len(inv.Drives) tapes (SPEC §4.3 phase 6).
type LoadInput struct {
	Changer string
	// Tapes are the drive-set's physical tapes; Tapes[i] goes into the i-th drive.
	Tapes []TapeAssignment
	// AllowNonBlankTapes mirrors Library.AllowNonBlankTapes: a non-blank tape is
	// overwritten with a warning instead of failing the run. The blank check always
	// runs (SPEC §4.3 step 6).
	AllowNonBlankTapes bool
}

// LoadActivities hosts the Load activity.
type LoadActivities struct{}

func newLoadActivities() *LoadActivities { return &LoadActivities{} }

// Load moves this drive-set's blank tapes into the drives and blank-checks each,
// returning a LoadedTape per input tape in order. Single attempt.
func (a *LoadActivities) Load(ctx context.Context, input LoadInput) (_ []LoadedTape, err error) {
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

	slog.InfoContext(ctx, "load: loading blank tapes into drives and blank-checking them", "tapes", setSize)

	// relocated maps storage slots filled by this Load's relocations to the barcode
	// moved there. The inventory is read once, so this keeps two relocations from
	// targeting the same free slot and lets a later target-slot lookup see a tape
	// the stale snapshot shows as absent (#224).
	relocated := make(map[int]tape.Barcode)

	// A failed Load returns what it loaded to the slots they came from, so a
	// refused tape is never left in a drive.
	type loadedInto struct{ slot, drive int }

	var inDrives []loadedInto

	defer func() {
		if err == nil {
			return
		}

		for _, loaded := range inDrives {
			if unloadErr := changer.Unload(context.WithoutCancel(ctx), loaded.slot, loaded.drive); unloadErr != nil {
				slog.WarnContext(ctx, "load: could not return a tape to its slot after a failed load",
					"slot", loaded.slot, "drive", loaded.drive, "error", unloadErr)
			}
		}
	}()

	for i, assignment := range input.Tapes {
		stDev := assignment.Drive
		targetSlot := assignment.BlankSlot

		drive := tape.NewDrive(stDev)

		// Pair by unit serial, not set position: a retry set or kernel probe order
		// can otherwise load one drive while checking another (#137).
		de, err := driveElementFor(ctx, drive, inv)
		if err != nil {
			return nil, fmt.Errorf("load drive %d (%s): %w", assignment.DriveIndex, stDev, err)
		}

		barcode, err := reconcileLoad(ctx, changer, inv, de, targetSlot, relocated)
		if err != nil {
			return nil, fmt.Errorf("load drive %d (%s, slot %d): %w", assignment.DriveIndex, stDev, targetSlot, err)
		}

		inDrives = append(inDrives, loadedInto{slot: targetSlot, drive: de.Address})

		sgDev, err := drive.SGDevice()
		if err != nil {
			return nil, fmt.Errorf("drive %d (%s): resolve SCSI generic node: %w", assignment.DriveIndex, stDev, err)
		}

		blank, err := blankCheckWhenReady(ctx, drive, driveReadyTimeout, driveReadyPoll)
		if err != nil {
			return nil, fmt.Errorf("drive %d (%s): blank check for tape %s: %w", assignment.DriveIndex, stDev, barcode, err)
		}

		overwroteNonBlank := false

		if !blank {
			if !input.AllowNonBlankTapes {
				return nil, temporal.NewNonRetryableApplicationError(fmt.Sprintf(
					"drive %d (%s): tape %s is not blank — refusing to write"+
						" (SPEC §4.3 step 6; it was returned to slot %d — replace it with a blank to continue)",
					assignment.DriveIndex, stDev, barcode, targetSlot), nonBlankTapeErrorType, nil, string(barcode), targetSlot)
			}

			// Operator opted in to reclaiming used tapes; record the irreversible
			// overwrite for the warning and report.
			overwroteNonBlank = true
		}

		loaded[i] = LoadedTape{
			Barcode:           barcode,
			DriveIndex:        assignment.DriveIndex,
			TapeIndex:         assignment.TapeIndex,
			CopyIndex:         assignment.CopyIndex,
			SourceSlot:        targetSlot,
			STDevice:          stDev,
			SGDevice:          sgDev,
			OverwroteNonBlank: overwroteNonBlank,
		}

		slog.InfoContext(ctx, "load: tape loaded and blank-checked",
			"barcode", barcode, "drive", assignment.DriveIndex, "slot", targetSlot,
			"blank", blank, "device", stDev)
	}

	return loaded, nil
}

// driveElementFor pairs a drive node to its changer data-transfer element by
// unit serial (INQUIRY VPD 0x80, no media motion) vs. the changer's DVCID
// (#137). It never falls back to set position.
func driveElementFor(ctx context.Context, drive *tape.Drive, inv tape.Inventory) (tape.DriveElement, error) {
	info, err := drive.Inquire(ctx)
	if err != nil {
		return tape.DriveElement{}, fmt.Errorf("read drive identity (INQUIRY): %w", err)
	}

	return matchDriveElement(inv, info.Serial)
}

// matchDriveElement returns the single element whose DVCID serial equals
// serial, failing on an empty serial, no match, or an ambiguous match.
func matchDriveElement(inv tape.Inventory, serial string) (tape.DriveElement, error) {
	if serial == "" {
		return tape.DriveElement{}, errors.New("cannot pair device node to a changer element: " +
			"no drive unit serial (INQUIRY VPD 0x80) or changer DVCID identity reported (issue #137)")
	}

	match := -1

	for i, de := range inv.Drives {
		if de.Serial != serial {
			continue
		}

		if match != -1 {
			return tape.DriveElement{}, fmt.Errorf("drive serial %q matches more than one changer element (drives %d and %d)",
				serial, inv.Drives[match].Address, de.Address)
		}

		match = i
	}

	if match == -1 {
		return tape.DriveElement{}, fmt.Errorf("drive serial %q matches no changer data-transfer element "+
			"(the changer reported serials %s)", serial, driveSerials(inv))
	}

	return inv.Drives[match], nil
}

func driveSerials(inv tape.Inventory) string {
	parts := make([]string, len(inv.Drives))
	for i, de := range inv.Drives {
		parts[i] = fmt.Sprintf("drive %d=%q", de.Address, de.Serial)
	}

	return "[" + strings.Join(parts, ", ") + "]"
}

const (
	// driveReadyTimeout bounds the wait for a freshly loaded drive: MOVE MEDIUM
	// returns before the drive has threaded the tape, and real LTO drives report
	// NOT READY for tens of seconds afterwards.
	driveReadyTimeout = 3 * time.Minute
	driveReadyPoll    = 1 * time.Second
)

// blankChecker is the drive capability blankCheckWhenReady needs.
type blankChecker interface {
	IsBlank(ctx context.Context) (bool, error)
}

// blankCheckWhenReady runs the blank check, retrying while the drive is still
// becoming ready after a load, until timeout; a persistent fault surfaces as the
// final error.
func blankCheckWhenReady(ctx context.Context, drive blankChecker, timeout, poll time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)

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
		case <-time.After(poll):
		}
	}
}

// reconcileLoad loads the tape from targetSlot into de (already paired by drive
// identity, #137), issuing only the moves needed: skip when already loaded,
// load when empty, relocate an unexpected tape first. It returns the barcode
// that ends up in the drive; the blank check always follows. relocated is
// updated and consulted as described in Load (#224).
func reconcileLoad(ctx context.Context, changer *tape.Changer, inv tape.Inventory, de tape.DriveElement, targetSlot int, relocated map[int]tape.Barcode) (tape.Barcode, error) {
	driveAddr := de.Address

	if de.Loaded {
		if de.SourceSlot == targetSlot {
			return de.Barcode, nil
		}

		// A tape with a live LTFS mount asserts PREVENT MEDIUM REMOVAL, so its unload
		// fails clearly instead of yanking a live tape.
		relocateSlot := findFreeStorageSlot(inv, de.SourceSlot, relocated)
		if relocateSlot == -1 {
			return "", fmt.Errorf("drive has unexpected tape %s (from slot %d) and no free storage slot to relocate it",
				de.Barcode, de.SourceSlot)
		}

		// Claim before moving so later drives in this Load see it.
		relocated[relocateSlot] = de.Barcode

		if err := changer.Unload(ctx, relocateSlot, driveAddr); err != nil {
			return "", fmt.Errorf("relocate unexpected tape %s (from slot %d) to slot %d: %w",
				de.Barcode, de.SourceSlot, relocateSlot, err)
		}
	}

	// Check this Load's relocations first: the snapshot may show the slot empty
	// though an earlier relocation filled it (#224).
	targetBarcode, ok := relocated[targetSlot]
	if !ok {
		targetBarcode, ok = slotBarcode(inv, targetSlot)
	}

	if !ok {
		// Name where the tapes actually are, so the error alone is enough to pick
		// valid slots or see the library needs restocking.
		return "", fmt.Errorf(
			"target slot %d is empty; cannot load a blank tape "+
				"(storage slots holding tapes: %s; tapes in drives: %d; tapes in I/O station: %d)",
			targetSlot,
			formatSlotRanges(occupiedStorageSlots(inv, relocated)),
			loadedDriveCount(inv),
			occupiedIOCount(inv))
	}

	if err := changer.Load(ctx, targetSlot, driveAddr); err != nil {
		return "", fmt.Errorf("load tape %s from slot %d: %w", targetBarcode, targetSlot, err)
	}

	return targetBarcode, nil
}

// occupiedStorageSlots returns the ascending addresses of occupied storage
// slots, including this Load's relocations.
func occupiedStorageSlots(inv tape.Inventory, relocated map[int]tape.Barcode) []int {
	occupied := make(map[int]struct{}, len(inv.Slots))

	for _, slot := range inv.Slots {
		if slot.Full {
			occupied[slot.Address] = struct{}{}
		}
	}

	for address := range relocated {
		occupied[address] = struct{}{}
	}

	addresses := make([]int, 0, len(occupied))
	for address := range occupied {
		addresses = append(addresses, address)
	}

	sort.Ints(addresses)

	return addresses
}

func loadedDriveCount(inv tape.Inventory) int {
	var count int

	for _, drive := range inv.Drives {
		if drive.Loaded {
			count++
		}
	}

	return count
}

func occupiedIOCount(inv tape.Inventory) int {
	var count int

	for _, ioSlot := range inv.IOSlots {
		if ioSlot.Full {
			count++
		}
	}

	return count
}

// formatSlotRanges renders ascending slot addresses compactly ("1-19, 23-47"),
// or "none".
func formatSlotRanges(addresses []int) string {
	if len(addresses) == 0 {
		return "none"
	}

	var (
		parts []string
		start = addresses[0]
		prev  = addresses[0]
	)

	for _, address := range addresses[1:] {
		if address == prev+1 {
			prev = address

			continue
		}

		parts = append(parts, formatSlotRange(start, prev))
		start, prev = address, address
	}

	parts = append(parts, formatSlotRange(start, prev))

	return strings.Join(parts, ", ")
}

func formatSlotRange(start, end int) string {
	if start == end {
		return strconv.Itoa(start)
	}

	return fmt.Sprintf("%d-%d", start, end)
}

// findFreeStorageSlot returns an empty storage slot, preferring preferSlot and
// skipping slots claimed in relocated, or -1 if none.
func findFreeStorageSlot(inv tape.Inventory, preferSlot int, relocated map[int]tape.Barcode) int {
	for _, slot := range inv.Slots {
		if _, claimed := relocated[slot.Address]; slot.Address == preferSlot && !slot.Full && !claimed {
			return slot.Address
		}
	}

	for _, slot := range inv.Slots {
		if _, claimed := relocated[slot.Address]; !slot.Full && !claimed {
			return slot.Address
		}
	}

	return -1
}

// findFreeIOSlot returns an empty I/O station slot, or -1.
func findFreeIOSlot(inv tape.Inventory) int {
	for _, slot := range inv.IOSlots {
		if !slot.Full {
			return slot.Address
		}
	}

	return -1
}

// slotBarcode returns the barcode in storage slot addr, if occupied. Slot
// addresses are not guaranteed contiguous, so it scans.
func slotBarcode(inv tape.Inventory, addr int) (tape.Barcode, bool) {
	for _, slot := range inv.Slots {
		if slot.Address == addr {
			return slot.Barcode, slot.Full
		}
	}

	return "", false
}

// planDriveSets partitions every (tape, copy) pair into drive-sets of at most
// len(drives) tapes (SPEC §4.3 phases 6–8). Pairs are chunked in (tape, copy)
// order so copies of one tape tend to share a set and the ZFS ARC coalesces
// their identical reads (SPEC §14). Each physical tape needs its own blank slot.
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
				Drive:      drives[len(current)],
				DriveIndex: len(current),
				BlankSlot:  blankSlots[slot],
				TapeIndex:  tapeIndex,
				CopyIndex:  copyIndex,
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

// loadPhase runs the Load activity for one drive-set (SPEC §4.3 phase 6).
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

	// Overwriting is deliberate but irreversible: log it loudly with barcode and
	// slot (SPEC §4.3 step 6).
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
	Changer      string
	WrittenTapes []WrittenTape
}

// IOStatusInput is the payload for the IOStationStatus activity.
type IOStatusInput struct {
	Changer string
}

// EjectActivities hosts the Eject and IOStationStatus activities.
type EjectActivities struct{}

func newEjectActivities() *EjectActivities { return &EjectActivities{} }

// Eject moves each written tape from its drive to a free I/O station slot,
// reconciling against live changer state. When the station is full it still
// unloads every tape to its source slot (never leaving one in a drive) and
// returns the rest in EjectResult.Remaining for a later retry. Single attempt.
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
		// Re-read so this iteration sees the previous move.
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

		if exported {
			slog.InfoContext(ctx, "eject: written tape exported to the I/O station for removal",
				"barcode", wt.Barcode, "drive", wt.DriveIndex)
		} else {
			remaining = append(remaining, wt)

			slog.InfoContext(ctx, "eject: I/O station full; written tape unloaded to its storage slot to await export",
				"barcode", wt.Barcode, "drive", wt.DriveIndex, "slot", wt.SourceSlot)
		}
	}

	inv, err = changer.Inventory(ctx)
	if err != nil {
		return EjectResult{}, fmt.Errorf("inventory after eject: %w", err)
	}

	inIOStation := barcodesInIOStation(inv)

	slog.InfoContext(ctx, "eject: drive-set eject complete",
		"exported", len(input.WrittenTapes)-len(remaining), "awaitingExport", len(remaining), "inIOStation", len(inIOStation))

	return EjectResult{
		InIOStation: inIOStation,
		Remaining:   remaining,
	}, nil
}

// IOStationStatus returns a read-only snapshot of the I/O station, polled while
// Eject is paused to detect auto-resume (SPEC §4.3 phase 8). It moves no media,
// so it is safe to retry.
func (a *EjectActivities) IOStationStatus(ctx context.Context, input IOStatusInput) (IOStatus, error) {
	changer := tape.NewChanger(input.Changer)

	inv, err := changer.Inventory(ctx)
	if err != nil {
		return IOStatus{}, fmt.Errorf("inventory for I/O station status: %w", err)
	}

	return ioStatus(inv), nil
}

// ejectTape moves one written tape to an I/O slot and reports whether it got
// there. The drive is always unloaded first, so even with a full station the
// tape waits in its source slot rather than a drive.
func ejectTape(ctx context.Context, changer *tape.Changer, inv tape.Inventory, wt WrittenTape) (bool, error) {
	for _, io := range inv.IOSlots {
		if io.Full && io.Barcode == wt.Barcode {
			return true, nil
		}
	}

	// Find the drive by barcode, not wt.DriveIndex: the config index is not the
	// changer's element position on a probe-order-mismatched host (#137).
	for _, drive := range inv.Drives {
		if drive.Loaded && drive.Barcode == wt.Barcode {
			if err := changer.Unload(ctx, wt.SourceSlot, drive.Address); err != nil {
				return false, fmt.Errorf("unload tape %s from drive %d to slot %d: %w",
					wt.Barcode, wt.DriveIndex, wt.SourceSlot, err)
			}

			break
		}
	}

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

// barcodesInIOStation returns the barcodes in the I/O station.
func barcodesInIOStation(inv tape.Inventory) []tape.Barcode {
	var inIO []tape.Barcode

	for _, io := range inv.IOSlots {
		if io.Full {
			inIO = append(inIO, io.Barcode)
		}
	}

	return inIO
}

// ioStatus derives the station snapshot from inv. StationClosed is true only
// when every I/O slot is accessible to the robot.
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
	// ioStationPollInterval is how often a paused Eject re-reads the I/O station.
	ioStationPollInterval = 30 * time.Second
	ioStatusTimeout       = 30 * time.Second
	// ioStatusMaxAttempts: a changer that stays unreadable during the pause fails
	// the run.
	ioStatusMaxAttempts = 3
)

// ejectPhase runs Eject for one drive-set (SPEC §4.3 phase 8). When the I/O
// station fills, it alerts the operator and waits for the station to be cleared
// (auto-detected or OperatorResumeSignal), then retries the rest; on timeout it
// fails with every tape in a slot, none in a drive. state.currentPause is
// PauseEject for the duration of the wait.
func ejectPhase(ctx workflow.Context, cfg config.Config, state *runState, written []WrittenTape) error {
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

		// Drain stale resume/abort before the alert so only pre-alert signals are
		// discarded (#216, #254). The wait ignores abort (an Eject pause rejects it);
		// draining keeps a stale abort from leaking onto a later pause.
		drainStalePauseSignals(ctx)

		notifyOperatorPause(ctx, result.InIOStation, len(remaining))

		state.currentPause = CurrentPause{
			Kind:           PauseEject,
			AffectedTapes:  barcodeStrings(result.InIOStation),
			AwaitingExport: len(remaining),
		}

		resumed, err := waitForIOCleared(ctx, cfg)

		state.currentPause = CurrentPause{}

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

// runEject runs one Eject activity on the data worker (single attempt).
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

// waitForIOCleared pauses until the operator clears the I/O station, returning
// false if the configured wait elapses first. The poll loop runs in a
// self-continuing child workflow so the run's history grows O(1) per pause
// (#168); the parent selects on OperatorResumeSignal and the child. Stale
// signals are drained by the caller before the alert (#216), not here.
func waitForIOCleared(ctx workflow.Context, cfg config.Config) (bool, error) {
	signalCh := workflow.GetSignalChannel(ctx, OperatorResumeSignal)

	// Absolute deadline, carried through the child's continuations so the budget
	// never drifts.
	deadline := workflow.Now(ctx).Add(cfg.Library.EffectiveIOWaitTimeout())

	// ParentClosePolicy TERMINATE ties the child to this run (no cross-run state,
	// SPEC §4.2).
	childCtx, cancelChild := workflow.WithCancel(ctx)
	defer cancelChild()

	childCtx = workflow.WithChildOptions(childCtx, workflow.ChildWorkflowOptions{
		TaskQueue:         TaskQueue,
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_TERMINATE,
	})

	childFuture := workflow.ExecuteChildWorkflow(childCtx, ioStationWaitWorkflow, ioStationWaitInput{
		Cfg:      cfg,
		Deadline: deadline,
	})

	var (
		signalled bool
		resumed   bool
		childErr  error
	)

	selector := workflow.NewSelector(ctx)
	selector.AddReceive(signalCh, func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, nil)

		signalled = true
	})
	selector.AddFuture(childFuture, func(f workflow.Future) {
		childErr = f.Get(ctx, &resumed)
	})

	selector.Select(ctx)

	if signalled {
		cancelChild()

		return true, nil
	}

	if childErr != nil {
		return false, childErr
	}

	return resumed, nil
}

// runIOStationStatus runs one IOStationStatus poll with a small retry budget
// (it moves no media).
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

// notifyOperatorPause alerts the operator that Eject paused on a full I/O
// station (SPEC §11). Best-effort: a webhook outage must not abort a waiting
// run.
func notifyOperatorPause(ctx workflow.Context, readyForRemoval []tape.Barcode, awaiting int) {
	input := OperatorPauseInput{
		DryRun:          isDryRun(ctx),
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

func barcodeStrings(barcodes []tape.Barcode) []string {
	out := make([]string, len(barcodes))
	for i, barcode := range barcodes {
		out[i] = string(barcode)
	}

	return out
}
