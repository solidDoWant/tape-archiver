//go:build integration

package backup

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// TestTeardownAbortReleasesDriveForEject: when Teardown's Unmount fails and it
// kills ltfs, it must release the stale mountpoint and the drive lock so Eject
// returns the tape to a slot instead of leaving it in the drive (#442). mhvtl
// does not enforce PREVENT MEDIUM REMOVAL, so on mhvtl the lock half is proven
// by pkg/tape's command-path test and real hardware, not by this unload.
func TestTeardownAbortReleasesDriveForEject(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)
	testutil.SkipIfLTFSUnavailable(t)

	changerDev := testutil.ChangerDev(t)
	changer := tape.NewChanger(changerDev)

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "no drives found")
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")
	require.True(t, inv.Slots[0].Full, "slot 1 must have a tape")

	slot := inv.Slots[0]
	driveAddr := inv.Drives[0].Address
	barcode := slot.Barcode
	require.NotEmpty(t, barcode)

	require.NoError(t, changer.Load(t.Context(), slot.Address, driveAddr), "load tape")

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 60*time.Second)
		defer cancel()

		cleanupInv, invErr := changer.Inventory(ctx)
		if invErr != nil {
			return
		}

		for _, io := range cleanupInv.IOSlots {
			if io.Full && io.Barcode == barcode {
				_ = changer.Transfer(ctx, io.Address, slot.Address)
			}
		}

		if cleanupInv.Drives[0].Loaded {
			_ = tape.AllowMediumRemoval(ctx, testutil.Drive0SgDev(t))
			_ = changer.Unload(ctx, slot.Address, driveAddr)
		}
	})

	testutil.SkipIfDriveNotReady(t, testutil.Drive0Dev(t))

	sgDev := testutil.Drive0SgDev(t)
	registry := newMountRegistry()
	acts := newWriteActivities(registry, t.TempDir())

	require.NoError(t, acts.FormatTape(t.Context(), FormatInput{Device: sgDev, Barcode: barcode}), "FormatTape")
	require.NoError(t, writeTreeErr(acts.WriteTree(t.Context(), WriteTreeInput{Device: sgDev, Barcode: barcode})),
		"WriteTree")

	mount, ok := registry.Get(sgDev)
	require.True(t, ok, "WriteTree must park the mount")

	mountpoint := mount.Mountpoint()

	// An already-cancelled ctx fails Unmount before fusermount runs, forcing the
	// Kill fallback with ltfs still holding the drive.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	err = registry.Teardown(cancelled)
	require.Error(t, err, "the failed unmount must still be reported")
	assert.NotContains(t, err.Error(), "abort:", "releasing the killed mount must succeed")

	_, statErr := os.Stat(mountpoint)
	require.NoError(t, statErr, "the mountpoint must not be left orphaned (ENOTCONN)")
	assert.False(t, inMountinfo(t, mountpoint), "the killed FUSE mount must be detached")

	result, err := newEjectActivities().Eject(t.Context(), EjectInput{
		Changer:      changerDev,
		WrittenTapes: []WrittenTape{{Barcode: barcode, SourceSlot: slot.Address}},
	})
	require.NoError(t, err, "Eject after a killed mount")
	assert.Empty(t, result.Remaining, "the tape must reach the I/O station")

	inv, err = changer.Inventory(t.Context())
	require.NoError(t, err, "inventory after eject")
	assert.False(t, inv.Drives[0].Loaded, "the tape must not be left in the drive")
	assert.Contains(t, barcodesInIOStation(inv), barcode, "the tape must be in the I/O station")
}

// inMountinfo reports whether path is a mount point in this mount namespace.
func inMountinfo(t *testing.T, path string) bool {
	t.Helper()

	data, err := os.ReadFile("/proc/self/mountinfo")
	require.NoError(t, err)

	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) >= 5 && fields[4] == path {
			return true
		}
	}

	return false
}
