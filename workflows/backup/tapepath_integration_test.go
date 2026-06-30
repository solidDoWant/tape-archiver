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
		// resetting the vtltape daemon's in-memory state to blank.
		_ = exec.CommandContext(cleanupCtx, "mt", "-f", stDev, "rewind").Run()
		_ = exec.CommandContext(cleanupCtx, "sg_raw", sgDev,
			"0x19", "0x00", "0x00", "0x00", "0x00", "0x00").Run()
		_ = changer.Unload(cleanupCtx, slotAddr, driveAddr)
	})

	// --- Phase: Load ----------------------------------------------------------
	// Call the Load activity directly to exercise the reconciliation, blank
	// check, and SGDevice resolution without a running Temporal worker.
	loadActs := newLoadActivities()
	plan := TapePlan{
		Copies: 1,
		Tapes:  []PlannedTape{{Archives: []PlannedArchive{{SourceIndex: 0, DataBytes: 0}}}},
	}
	loadInput := LoadInput{
		Changer:    testutil.ChangerDev(t),
		Drives:     []string{stDev},
		BlankSlots: []int{slotAddr},
		Plan:       plan,
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
