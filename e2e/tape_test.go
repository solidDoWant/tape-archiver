//go:build e2e

package e2e

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// tapeFixture is a blanked tape parked in a storage slot plus the Library block
// naming it. The fixture only prepares and restores; the data worker writes.
type tapeFixture struct {
	changer   *tape.Changer
	slotAddr  int
	driveAddr int
	barcode   tape.Barcode
	library   config.Library
}

// prepareBlankTape prepares a blank tape from storage slot index 2.
func prepareBlankTape(t *testing.T) tapeFixture {
	t.Helper()

	return prepareBlankTapeAt(t, 2)
}

// prepareBlankTapeAt blanks the tape at slotIndex (drive 0 must be empty) and
// registers a cleanup restoring "drive 0 empty, tape in its slot". Tests use
// distinct slots so they never collide on the shared library.
func prepareBlankTapeAt(t *testing.T, slotIndex int) tapeFixture {
	t.Helper()

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "at least one drive required")
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")

	require.Greater(t, len(inv.Slots), slotIndex, "storage slot %d required", slotIndex)
	slot := inv.Slots[slotIndex]
	require.Truef(t, slot.Full, "slot %d must hold a tape", slotIndex)
	require.NotEmptyf(t, slot.Barcode, "slot %d tape must have a barcode", slotIndex)

	stDev := testutil.Drive0Dev(t)
	sgDev := testutil.Drive0SgDev(t)

	fixture := tapeFixture{
		changer:   changer,
		slotAddr:  slot.Address,
		driveAddr: inv.Drives[0].Address,
		barcode:   slot.Barcode,
		library: config.Library{
			Changer:           testutil.ChangerDev(t),
			Drives:            []string{stDev},
			BlankSlots:        []int{slot.Address},
			TapeCapacityBytes: 2_500_000_000_000,
		},
	}

	// Registered before touching the drive so any failure still restores it.
	t.Cleanup(func() { returnTapeToSlot(fixture) })

	require.NoError(t, changer.Load(t.Context(), fixture.slotAddr, fixture.driveAddr), "pre-load for readiness/blanking")
	// TestMain already proved this kernel can ready the drive, so a drive that
	// does not ready now has wedged: fail rather than skip.
	testutil.RequireDriveReady(t, stDev)
	eraseLoadedTape(t.Context(), stDev, sgDev)
	require.NoError(t, changer.Unload(t.Context(), fixture.slotAddr, fixture.driveAddr), "unload after blanking")

	return fixture
}

// multiTapeFixture is several blank tapes on one drive, exercising the drive-set
// loop (issue #66).
type multiTapeFixture struct {
	barcodes []tape.Barcode
	library  config.Library
}

// prepareBlankTapesAt blanks each slot and returns a single-drive Library naming
// them all as blank sources.
func prepareBlankTapesAt(t *testing.T, slotIndexes ...int) multiTapeFixture {
	t.Helper()

	require.NotEmpty(t, slotIndexes, "at least one slot index required")

	var (
		barcodes   []tape.Barcode
		blankSlots []int
		drives     []string
	)

	for _, index := range slotIndexes {
		fixture := prepareBlankTapeAt(t, index)
		barcodes = append(barcodes, fixture.barcode)
		blankSlots = append(blankSlots, fixture.slotAddr)
		drives = fixture.library.Drives // the same single drive for every tape
	}

	return multiTapeFixture{
		barcodes: barcodes,
		library: config.Library{
			Changer:           testutil.ChangerDev(t),
			Drives:            drives,
			BlankSlots:        blankSlots,
			TapeCapacityBytes: 2_500_000_000_000,
		},
	}
}

// eraseLoadedTape issues a short SCSI ERASE (CDB 0x19, LONG=0), blanking mhvtl
// without a long erase. It rewinds first so ERASE starts at BOT. Best-effort.
func eraseLoadedTape(ctx context.Context, stDev, sgDev string) {
	rewindCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = exec.CommandContext(rewindCtx, "mt", "-f", stDev, "rewind").Run()

	cancel()

	_ = exec.CommandContext(ctx, "sg_raw", sgDev,
		"0x19", "0x00", "0x00", "0x00", "0x00", "0x00").Run()
}

// returnTapeToSlot best-effort restores "drive 0 empty, tape in its slot"
// without ever loading the drive.
func returnTapeToSlot(fixture tapeFixture) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	inv, err := fixture.changer.Inventory(cleanupCtx)
	if err != nil {
		return
	}

	// A tape still in drive 0 (run failed before Eject): unload to its slot.
	if len(inv.Drives) > 0 && inv.Drives[0].Loaded {
		_ = fixture.changer.Unload(cleanupCtx, fixture.slotAddr, fixture.driveAddr)

		return
	}

	// Otherwise it was parked in an I/O slot by Eject: move it back to storage.
	for _, io := range inv.IOSlots {
		if io.Full && io.Barcode == fixture.barcode {
			_ = fixture.changer.Transfer(cleanupCtx, io.Address, fixture.slotAddr)

			return
		}
	}
}
