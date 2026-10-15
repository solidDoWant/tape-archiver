//go:build integration

package tape_test

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

// TestInventory checks the mhvtl topology: 2 drives, 47 slots, 3 I/O slots.
func TestInventory(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err)

	assert.Len(t, inv.Drives, 2, "expected 2 drives")
	assert.GreaterOrEqual(t, len(inv.Slots), 47, "expected at least 47 storage slots")
	assert.GreaterOrEqual(t, len(inv.IOSlots), 3, "expected at least 3 I/O slots")

	for _, drive := range inv.Drives {
		assert.False(t, drive.Loaded, "drive %d should be empty on startup", drive.Address)
	}

	for _, slot := range inv.Slots {
		assert.True(t, slot.Full, "storage slot %d should be full", slot.Address)
		assert.NotEmpty(t, slot.Barcode, "storage slot %d should have a barcode", slot.Address)
	}
}

// TestLoadConfirm loads slot 1 into drive 0 and unloads it back.
func TestLoadConfirm(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "initial inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "no drives found")
	require.NotEmpty(t, inv.Slots, "no storage slots found")
	require.False(t, inv.Drives[0].Loaded, "drive 0 should start empty")
	require.True(t, inv.Slots[0].Full, "slot 1 should have a tape")

	slotAddr := inv.Slots[0].Address
	driveAddr := inv.Drives[0].Address
	barcode := inv.Slots[0].Barcode

	err = changer.Load(t.Context(), slotAddr, driveAddr)
	require.NoError(t, err, "load")

	t.Cleanup(func() {
		_ = changer.Unload(t.Context(), slotAddr, driveAddr)
	})

	inv, err = changer.Inventory(t.Context())
	require.NoError(t, err, "inventory after load")

	require.GreaterOrEqual(t, len(inv.Drives), 1)
	assert.True(t, inv.Drives[0].Loaded, "drive 0 should be loaded after Load()")
	assert.Equal(t, barcode, inv.Drives[0].Barcode, "loaded barcode should match")

	err = changer.Unload(t.Context(), slotAddr, driveAddr)
	require.NoError(t, err, "unload")

	inv, err = changer.Inventory(t.Context())
	require.NoError(t, err, "inventory after unload")

	require.GreaterOrEqual(t, len(inv.Drives), 1)
	assert.False(t, inv.Drives[0].Loaded, "drive should be empty after Unload()")
	assert.True(t, inv.Slots[0].Full, "slot should be full after Unload()")
}

// TestResolveViaSymlink reproduces #326: a changer or drive reached through a
// symlink whose basename is not the kernel device name must still resolve.
func TestResolveViaSymlink(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	linkDir := t.TempDir()

	changerDev := testutil.ChangerDev(t)
	changerLink := filepath.Join(linkDir, "changer")
	require.NoError(t, os.Symlink(changerDev, changerLink))

	inv, err := tape.NewChanger(changerLink).Inventory(t.Context())
	require.NoError(t, err, "changer inventory via symlink %s -> %s", changerLink, changerDev)
	assert.Len(t, inv.Drives, 2, "expected 2 drives")

	driveDev := testutil.Drive0Dev(t)
	driveLink := filepath.Join(linkDir, "drive0")
	require.NoError(t, os.Symlink(driveDev, driveLink))

	wantSG, err := tape.NewDrive(driveDev).SGDevice()
	require.NoError(t, err, "resolve sg node from raw tape node %s", driveDev)

	gotSG, err := tape.NewDrive(driveLink).SGDevice()
	require.NoError(t, err, "resolve sg node via symlink %s -> %s", driveLink, driveDev)
	assert.Equal(t, wantSG, gotSG, "symlinked drive node must resolve the same sg node as the raw node")
}

// TestBlankCheck verifies a fresh mhvtl tape is reported blank.
func TestBlankCheck(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))
	drive := tape.NewDrive(testutil.Drive0Dev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1)
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")

	// Slot index 2 is never written by any test (0 and 1 are, and vtltape caches
	// mkltfs content for the rest of the run), so it is guaranteed blank.
	const blankSlotIndex = 2
	require.GreaterOrEqual(t, len(inv.Slots), blankSlotIndex+1,
		"need at least %d storage slots", blankSlotIndex+1)
	require.True(t, inv.Slots[blankSlotIndex].Full, "slot %d must have a tape", blankSlotIndex)

	slot := inv.Slots[blankSlotIndex]
	driveAddr := inv.Drives[0].Address

	err = changer.Load(t.Context(), slot.Address, driveAddr)
	require.NoError(t, err, "load")

	// Not cancelled by t.Skip(), so cleanup still unloads the tape.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()

		_ = changer.Unload(ctx, slot.Address, driveAddr)
	})

	testutil.SkipIfDriveNotReady(t, testutil.Drive0Dev(t))

	blank, err := drive.IsBlank(t.Context())
	require.NoError(t, err, "IsBlank on blank tape")
	assert.True(t, blank, "freshly loaded mhvtl tape should be blank")
}

// TestInquire reads the identity of mhvtl's IBM ULT3580-TD6 drive and STK L700
// changer.
func TestInquire(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	drive := tape.NewDrive(testutil.Drive0Dev(t))
	changer := tape.NewChanger(testutil.ChangerDev(t))

	driveInfo, err := drive.Inquire(t.Context())
	require.NoError(t, err, "drive INQUIRY")
	assert.Equal(t, "IBM", driveInfo.Vendor)
	assert.Equal(t, "ULT3580-TD6", driveInfo.Product)
	assert.Equal(t, "IBM ULT3580-TD6", driveInfo.Model())
	assert.Equal(t, "LTO-6", driveInfo.LTOGeneration())
	assert.NotEmpty(t, driveInfo.Serial, "drive should report a unit serial number")

	changerInfo, err := changer.Inquire(t.Context())
	require.NoError(t, err, "changer INQUIRY")
	assert.Equal(t, "STK L700", changerInfo.Model())
	assert.Equal(t, "unknown", changerInfo.LTOGeneration())
}

// TestTransferToIO moves a tape from a storage slot into an I/O slot.
func TestTransferToIO(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.NotEmpty(t, inv.Slots, "no storage slots")
	require.NotEmpty(t, inv.IOSlots, "no I/O slots")
	require.True(t, inv.Slots[0].Full, "slot 1 must have a tape")
	require.False(t, inv.IOSlots[0].Full, "I/O slot must be empty")

	srcSlot := inv.Slots[0]
	ioSlot := inv.IOSlots[0]
	barcode := srcSlot.Barcode

	err = changer.Transfer(t.Context(), srcSlot.Address, ioSlot.Address)
	require.NoError(t, err, "transfer to I/O slot")

	t.Cleanup(func() {
		// Survives test cancellation so the tape always goes back.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()

		_ = changer.Transfer(ctx, ioSlot.Address, srcSlot.Address)
	})

	inv, err = changer.Inventory(t.Context())
	require.NoError(t, err, "inventory after transfer")

	assert.True(t, inv.IOSlots[0].Full, "I/O slot should be full after transfer")
	assert.Equal(t, barcode, inv.IOSlots[0].Barcode, "barcode in I/O slot should match")
	assert.False(t, inv.Slots[0].Full, "source slot should be empty after transfer")
}

// TestLogPages reads TapeAlert and the reposition counter; mhvtl's LTO-6 drive
// supports page 0x30, so the counter must be measured, not silently zero.
func TestLogPages(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	sgDev := testutil.Drive0SgDev(t)
	if _, err := os.Stat(sgDev); err != nil {
		t.Skipf("sg device %s not available: %v (set %s to override)", sgDev, err, testutil.EnvDrive0SgDev)
	}

	reader := tape.NewLogPageReader(sgDev)

	result, err := reader.ReadLogPages(t.Context())
	require.NoError(t, err, "ReadLogPages")

	assert.NotEmpty(t, result.TapeAlert.Flags, "should have parsed TapeAlert flags")
	assert.False(t, result.TapeAlert.AnySet(), "no TapeAlert flags should be set on a fresh virtual drive")

	assert.True(t, result.RepositionsMeasured, "the LTO-6 drive supports page 0x30, so repositions must be measured, not silently zero")
	assert.Zero(t, result.Repositions, "a fresh virtual drive should not have back-hitched")
}

// TestMediumGeneration reads the loaded tape's generation (#340); an empty drive
// must fail rather than name one.
func TestMediumGeneration(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	sgDev := testutil.Drive0SgDev(t)
	if _, err := os.Stat(sgDev); err != nil {
		t.Skipf("sg device %s not available: %v (set %s to override)", sgDev, err, testutil.EnvDrive0SgDev)
	}

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1)
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")

	_, err = tape.MediumGeneration(t.Context(), sgDev)
	require.Error(t, err, "an empty drive must not report a medium generation")

	// Slot index 3 is only used here and never written.
	const generationSlotIndex = 3
	require.GreaterOrEqual(t, len(inv.Slots), generationSlotIndex+1,
		"need at least %d storage slots", generationSlotIndex+1)

	slot := inv.Slots[generationSlotIndex]
	driveAddr := inv.Drives[0].Address

	require.NoError(t, changer.Load(t.Context(), slot.Address, driveAddr), "load")

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()

		_ = changer.Unload(ctx, slot.Address, driveAddr)
	})

	testutil.SkipIfDriveNotReady(t, testutil.Drive0Dev(t))

	generation, err := tape.MediumGeneration(t.Context(), sgDev)
	require.NoError(t, err, "MediumGeneration")
	assert.Equal(t, "LTO-6", generation, "the library's media are LTO-6 cartridges")
}
