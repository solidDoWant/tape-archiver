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

// tapeFixture is a blanked tape parked in a storage slot, ready for a run to
// load, plus the library coordinates and the run-config Library block that names
// it. The data worker (container) performs the actual Load/Write against these
// same host devices; this fixture only prepares and, on cleanup, restores them.
type tapeFixture struct {
	changer   *tape.Changer
	slotAddr  int
	driveAddr int
	barcode   tape.Barcode
	library   config.Library
}

// prepareBlankTape mirrors the integration suite's setup: it confirms drive 0 is
// empty, picks a populated storage slot, loads and blanks that tape, and unloads
// it, leaving a genuinely blank tape for the run to load. It registers a cleanup
// that returns the library to "drive 0 empty, tape in its slot" so a repeat run
// (and the sibling verify-fault test) find the expected starting state.
func prepareBlankTape(t *testing.T) tapeFixture {
	t.Helper()

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "at least one drive required")
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")

	// Slot index 2 matches the sibling integration whole-run test's convention so
	// the two never collide on a shared mhvtl library.
	require.GreaterOrEqual(t, len(inv.Slots), 3, "at least three storage slots required")
	slot := inv.Slots[2]
	require.True(t, slot.Full, "slot 2 must hold a tape")
	require.NotEmpty(t, slot.Barcode, "slot 2 tape must have a barcode")

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

	// Restore the library before touching the drive, so even an early failure or a
	// readiness skip leaves drive 0 empty and the tape back in its slot.
	t.Cleanup(func() { returnTapeToSlot(fixture) })

	require.NoError(t, changer.Load(t.Context(), fixture.slotAddr, fixture.driveAddr), "pre-load for readiness/blanking")
	testutil.SkipIfDriveNotReady(t, stDev)
	eraseLoadedTape(t.Context(), stDev, sgDev)
	require.NoError(t, changer.Unload(t.Context(), fixture.slotAddr, fixture.driveAddr), "unload after blanking")

	return fixture
}

// eraseLoadedTape issues a short SCSI ERASE (CDB 0x19, LONG=0) to the tape in the
// drive, resetting mhvtl's state to blank without a long physical erase. It
// rewinds first (bounded) so ERASE starts at BOT. Best-effort, mirroring the
// integration suite.
func eraseLoadedTape(ctx context.Context, stDev, sgDev string) {
	rewindCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = exec.CommandContext(rewindCtx, "mt", "-f", stDev, "rewind").Run()

	cancel()

	_ = exec.CommandContext(ctx, "sg_raw", sgDev,
		"0x19", "0x00", "0x00", "0x00", "0x00", "0x00").Run()
}

// returnTapeToSlot restores the library to "drive 0 empty, tape in its storage
// slot" after a run. It never loads the drive. Best-effort: failures are ignored
// so cleanup never fails a passing test.
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
