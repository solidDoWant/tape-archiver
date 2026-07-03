//go:build integration

package backup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// TestTapePath exercises the Load → Write (Format/WriteTree/Finalize) → Eject
// activity sequence end-to-end against mhvtl. Each activity is called directly
// (without a Temporal worker) to keep the test self-contained.
//
// Covers AC4 of issue #54 (full tape path against the virtual library, skips
// when mhvtl or LTFS is absent).
func TestTapePath(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)
	testutil.SkipIfLTFSUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "initial inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "at least 1 drive required")
	require.NotEmpty(t, inv.Slots, "at least 1 storage slot required")
	require.NotEmpty(t, inv.IOSlots, "at least 1 I/O slot required")

	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")
	// Use slot index 1 (the second slot) so this test does not conflict with
	// TestSessionSplitWriteWithFinalizeRetry, which uses slot index 0 and formats
	// the tape there — leaving it non-blank for any subsequent test.
	require.GreaterOrEqual(t, len(inv.Slots), 2, "at least 2 storage slots required")
	require.True(t, inv.Slots[1].Full, "slot 1 must have a tape")

	stDev := testutil.Drive0Dev(t)
	sgDev := testutil.Drive0SgDev(t)
	slotAddr := inv.Slots[1].Address
	driveAddr := inv.Drives[0].Address
	expectedBarcode := inv.Slots[1].Barcode
	require.NotEmpty(t, expectedBarcode, "slot 1 tape must have a barcode")

	// Ensure the library is in a clean state when the test ends: the tape must be
	// back in its storage slot AND blank so that consecutive test runs start from
	// the same state. The vtltape daemon caches tape content in memory; after
	// mkltfs formats a tape the daemon reports it as non-blank even if mktape
	// recreates the disk files. A short SCSI ERASE (LONG=0, sent via sg_raw)
	// resets the daemon's in-memory state to blank without a long physical erase.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 120*time.Second)
		defer cancel()

		cleanupInv, invErr := changer.Inventory(cleanupCtx)
		if invErr != nil {
			return
		}

		tapeInDrive := len(cleanupInv.Drives) > 0 &&
			cleanupInv.Drives[0].Loaded &&
			cleanupInv.Drives[0].Barcode == expectedBarcode

		if !tapeInDrive {
			// If tape is in an I/O slot (normal after Eject), move it to storage first.
			for _, io := range cleanupInv.IOSlots {
				if io.Full && io.Barcode == expectedBarcode {
					_ = changer.Transfer(cleanupCtx, io.Address, slotAddr)
					break
				}
			}
			// Load from storage slot into drive for the erase step.
			if err := changer.Load(cleanupCtx, slotAddr, driveAddr); err != nil {
				return
			}
		}

		// Short SCSI ERASE (CDB 0x19, LONG=0) rewinds and truncates at BOT,
		// resetting the vtltape daemon's in-memory state to blank. Bound the
		// rewind: on a drive that is stuck "not ready" the st-node rewind blocks
		// until the medium becomes ready, which would otherwise hang cleanup for
		// the full 120s budget.
		rewindCtx, rewindCancel := context.WithTimeout(cleanupCtx, 10*time.Second)
		_ = exec.CommandContext(rewindCtx, "mt", "-f", stDev, "rewind").Run()

		rewindCancel()

		_ = exec.CommandContext(cleanupCtx, "sg_raw", sgDev,
			"0x19", "0x00", "0x00", "0x00", "0x00", "0x00").Run()
		_ = changer.Unload(cleanupCtx, slotAddr, driveAddr)
	})

	// Probe drive readiness up front. On some kernels mhvtl leaves a freshly
	// loaded drive stuck "not ready"; the Load activity's blank check would then
	// fail rather than skip. Load the target tape and wait for the drive to become
	// ready — skipping cleanly if it never does, like the other mhvtl tests — then
	// unload so the Load activity below still exercises a load from scratch.
	require.NoError(t, changer.Load(t.Context(), slotAddr, driveAddr), "pre-load for readiness probe")
	testutil.SkipIfDriveNotReady(t, stDev)
	require.NoError(t, changer.Unload(t.Context(), slotAddr, driveAddr), "unload after readiness probe")

	// --- Phase: Load ----------------------------------------------------------
	// Call the Load activity directly to exercise the reconciliation, blank
	// check, and SGDevice resolution without a running Temporal worker.
	loadActs := newLoadActivities()
	loadInput := LoadInput{
		Changer: testutil.ChangerDev(t),
		Tapes: []TapeAssignment{
			{Drive: stDev, BlankSlot: slotAddr, TapeIndex: 0, CopyIndex: 0},
		},
	}

	loadedTapes, err := loadActs.Load(t.Context(), loadInput)
	require.NoError(t, err, "Load activity")
	require.Len(t, loadedTapes, 1, "Load must return one LoadedTape per drive")

	lt := loadedTapes[0]
	assert.Equal(t, expectedBarcode, lt.Barcode, "barcode must match slot 1")
	assert.Equal(t, 0, lt.DriveIndex)
	assert.Equal(t, 0, lt.TapeIndex)
	assert.Equal(t, 0, lt.CopyIndex)
	assert.Equal(t, slotAddr, lt.SourceSlot)
	assert.Equal(t, stDev, lt.STDevice)
	assert.Equal(t, sgDev, lt.SGDevice)

	testutil.SkipIfDriveNotReady(t, stDev)

	// --- Phase: Write (FormatTape → WriteTree → FinalizeTape) ----------------
	stagingDir := t.TempDir()
	registry := newMountRegistry()
	writeActs := newWriteActivities(registry, stagingDir)

	// FormatTape (AC2: LTFS volume name = barcode).
	require.NoError(t, writeActs.FormatTape(t.Context(), FormatInput{
		Device:  lt.SGDevice,
		Barcode: lt.Barcode,
	}), "FormatTape")

	// Stage a small file so WriteTree has real content to copy.
	srcFile := filepath.Join(stagingDir, "archive.000")
	require.NoError(t, os.WriteFile(srcFile, []byte("tape-path integration test data"), 0o644))

	archives := []TapeWriteArchive{
		{
			SourceIndex: 0,
			Slices: []StagedSlice{
				{Path: srcFile, SHA256: "deadbeef", SizeBytes: 31},
			},
		},
	}

	// WriteTree (AC2: tree copied, manifest written last).
	require.NoError(t, writeActs.WriteTree(t.Context(), WriteTreeInput{
		Device:    lt.SGDevice,
		Barcode:   lt.Barcode,
		TapeIndex: lt.TapeIndex,
		CopyIndex: lt.CopyIndex,
		Archives:  archives,
	}), "WriteTree")

	// FinalizeTape (AC3: LTFS index captured).
	indexXML, err := writeActs.FinalizeTape(t.Context(), FinalizeInput{Device: lt.SGDevice})
	require.NoError(t, err, "FinalizeTape")
	assert.NotEmpty(t, indexXML, "captured LTFS index must not be empty")
	assert.Contains(t, string(indexXML), "<ltfsindex", "captured index must be LTFS XML")
	assert.Contains(t, string(indexXML), "<name>"+string(lt.Barcode)+"</name>",
		"index must name the volume by barcode")

	written := WrittenTape{
		Barcode:    lt.Barcode,
		DriveIndex: lt.DriveIndex,
		TapeIndex:  lt.TapeIndex,
		CopyIndex:  lt.CopyIndex,
		SourceSlot: lt.SourceSlot,
		IndexXML:   indexXML,
	}

	// --- Phase: Eject ---------------------------------------------------------
	ejectActs := newEjectActivities()
	require.NoError(t, ejectActs.Eject(t.Context(), EjectInput{
		Changer:      testutil.ChangerDev(t),
		WrittenTapes: []WrittenTape{written},
	}), "Eject")

	// Verify the tape landed in an I/O slot (AC3: transferred for physical removal).
	finalInv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "final inventory")

	inIOSlot := false

	for _, io := range finalInv.IOSlots {
		if io.Full && io.Barcode == lt.Barcode {
			inIOSlot = true
			break
		}
	}

	assert.True(t, inIOSlot, "tape %s must be in an I/O slot after Eject", lt.Barcode)
}

// TestTapePathMultipleDriveSets exercises the drive-set loop against mhvtl: a plan
// with more physical tapes than the library has drives is written as a sequence of
// drive-sets, each loaded, written, and ejected before the next begins. With a
// single configured drive, a 2-logical-tape × 2-copy plan needs four drive-sets —
// covering both extra logical tapes and extra copies (issue #66 AC6). It mirrors
// runTapePath by driving planDriveSets and the Load/Write/Eject activities
// directly, so the whole path runs without a Temporal worker. Skips when mhvtl or
// LTFS is absent.
func TestTapePathMultipleDriveSets(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)
	testutil.SkipIfLTFSUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "initial inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "at least 1 drive required")
	require.NotEmpty(t, inv.IOSlots, "at least 1 I/O slot required")
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")

	stDev := testutil.Drive0Dev(t)
	sgDev := testutil.Drive0SgDev(t)
	driveAddr := inv.Drives[0].Address

	// Use storage slots 4–7 so this test does not collide with the session (slot 0),
	// single-tape tape-path (slot 1), and whole-run (slot 2) integration tests
	// sharing the mhvtl library.
	slotIndexes := []int{4, 5, 6, 7}
	require.Greater(t, len(inv.Slots), slotIndexes[len(slotIndexes)-1], "at least 8 storage slots required")

	type physicalTape struct {
		slotAddr int
		barcode  tape.Barcode
	}

	var chosen []physicalTape

	for _, index := range slotIndexes {
		require.Truef(t, inv.Slots[index].Full, "slot %d must hold a tape", index)
		require.NotEmptyf(t, inv.Slots[index].Barcode, "slot %d tape must have a barcode", index)
		chosen = append(chosen, physicalTape{slotAddr: inv.Slots[index].Address, barcode: inv.Slots[index].Barcode})
	}

	// Restore the library on exit: every tape blanked and parked back in its slot,
	// drive 0 empty — so a repeat run and the sibling tests find the expected state.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 240*time.Second)
		defer cancel()

		for _, pt := range chosen {
			returnAndBlank(cleanupCtx, changer, stDev, sgDev, driveAddr, pt.slotAddr, pt.barcode)
		}
	})

	// Probe drive readiness up front using the first tape, skipping cleanly if mhvtl
	// left the drive stuck "not ready", then unload so the run starts from scratch.
	require.NoError(t, changer.Load(t.Context(), chosen[0].slotAddr, driveAddr), "pre-load for readiness probe")
	testutil.SkipIfDriveNotReady(t, stDev)
	require.NoError(t, changer.Unload(t.Context(), chosen[0].slotAddr, driveAddr), "unload after readiness probe")

	// A single configured drive with a 2-tape × 2-copy plan → four drive-sets of one
	// tape each.
	plan := TapePlan{
		Copies: 2,
		Tapes: []PlannedTape{
			{Archives: []PlannedArchive{{SourceIndex: 0}}},
			{Archives: []PlannedArchive{{SourceIndex: 1}}},
		},
	}

	blankSlots := make([]int, len(chosen))
	for i, pt := range chosen {
		blankSlots[i] = pt.slotAddr
	}

	sets, err := planDriveSets(plan, []string{stDev}, blankSlots)
	require.NoError(t, err, "planDriveSets")
	require.Len(t, sets, 4, "2 tapes × 2 copies on 1 drive must need four drive-sets")

	// Stage a small file so WriteTree has real content to copy.
	stagingDir := t.TempDir()
	srcFile := filepath.Join(stagingDir, "archive.000")
	require.NoError(t, os.WriteFile(srcFile, []byte("multi-set drive-set integration data"), 0o644))

	archives := []TapeWriteArchive{
		{
			SourceIndex: 0,
			Slices:      []StagedSlice{{Path: srcFile, SHA256: "deadbeef", SizeBytes: 36}},
		},
	}

	loadActs := newLoadActivities()
	registry := newMountRegistry()
	writeActs := newWriteActivities(registry, stagingDir)
	ejectActs := newEjectActivities()

	var writtenBarcodes []tape.Barcode

	for setIndex, set := range sets {
		require.Lenf(t, set, 1, "set %d must hold one tape (single drive)", setIndex)

		// --- Load the set --------------------------------------------------------
		loaded, err := loadActs.Load(t.Context(), LoadInput{
			Changer: testutil.ChangerDev(t),
			Tapes:   set,
		})
		require.NoErrorf(t, err, "set %d: Load", setIndex)
		require.Lenf(t, loaded, 1, "set %d: one LoadedTape", setIndex)

		lt := loaded[0]

		testutil.SkipIfDriveNotReady(t, stDev)

		// --- Write the tape (Format → WriteTree → Finalize) ----------------------
		require.NoErrorf(t, writeActs.FormatTape(t.Context(), FormatInput{
			Device:  lt.SGDevice,
			Barcode: lt.Barcode,
		}), "set %d: FormatTape", setIndex)

		require.NoErrorf(t, writeActs.WriteTree(t.Context(), WriteTreeInput{
			Device:    lt.SGDevice,
			Barcode:   lt.Barcode,
			TapeIndex: lt.TapeIndex,
			CopyIndex: lt.CopyIndex,
			Archives:  archives,
		}), "set %d: WriteTree", setIndex)

		indexXML, err := writeActs.FinalizeTape(t.Context(), FinalizeInput{Device: lt.SGDevice})
		require.NoErrorf(t, err, "set %d: FinalizeTape", setIndex)
		assert.Containsf(t, string(indexXML), "<ltfsindex", "set %d: captured index must be LTFS XML", setIndex)

		// --- Eject the set (frees the drive for the next set) --------------------
		require.NoErrorf(t, ejectActs.Eject(t.Context(), EjectInput{
			Changer: testutil.ChangerDev(t),
			WrittenTapes: []WrittenTape{{
				Barcode:    lt.Barcode,
				DriveIndex: lt.DriveIndex,
				TapeIndex:  lt.TapeIndex,
				CopyIndex:  lt.CopyIndex,
				SourceSlot: lt.SourceSlot,
				IndexXML:   indexXML,
			}},
		}), "set %d: Eject", setIndex)

		writtenBarcodes = append(writtenBarcodes, lt.Barcode)

		// The drive must be free again before the next set loads.
		postInv, err := changer.Inventory(t.Context())
		require.NoErrorf(t, err, "set %d: inventory after eject", setIndex)
		assert.Falsef(t, postInv.Drives[0].Loaded, "drive must be free after ejecting set %d", setIndex)
	}

	require.Len(t, writtenBarcodes, 4, "all four physical tapes must be written")

	// Every written tape ended up in an I/O slot for physical removal.
	finalInv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "final inventory")

	ioBarcodes := make(map[tape.Barcode]bool)

	for _, io := range finalInv.IOSlots {
		if io.Full {
			ioBarcodes[io.Barcode] = true
		}
	}

	for _, barcode := range writtenBarcodes {
		assert.Truef(t, ioBarcodes[barcode], "tape %s must be in an I/O slot after its set's Eject", barcode)
	}
}

// returnAndBlank restores one written tape to blank in its home storage slot,
// wherever it currently sits (an I/O slot after Eject, the drive after an
// interrupted run, or already home). It is best-effort: failures are ignored so
// cleanup never fails a passing test. A short SCSI ERASE resets mhvtl's in-memory
// state to blank without a long physical erase.
func returnAndBlank(ctx context.Context, changer *tape.Changer, stDev, sgDev string, driveAddr, slotAddr int, barcode tape.Barcode) {
	inv, err := changer.Inventory(ctx)
	if err != nil {
		return
	}

	// Get the tape into its home storage slot first (unless it is in the drive).
	loadedInDrive := len(inv.Drives) > 0 && inv.Drives[0].Loaded && inv.Drives[0].Barcode == barcode

	if !loadedInDrive {
		for _, io := range inv.IOSlots {
			if io.Full && io.Barcode == barcode {
				_ = changer.Transfer(ctx, io.Address, slotAddr)

				break
			}
		}

		if err := changer.Load(ctx, slotAddr, driveAddr); err != nil {
			return
		}
	}

	// Rewind (bounded) then short ERASE to reset the tape to blank, and unload it
	// back to its storage slot.
	rewindCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = exec.CommandContext(rewindCtx, "mt", "-f", stDev, "rewind").Run()

	cancel()

	_ = exec.CommandContext(ctx, "sg_raw", sgDev, "0x19", "0x00", "0x00", "0x00", "0x00", "0x00").Run()
	_ = changer.Unload(ctx, slotAddr, driveAddr)
}
