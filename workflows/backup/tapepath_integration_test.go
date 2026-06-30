//go:build integration

package backup

import (
	"context"
	"os"
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
	require.True(t, inv.Slots[0].Full, "slot 0 must have a blank tape")

	stDev := testutil.Drive0Dev(t)
	sgDev := testutil.Drive0SgDev(t)
	slotAddr := inv.Slots[0].Address
	driveAddr := inv.Drives[0].Address
	expectedBarcode := inv.Slots[0].Barcode
	require.NotEmpty(t, expectedBarcode, "slot 0 tape must have a barcode")

	// Ensure the tape is unloaded and back in its source slot when the test ends,
	// regardless of which step failed. If the tape is still in the drive (e.g.
	// FinalizeTape left it mounted), try to unmount first, then unload.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 90*time.Second)
		defer cancel()

		cleanupInv, invErr := changer.Inventory(cleanupCtx)
		if invErr != nil {
			return
		}

		if len(cleanupInv.Drives) > 0 && cleanupInv.Drives[0].Loaded {
			_ = changer.Unload(cleanupCtx, slotAddr, driveAddr)
		}
	})

	// --- Phase: Load ----------------------------------------------------------
	// Call the Load activity directly to exercise the reconciliation, blank
	// check, and SGDevice resolution without a running Temporal worker.
	testutil.SkipIfDriveNotReady(t, stDev) // ensure drive ready before blank check

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
	assert.Equal(t, expectedBarcode, lt.Barcode, "barcode must match slot 0")
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
