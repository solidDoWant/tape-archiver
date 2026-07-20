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

// TestInventory verifies that Changer.Inventory() returns the expected topology
// from the mhvtl virtual library (2 drives, 47 storage slots, 3 I/O slots).
func TestInventory(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err)

	assert.Len(t, inv.Drives, 2, "expected 2 drives")
	assert.GreaterOrEqual(t, len(inv.Slots), 47, "expected at least 47 storage slots")
	assert.GreaterOrEqual(t, len(inv.IOSlots), 3, "expected at least 3 I/O slots")

	// Drives should initially be empty.
	for _, drive := range inv.Drives {
		assert.False(t, drive.Loaded, "drive %d should be empty on startup", drive.Address)
	}

	// All storage slots should have barcoded tapes in the default mhvtl config.
	for _, slot := range inv.Slots {
		assert.True(t, slot.Full, "storage slot %d should be full", slot.Address)
		assert.NotEmpty(t, slot.Barcode, "storage slot %d should have a barcode", slot.Address)
	}
}

// TestLoadConfirm verifies that loading a tape from slot 1 into drive 0
// results in the drive reporting the tape as loaded, and that unloading it
// returns it to the slot.
func TestLoadConfirm(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	// Confirm the initial state: slot 1 full, drive 0 empty.
	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "initial inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "no drives found")
	require.NotEmpty(t, inv.Slots, "no storage slots found")
	require.False(t, inv.Drives[0].Loaded, "drive 0 should start empty")
	require.True(t, inv.Slots[0].Full, "slot 1 should have a tape")

	slotAddr := inv.Slots[0].Address
	driveAddr := inv.Drives[0].Address
	barcode := inv.Slots[0].Barcode

	// Load tape from storage slot into drive 0.
	err = changer.Load(t.Context(), slotAddr, driveAddr)
	require.NoError(t, err, "load")

	t.Cleanup(func() {
		_ = changer.Unload(t.Context(), slotAddr, driveAddr)
	})

	// Confirm drive shows tape loaded with correct barcode.
	inv, err = changer.Inventory(t.Context())
	require.NoError(t, err, "inventory after load")

	require.GreaterOrEqual(t, len(inv.Drives), 1)
	assert.True(t, inv.Drives[0].Loaded, "drive 0 should be loaded after Load()")
	assert.Equal(t, barcode, inv.Drives[0].Barcode, "loaded barcode should match")

	// Unload back to the same slot.
	err = changer.Unload(t.Context(), slotAddr, driveAddr)
	require.NoError(t, err, "unload")

	// Confirm drive empty and slot restored.
	inv, err = changer.Inventory(t.Context())
	require.NoError(t, err, "inventory after unload")

	require.GreaterOrEqual(t, len(inv.Drives), 1)
	assert.False(t, inv.Drives[0].Loaded, "drive should be empty after Unload()")
	assert.True(t, inv.Slots[0].Full, "slot should be full after Unload()")
}

// TestResolveViaSymlink reproduces issue #326: a changer or drive configured at a
// stable udev symlink whose basename is not the kernel device name (as a dry run
// targets /dev/mhvtl/changer -> sch2, /dev/mhvtl/drive0 -> nst0) must still resolve
// its SCSI address. The resolver formerly built /sys/class/.../<basename>/device
// from the path basename, which does not exist for such a symlink; it now matches
// the node's device number, so a symlink resolves identically to the raw node.
//
// The symlinks live in t.TempDir() (not /dev), so this needs no root: os.Stat
// follows the symlink to the real node's device number while the path basename
// ("changer"/"drive0") deliberately differs from the kernel name.
func TestResolveViaSymlink(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	linkDir := t.TempDir()

	changerDev := testutil.ChangerDev(t)
	changerLink := filepath.Join(linkDir, "changer")
	require.NoError(t, os.Symlink(changerDev, changerLink))

	// The changer resolves its sg node from the symlink's SCSI address; a
	// successful Inventory proves resolution worked through the renamed path.
	inv, err := tape.NewChanger(changerLink).Inventory(t.Context())
	require.NoError(t, err, "changer inventory via symlink %s -> %s", changerLink, changerDev)
	assert.Len(t, inv.Drives, 2, "expected 2 drives")

	// The drive resolves its paired sg node the same way; assert the symlink
	// yields the identical sg node as the raw tape node.
	driveDev := testutil.Drive0Dev(t)
	driveLink := filepath.Join(linkDir, "drive0")
	require.NoError(t, os.Symlink(driveDev, driveLink))

	wantSG, err := tape.NewDrive(driveDev).SGDevice()
	require.NoError(t, err, "resolve sg node from raw tape node %s", driveDev)

	gotSG, err := tape.NewDrive(driveLink).SGDevice()
	require.NoError(t, err, "resolve sg node via symlink %s -> %s", driveLink, driveDev)
	assert.Equal(t, wantSG, gotSG, "symlinked drive node must resolve the same sg node as the raw node")
}

// TestBlankCheck verifies that a freshly loaded mhvtl tape is reported as blank
// by Drive.IsBlank().
func TestBlankCheck(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))
	// Pass only the tape node; the drive resolves its paired sg node from the
	// SCSI address, exercising the production path.
	drive := tape.NewDrive(testutil.Drive0Dev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1)
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")

	// Use a dedicated slot that no other integration test ever writes, so the
	// tape is guaranteed blank. Slot index 0 is formatted by pkg/ltfs and the
	// backup session test, and slot index 1 by the backup tape-path test; both
	// leave their tape non-blank. mkltfs content is sticky (the vtltape daemon
	// caches it), so a contaminated tape stays non-blank for the rest of the run
	// and this assertion would fail spuriously. Slot index 2 is touched only by
	// this test (load/unload, never written), so make_vtl_media's fresh blank
	// media survives.
	const blankSlotIndex = 2
	require.GreaterOrEqual(t, len(inv.Slots), blankSlotIndex+1,
		"need at least %d storage slots", blankSlotIndex+1)
	require.True(t, inv.Slots[blankSlotIndex].Full, "slot %d must have a tape", blankSlotIndex)

	slot := inv.Slots[blankSlotIndex]
	driveAddr := inv.Drives[0].Address

	err = changer.Load(t.Context(), slot.Address, driveAddr)
	require.NoError(t, err, "load")

	// Use a context that is not cancelled by t.Skip() so cleanup completes even
	// when SkipIfDriveNotReady skips the test before IsBlank() is called.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()

		_ = changer.Unload(ctx, slot.Address, driveAddr)
	})

	testutil.SkipIfDriveNotReady(t, testutil.Drive0Dev(t))

	// A freshly loaded mhvtl tape is blank.
	blank, err := drive.IsBlank(t.Context())
	require.NoError(t, err, "IsBlank on blank tape")
	assert.True(t, blank, "freshly loaded mhvtl tape should be blank")
}

// TestInquire verifies that INQUIRY reads the SCSI identity of both the mhvtl
// drive and the changer, and that the drive's LTO generation is derived from its
// product id. INQUIRY needs no loaded tape and moves no media. The mhvtl default
// config emulates an IBM ULT3580-TD6 (LTO-6) drive behind an STK L700 library.
func TestInquire(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	// Pass only the tape/changer nodes; each resolves its paired sg node from the
	// SCSI address, exercising the production path.
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
	// A changer has no LTO generation.
	assert.Equal(t, "unknown", changerInfo.LTOGeneration())
}

// TestTransferToIO verifies that a tape can be transferred directly from a
// storage slot into an I/O station slot.
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
		// Use a context that survives the test's own cancellation so the tape
		// is always returned to its storage slot.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()

		_ = changer.Transfer(ctx, ioSlot.Address, srcSlot.Address)
	})

	// Confirm the I/O slot now holds the tape and the source slot is empty.
	inv, err = changer.Inventory(t.Context())
	require.NoError(t, err, "inventory after transfer")

	assert.True(t, inv.IOSlots[0].Full, "I/O slot should be full after transfer")
	assert.Equal(t, barcode, inv.IOSlots[0].Barcode, "barcode in I/O slot should match")
	assert.False(t, inv.Slots[0].Full, "source slot should be empty after transfer")
}

// TestLogPages verifies that sg_logs can be queried against the mhvtl drive and
// that both TapeAlert flags (page 0x2e) and the reposition counter (page 0x30,
// total_suspended_writes) are parsed. The mhvtl IBM LTO-6 drive supports page
// 0x30, so the reposition counter must actually be measured (not silently zero):
// a fresh drive reports it measured and at zero.
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
