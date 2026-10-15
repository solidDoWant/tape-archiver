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

// TestTapePath calls the Load → Write → Eject activities directly against mhvtl
// (issue #54 AC4).
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
	// Slot index 1: TestSessionSplitWriteWithFinalizeRetry formats slot 0's tape.
	require.GreaterOrEqual(t, len(inv.Slots), 2, "at least 2 storage slots required")
	require.True(t, inv.Slots[1].Full, "slot 1 must have a tape")

	stDev := testutil.Drive0Dev(t)
	sgDev := testutil.Drive0SgDev(t)
	slotAddr := inv.Slots[1].Address
	driveAddr := inv.Drives[0].Address
	expectedBarcode := inv.Slots[1].Barcode
	require.NotEmpty(t, expectedBarcode, "slot 1 tape must have a barcode")

	// Leave the tape blank in its slot. vtltape caches tape content in memory, so
	// after mkltfs it reports non-blank even if mktape recreates the files; a short
	// SCSI ERASE (LONG=0, via sg_raw) resets that state.
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
			for _, io := range cleanupInv.IOSlots {
				if io.Full && io.Barcode == expectedBarcode {
					_ = changer.Transfer(cleanupCtx, io.Address, slotAddr)
					break
				}
			}

			if err := changer.Load(cleanupCtx, slotAddr, driveAddr); err != nil {
				return
			}
		}

		// Bound the rewind: on a drive stuck "not ready" it blocks until the medium is
		// ready, hanging cleanup for the full budget.
		rewindCtx, rewindCancel := context.WithTimeout(cleanupCtx, 10*time.Second)
		_ = exec.CommandContext(rewindCtx, "mt", "-f", stDev, "rewind").Run()

		rewindCancel()

		_ = exec.CommandContext(cleanupCtx, "sg_raw", sgDev,
			"0x19", "0x00", "0x00", "0x00", "0x00", "0x00").Run()
		_ = changer.Unload(cleanupCtx, slotAddr, driveAddr)
	})

	// mhvtl can leave a freshly loaded drive stuck "not ready", which would fail the
	// blank check rather than skip; probe readiness first, then unload.
	require.NoError(t, changer.Load(t.Context(), slotAddr, driveAddr), "pre-load for readiness probe")
	testutil.SkipIfDriveNotReady(t, stDev)
	require.NoError(t, changer.Unload(t.Context(), slotAddr, driveAddr), "unload after readiness probe")

	// --- Phase: Load ----------------------------------------------------------
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

	// AC3: the index is staged to disk, not returned (issue #221).
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

	// AC3: transferred to an I/O slot for physical removal.
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

// TestTapePathAllowNonBlankTapes: a non-blank tape is refused by default and
// written with an "overwrote non-blank" flag under the override (issue #91).
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

	// A slot unused by the other tape-path tests, so the non-blank tape never
	// interferes with them.
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

	// Leave the tape blank in its slot (see TestTapePath cleanup).
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

	// The tape stays loaded so the format below makes it non-blank in place; the
	// reconciling Load calls then only re-run the blank check.
	require.NoError(t, changer.Load(t.Context(), slotAddr, driveAddr), "pre-load")
	testutil.SkipIfDriveNotReady(t, stDev)

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

	_, err = loadActs.Load(t.Context(), loadInput(false))
	require.Error(t, err, "Load must refuse a non-blank tape by default")
	assert.Contains(t, err.Error(), "not blank", "refusal must name the non-blank cause")

	afterRefusal, err := changer.Inventory(t.Context())
	require.NoError(t, err)
	assert.False(t, afterRefusal.Drives[0].Loaded, "the refused tape must not be left in the drive")
	assert.True(t, afterRefusal.Slots[slotIdx].Full, "the refused tape must be back in its slot")
	assert.Equal(t, barcode, afterRefusal.Slots[slotIdx].Barcode)

	loaded, err := loadActs.Load(t.Context(), loadInput(true))
	require.NoError(t, err, "Load must proceed on a non-blank tape when AllowNonBlankTapes is set")
	require.Len(t, loaded, 1, "Load must return one LoadedTape")
	assert.Equal(t, barcode, loaded[0].Barcode, "loaded barcode must match the slot tape")
	assert.True(t, loaded[0].OverwroteNonBlank,
		"a non-blank tape written under the override must be flagged as overwritten")
}

// TestTapePathMultipleDriveSets mirrors runTapePath against mhvtl: one drive and a
// 2-tape × 2-copy plan give four sequential drive-sets (issue #66 AC6).
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

	// Slots 4-7 avoid the session (0), tape-path (1), and whole-run (2) tests.
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

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 240*time.Second)
		defer cancel()

		for _, pt := range chosen {
			returnAndBlank(cleanupCtx, changer, stDev, sgDev, driveAddr, pt.slotAddr, pt.barcode)
		}
	})

	require.NoError(t, changer.Load(t.Context(), chosen[0].slotAddr, driveAddr), "pre-load for readiness probe")
	testutil.SkipIfDriveNotReady(t, stDev)
	require.NoError(t, changer.Unload(t.Context(), chosen[0].slotAddr, driveAddr), "unload after readiness probe")

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

		// --- Eject the set --------------------------------------------------------
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

		postInv, err := changer.Inventory(t.Context())
		require.NoErrorf(t, err, "set %d: inventory after eject", setIndex)
		assert.Falsef(t, postInv.Drives[0].Loaded, "drive must be free after ejecting set %d", setIndex)

		ioAddr := -1

		for _, io := range postInv.IOSlots {
			if io.Full && io.Barcode == lt.Barcode {
				ioAddr = io.Address

				break
			}
		}

		require.NotEqualf(t, -1, ioAddr, "set %d: tape %s must be in an I/O slot after Eject", setIndex, lt.Barcode)

		// Four tapes but three I/O slots: simulate the operator removing exported
		// tapes between sets (issue #67) by moving each back to its home slot.
		require.NoErrorf(t, changer.Transfer(t.Context(), ioAddr, lt.SourceSlot),
			"set %d: clear the I/O slot for the next set", setIndex)
	}

	require.Len(t, writtenBarcodes, 4, "all four physical tapes must be written")
}

// TestLoadPairsDriveByIdentity runs Load with a non-prefix set whose drive is not
// changer element 0, proving the blank goes into the same physical drive that is
// blank-checked and recorded (issue #137 AC2-AC4). Positional pairing would load
// DTE 0 while checking a different drive. Skips without per-drive DVCID serials.
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

	// Map config nodes to changer elements by unit serial; their orders may
	// disagree, which is what this exercises.
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

	// Pick the drive that is not element 0 (on the dev host, /dev/nst0 is DTE 1).
	chosen := targets[0]
	for _, tg := range targets {
		if tg.dteAddress > chosen.dteAddress {
			chosen = tg
		}
	}

	require.NotEqualf(t, 0, chosen.dteAddress,
		"need a drive whose changer element ≠ 0 to exercise a position/identity mismatch; got %+v", targets)

	// A slot unused by the other tape-path tests.
	const slotIdx = 3
	require.Greater(t, len(inv.Slots), slotIdx, "need at least 4 storage slots")
	require.Truef(t, inv.Slots[slotIdx].Full, "slot %d must hold a tape", slotIdx)

	slotAddr := inv.Slots[slotIdx].Address
	barcode := inv.Slots[slotIdx].Barcode
	require.NotEmpty(t, barcode, "chosen slot tape must have a barcode")

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 120*time.Second)
		defer cancel()

		returnAndBlankFromElement(cleanupCtx, changer, chosen.device, chosen.serial, slotAddr, barcode)
	})

	if de, ok := driveElementBySerial(inv, chosen.serial); ok && de.Loaded {
		require.NoError(t, changer.Unload(t.Context(), slotAddr, de.Address), "clear the chosen drive before the test")
	}

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

	// AC4: the drive now holding the tape is the chosen device's element, never DTE 0.
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

// returnAndBlankFromElement blanks a tape and returns it home, addressing the
// drive by unit serial. Best-effort.
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

// returnAndBlank blanks a written tape (short SCSI ERASE) and returns it to its
// home slot from wherever it sits. Best-effort.
func returnAndBlank(ctx context.Context, changer *tape.Changer, stDev, sgDev string, driveAddr, slotAddr int, barcode tape.Barcode) {
	inv, err := changer.Inventory(ctx)
	if err != nil {
		return
	}

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

	rewindCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = exec.CommandContext(rewindCtx, "mt", "-f", stDev, "rewind").Run()

	cancel()

	_ = exec.CommandContext(ctx, "sg_raw", sgDev, "0x19", "0x00", "0x00", "0x00", "0x00", "0x00").Run()
	_ = changer.Unload(ctx, slotAddr, driveAddr)
}
