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
			Label:       "integration",
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

	// FinalizeTape stages the LTFS index to disk and returns its path (AC3: LTFS
	// index captured; staged rather than returned per issue #221).
	indexPath, err := writeActs.FinalizeTape(t.Context(), FinalizeInput{Device: lt.SGDevice, Barcode: lt.Barcode})
	require.NoError(t, err, "FinalizeTape")
	require.NotEmpty(t, indexPath, "FinalizeTape must return the staged index path")
	indexXML, err := os.ReadFile(indexPath)
	require.NoError(t, err, "read staged LTFS index")
	assert.NotEmpty(t, indexXML, "captured LTFS index must not be empty")
	assert.Contains(t, string(indexXML), "<ltfsindex", "captured index must be LTFS XML")
	assert.Contains(t, string(indexXML), "<name>"+string(lt.Barcode)+"</name>",
		"index must name the volume by barcode")

	written := WrittenTape{
		Barcode:      lt.Barcode,
		DriveIndex:   lt.DriveIndex,
		TapeIndex:    lt.TapeIndex,
		CopyIndex:    lt.CopyIndex,
		SourceSlot:   lt.SourceSlot,
		IndexXMLPath: indexPath,
	}

	// --- Phase: Eject ---------------------------------------------------------
	ejectActs := newEjectActivities()
	ejectResult, err := ejectActs.Eject(t.Context(), EjectInput{
		Changer:      testutil.ChangerDev(t),
		WrittenTapes: []WrittenTape{written},
	})
	require.NoError(t, err, "Eject")
	require.Empty(t, ejectResult.Remaining, "the single tape must be exported, not left remaining")

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

// TestTapePathAllowNonBlankTapes exercises the Library.AllowNonBlankTapes override
// (issue #91) against mhvtl: a pre-written (non-blank) tape makes the Load activity
// refuse by default, and proceed with an "overwrote non-blank" flag when the
// override is set. Blank detection is unchanged either way. Skips when mhvtl or LTFS
// is absent.
func TestTapePathAllowNonBlankTapes(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)
	testutil.SkipIfLTFSUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "initial inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "at least 1 drive required")
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")

	stDev := testutil.Drive0Dev(t)
	sgDev := testutil.Drive0SgDev(t)
	driveAddr := inv.Drives[0].Address

	// Use a full storage slot distinct from those the other tape-path tests use
	// (slot 0: session test, slot 1: TestTapePath) so a non-blank tape left here
	// never interferes with them. Integration tests run sequentially, but this
	// keeps the fixtures independent regardless of order.
	slotIdx := -1

	for i := 2; i < len(inv.Slots); i++ {
		if inv.Slots[i].Full {
			slotIdx = i
			break
		}
	}

	require.GreaterOrEqual(t, slotIdx, 2, "need a full storage slot at index >= 2")

	slotAddr := inv.Slots[slotIdx].Address
	barcode := inv.Slots[slotIdx].Barcode
	require.NotEmpty(t, barcode, "chosen slot tape must have a barcode")

	// Leave the library clean: the tape back in its slot AND blank so consecutive
	// runs start from the same state. A short SCSI ERASE resets the vtltape daemon's
	// cached in-memory content to blank (see TestTapePath cleanup for the rationale).
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 120*time.Second)
		defer cancel()

		cleanupInv, invErr := changer.Inventory(cleanupCtx)
		if invErr != nil {
			return
		}

		tapeInDrive := len(cleanupInv.Drives) > 0 &&
			cleanupInv.Drives[0].Loaded &&
			cleanupInv.Drives[0].Barcode == barcode

		if !tapeInDrive {
			if err := changer.Load(cleanupCtx, slotAddr, driveAddr); err != nil {
				return
			}
		}

		rewindCtx, rewindCancel := context.WithTimeout(cleanupCtx, 10*time.Second)
		_ = exec.CommandContext(rewindCtx, "mt", "-f", stDev, "rewind").Run()

		rewindCancel()

		_ = exec.CommandContext(cleanupCtx, "sg_raw", sgDev,
			"0x19", "0x00", "0x00", "0x00", "0x00", "0x00").Run()
		_ = changer.Unload(cleanupCtx, slotAddr, driveAddr)
	})

	// Load the tape and wait for the drive to become ready, skipping cleanly if it
	// never does (like the other mhvtl tests). The tape stays in the drive so the
	// format below can make it non-blank in place; the reconciling Load calls then
	// see it already loaded and re-run only the blank check.
	require.NoError(t, changer.Load(t.Context(), slotAddr, driveAddr), "pre-load")
	testutil.SkipIfDriveNotReady(t, stDev)

	// Make the tape NON-BLANK by writing an LTFS volume to it. mkltfs leaves the
	// vtltape daemon reporting the tape as non-blank, which is exactly the state the
	// Load blank check must detect.
	writeActs := newWriteActivities(newMountRegistry(), t.TempDir())
	require.NoError(t, writeActs.FormatTape(t.Context(), FormatInput{
		Device:  sgDev,
		Barcode: barcode,
	}), "FormatTape to make the tape non-blank")

	loadActs := newLoadActivities()
	loadInput := func(allow bool) LoadInput {
		return LoadInput{
			Changer:            testutil.ChangerDev(t),
			Tapes:              []TapeAssignment{{Drive: stDev, BlankSlot: slotAddr, TapeIndex: 0, CopyIndex: 0}},
			AllowNonBlankTapes: allow,
		}
	}

	// Default: the Load activity refuses the non-blank tape before any write.
	_, err = loadActs.Load(t.Context(), loadInput(false))
	require.Error(t, err, "Load must refuse a non-blank tape by default")
	assert.Contains(t, err.Error(), "not blank", "refusal must name the non-blank cause")

	// Override: the Load activity warns and proceeds, flagging the overwrite. The
	// tape is still in the drive, so this call reconciles to a no-op load and simply
	// re-runs the (unchanged) blank check on the same non-blank tape.
	loaded, err := loadActs.Load(t.Context(), loadInput(true))
	require.NoError(t, err, "Load must proceed on a non-blank tape when AllowNonBlankTapes is set")
	require.Len(t, loaded, 1, "Load must return one LoadedTape")
	assert.Equal(t, barcode, loaded[0].Barcode, "loaded barcode must match the slot tape")
	assert.True(t, loaded[0].OverwroteNonBlank,
		"a non-blank tape written under the override must be flagged as overwritten")
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
			Label:       "integration",
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

		indexPath, err := writeActs.FinalizeTape(t.Context(), FinalizeInput{Device: lt.SGDevice, Barcode: lt.Barcode})
		require.NoErrorf(t, err, "set %d: FinalizeTape", setIndex)
		indexXML, err := os.ReadFile(indexPath)
		require.NoErrorf(t, err, "set %d: read staged LTFS index", setIndex)
		assert.Containsf(t, string(indexXML), "<ltfsindex", "set %d: captured index must be LTFS XML", setIndex)

		// --- Eject the set (frees the drive for the next set) --------------------
		// Eject needs only identity/slot fields, not the index (issue #221).
		ejectResult, err := ejectActs.Eject(t.Context(), EjectInput{
			Changer: testutil.ChangerDev(t),
			WrittenTapes: []WrittenTape{{
				Barcode:    lt.Barcode,
				DriveIndex: lt.DriveIndex,
				TapeIndex:  lt.TapeIndex,
				CopyIndex:  lt.CopyIndex,
				SourceSlot: lt.SourceSlot,
			}},
		})
		require.NoErrorf(t, err, "set %d: Eject", setIndex)
		require.Emptyf(t, ejectResult.Remaining, "set %d: the set's tape must be exported", setIndex)

		writtenBarcodes = append(writtenBarcodes, lt.Barcode)

		// The drive must be free again before the next set loads.
		postInv, err := changer.Inventory(t.Context())
		require.NoErrorf(t, err, "set %d: inventory after eject", setIndex)
		assert.Falsef(t, postInv.Drives[0].Loaded, "drive must be free after ejecting set %d", setIndex)

		// The tape must have reached an I/O slot for physical removal.
		ioAddr := -1

		for _, io := range postInv.IOSlots {
			if io.Full && io.Barcode == lt.Barcode {
				ioAddr = io.Address

				break
			}
		}

		require.NotEqualf(t, -1, ioAddr, "set %d: tape %s must be in an I/O slot after Eject", setIndex, lt.Barcode)

		// This run writes four physical tapes but the library has only three I/O
		// slots, so the operator must remove exported tapes between sets to free
		// slots — the operator-in-the-loop flow the Eject phase relies on (issue
		// #67). Simulate that here by moving the exported tape from its I/O slot
		// back to its home storage slot before the next set runs.
		require.NoErrorf(t, changer.Transfer(t.Context(), ioAddr, lt.SourceSlot),
			"set %d: clear the I/O slot for the next set", setIndex)
	}

	require.Len(t, writtenBarcodes, 4, "all four physical tapes must be written")
}

// TestLoadPairsDriveByIdentity drives the real Load activity against mhvtl with a
// retry-shaped, non-prefix drive-set — a single assignment for a configured drive
// whose changer data-transfer element is NOT element 0 — and proves the blank is
// loaded into the same physical drive Load blank-checks and records (issue #137).
//
// It covers:
//   - AC3: the configured drive device order disagrees with the changer's element
//     order (this repo's dev host: /dev/nst0 is the changer's DTE 1), yet the tape
//     is loaded and blank-checked on the drive it was assigned to.
//   - AC4: the set is non-prefix (position 0 holds config drive index 1's node, or
//     a node whose DTE address ≠ 0); the test fails if the loaded-into drive (the
//     element the changer moved the blank into) and the written-to drive (the
//     assignment's device node) diverge. Positional pairing would move the blank
//     into DTE 0 while blank-checking a different drive.
//   - AC2: the returned LoadedTape.DriveIndex is the assignment's config drive
//     index, not its position in the set.
//
// Skips when mhvtl is absent or does not report per-drive DVCID serials.
func TestLoadPairsDriveByIdentity(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "initial inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 2, "identity pairing needs at least 2 drives")

	for i, de := range inv.Drives {
		if de.Serial == "" {
			t.Skipf("mhvtl drive %d reports no DVCID serial; identity pairing test needs DVCID", i)
		}
	}

	// Map each configured drive device node to the changer element that IS that
	// physical unit, by unit serial. The two config nodes and the changer's element
	// order may disagree (probe-order mismatch) — that is exactly what we exercise.
	configNodes := []string{testutil.Drive0Dev(t), testutil.Drive1Dev(t)}

	type target struct {
		configIndex int
		device      string
		serial      string
		dteAddress  int
	}

	var targets []target

	for configIndex, node := range configNodes {
		info, inqErr := tape.NewDrive(node).Inquire(t.Context())
		require.NoErrorf(t, inqErr, "INQUIRY on %s", node)
		require.NotEmptyf(t, info.Serial, "drive %s must report a unit serial", node)

		de, matchErr := matchDriveElement(inv, info.Serial)
		require.NoErrorf(t, matchErr, "pair %s by serial %q", node, info.Serial)

		targets = append(targets, target{configIndex: configIndex, device: node, serial: info.Serial, dteAddress: de.Address})
	}

	// Pick the configured drive whose changer element is NOT element 0, so a
	// single-assignment set (position 0) would positionally pair to DTE 0 — a
	// different physical drive. Identity pairing must instead pick this drive's own
	// element. On the dev host that is /dev/nst0 (DTE 1).
	chosen := targets[0]
	for _, tg := range targets {
		if tg.dteAddress > chosen.dteAddress {
			chosen = tg
		}
	}

	require.NotEqualf(t, 0, chosen.dteAddress,
		"need a drive whose changer element ≠ 0 to exercise a position/identity mismatch; got %+v", targets)

	// Use a storage slot distinct from the other tape-path tests (0 session, 1
	// single-tape, 2 whole-run, 4–7 multi-set) so leftover state never collides.
	const slotIdx = 3
	require.Greater(t, len(inv.Slots), slotIdx, "need at least 4 storage slots")
	require.Truef(t, inv.Slots[slotIdx].Full, "slot %d must hold a tape", slotIdx)

	slotAddr := inv.Slots[slotIdx].Address
	barcode := inv.Slots[slotIdx].Barcode
	require.NotEmpty(t, barcode, "chosen slot tape must have a barcode")

	// Restore the library: tape blanked and parked back in its slot, drive empty.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 120*time.Second)
		defer cancel()

		returnAndBlankFromElement(cleanupCtx, changer, chosen.device, chosen.serial, slotAddr, barcode)
	})

	// Start clean: the drive holding the chosen element must be empty, and the tape
	// must be in its slot. If a prior run left the tape in the drive, unload it.
	if de, ok := driveElementBySerial(inv, chosen.serial); ok && de.Loaded {
		require.NoError(t, changer.Unload(t.Context(), slotAddr, de.Address), "clear the chosen drive before the test")
	}

	// Readiness probe on the chosen drive (skips cleanly on a wedged mhvtl drive),
	// then unload so Load exercises a fresh load.
	chosenDTE, ok := driveElementBySerial(inv, chosen.serial)
	require.True(t, ok, "chosen element present in inventory")
	require.NoError(t, changer.Load(t.Context(), slotAddr, chosenDTE.Address), "pre-load for readiness probe")
	testutil.SkipIfDriveNotReady(t, chosen.device)
	require.NoError(t, changer.Unload(t.Context(), slotAddr, chosenDTE.Address), "unload after readiness probe")

	// --- Real Load with a retry-shaped, non-prefix set -----------------------
	loadActs := newLoadActivities()

	loaded, err := loadActs.Load(t.Context(), LoadInput{
		Changer:            testutil.ChangerDev(t),
		AllowNonBlankTapes: true, // robust to a leftover non-blank tape; pairing is what we assert
		Tapes: []TapeAssignment{
			{Drive: chosen.device, DriveIndex: chosen.configIndex, BlankSlot: slotAddr, TapeIndex: 0, CopyIndex: 0},
		},
	})
	require.NoError(t, err, "Load must pair the device node to its element by identity, not position")
	require.Len(t, loaded, 1)

	lt := loaded[0]
	assert.Equal(t, barcode, lt.Barcode, "loaded tape is the one from the target slot")
	assert.Equal(t, chosen.configIndex, lt.DriveIndex, "recorded DriveIndex is the config index (AC2)")
	assert.Equal(t, chosen.device, lt.STDevice, "written-to device is the assignment's node")

	// The loaded-into drive (the element now holding the tape) must be the SAME
	// physical drive as the written-to device (AC4): its serial equals the chosen
	// device's serial, and its address is the chosen element — never DTE 0.
	postInv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "post-load inventory")

	loadedInto := -1
	loadedIntoSerial := ""

	for _, de := range postInv.Drives {
		if de.Loaded && de.Barcode == barcode {
			loadedInto = de.Address
			loadedIntoSerial = de.Serial

			break
		}
	}

	require.NotEqualf(t, -1, loadedInto, "tape %s must be loaded into a drive after Load", barcode)
	assert.Equal(t, chosen.serial, loadedIntoSerial,
		"loaded-into drive and written-to drive must not diverge (AC4)")
	assert.Equal(t, chosen.dteAddress, loadedInto,
		"the blank must land in the assigned drive's element, not the set-position element")
}

// driveElementBySerial finds the data-transfer element with the given unit serial.
func driveElementBySerial(inv tape.Inventory, serial string) (tape.DriveElement, bool) {
	for _, de := range inv.Drives {
		if de.Serial == serial {
			return de, true
		}
	}

	return tape.DriveElement{}, false
}

// returnAndBlankFromElement restores a tape to blank in its home slot, addressing
// the drive by unit serial (identity) rather than by index. Best-effort cleanup.
func returnAndBlankFromElement(ctx context.Context, changer *tape.Changer, stDev, serial string, slotAddr int, barcode tape.Barcode) {
	inv, err := changer.Inventory(ctx)
	if err != nil {
		return
	}

	de, ok := driveElementBySerial(inv, serial)
	if !ok {
		return
	}

	loadedInDrive := de.Loaded && de.Barcode == barcode
	if !loadedInDrive {
		for _, io := range inv.IOSlots {
			if io.Full && io.Barcode == barcode {
				_ = changer.Transfer(ctx, io.Address, slotAddr)

				break
			}
		}

		if err := changer.Load(ctx, slotAddr, de.Address); err != nil {
			return
		}
	}

	sgDev, err := tape.NewDrive(stDev).SGDevice()
	if err != nil {
		return
	}

	rewindCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = exec.CommandContext(rewindCtx, "mt", "-f", stDev, "rewind").Run()

	cancel()

	_ = exec.CommandContext(ctx, "sg_raw", sgDev, "0x19", "0x00", "0x00", "0x00", "0x00", "0x00").Run()
	_ = changer.Unload(ctx, slotAddr, de.Address)
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
