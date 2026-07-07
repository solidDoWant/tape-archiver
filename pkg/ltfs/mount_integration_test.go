//go:build integration

package ltfs_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/ltfs"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// loadFormattedVolume loads slot 0 into drive 0, formats the tape, and returns a
// Volume bound to the drive. It registers an unload cleanup that survives the
// test's own cancellation. It mirrors the setup in
// TestFormatMountWriteUnmountReadIndex and is shared by the mount-lifecycle tests.
func loadFormattedVolume(t *testing.T) *ltfs.Volume {
	t.Helper()

	testutil.SkipIfMhvtlUnavailable(t)
	testutil.SkipIfLTFSUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "no drives found")
	require.NotEmpty(t, inv.Slots, "no storage slots found")
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")
	require.True(t, inv.Slots[0].Full, "slot 1 must have a tape")

	slot := inv.Slots[0]
	driveAddr := inv.Drives[0].Address
	barcode := slot.Barcode
	require.NotEmpty(t, barcode, "loaded tape must have a barcode")

	require.NoError(t, changer.Load(t.Context(), slot.Address, driveAddr), "load tape into drive 0")

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 60*time.Second)
		defer cancel()

		_ = changer.Unload(ctx, slot.Address, driveAddr)
	})

	testutil.SkipIfDriveNotReady(t, testutil.Drive0Dev(t))

	volume := ltfs.NewVolume(testutil.Drive0SgDev(t))
	require.NoError(t, volume.Format(t.Context(), barcode), "format tape")

	return volume
}

// TestUnmountIdempotent covers AC1 against mhvtl: a second Unmount after the ltfs
// process has already exited cleanly must succeed rather than fail on a
// re-issued fusermount -u. On the unfixed code the second call re-runs
// fusermount -u against the already-detached mountpoint and exits non-zero.
func TestUnmountIdempotent(t *testing.T) {
	volume := loadFormattedVolume(t)

	mount, err := volume.Mount(t.Context(), filepath.Join(t.TempDir(), "mnt"), filepath.Join(t.TempDir(), "work"))
	require.NoError(t, err, "mount LTFS volume")

	writeFiles(t, mount.Mountpoint(), map[string]string{"manifest.txt": "idempotent-unmount\n"})

	// First Unmount: detaches and waits for the index write to complete.
	require.NoError(t, mount.Unmount(t.Context()), "first unmount")

	// Second Unmount, after the ltfs process has exited cleanly: must be a no-op
	// success, not a spurious fusermount failure. This is the AC1 scenario.
	require.NoError(t, mount.Unmount(t.Context()), "retry unmount must succeed idempotently")
}

// TestMountRejectsLivePriorMount covers AC2 Case A against mhvtl: when a live
// mount from a prior run already occupies the barcode's mountpoint, a new Mount
// must NOT report success (the unfixed code returns immediately from the foreign
// st_dev), and must fail with an error identifying the pre-existing mount.
func TestMountRejectsLivePriorMount(t *testing.T) {
	volume := loadFormattedVolume(t)

	mountpoint := filepath.Join(t.TempDir(), "mnt")

	live, err := volume.Mount(t.Context(), mountpoint, filepath.Join(t.TempDir(), "work"))
	require.NoError(t, err, "initial mount")

	t.Cleanup(func() {
		_ = live.Unmount(context.WithoutCancel(t.Context()))
	})

	// A second Mount at the same (still-mounted) mountpoint must be refused before
	// any ltfs is spawned, with an error naming the pre-existing mount.
	second, err := volume.Mount(t.Context(), mountpoint, filepath.Join(t.TempDir(), "work2"))
	require.Error(t, err, "mount over a live prior mount must fail, not report success")
	assert.Nil(t, second, "no Mount handle for a rejected mount")
	assert.Contains(t, err.Error(), "already in use",
		"error must identify the pre-existing mount")
	assert.Contains(t, err.Error(), mountpoint, "error must name the mountpoint")
}

// TestMountRejectsOrphanedMount covers AC2 Case B against mhvtl: a SIGKILL-
// orphaned FUSE mount (stat returns ENOTCONN, but it is still listed in
// /proc/self/mountinfo) at the barcode's mountpoint must cause a new Mount to
// fail with a descriptive error, not the opaque ENOTCONN stat failure the
// unfixed code surfaces.
func TestMountRejectsOrphanedMount(t *testing.T) {
	volume := loadFormattedVolume(t)

	mountpoint := filepath.Join(t.TempDir(), "mnt")

	orphan, err := volume.Mount(t.Context(), mountpoint, filepath.Join(t.TempDir(), "work"))
	require.NoError(t, err, "initial mount")

	// Orphan the FUSE mount by killing the ltfs daemon without unmounting.
	require.NoError(t, orphan.Kill(), "kill ltfs to orphan the mount")

	t.Cleanup(func() {
		// Release the orphaned FUSE mount so the drive can be unloaded.
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "fusermount", "-u", mountpoint).Run()
	})

	second, err := volume.Mount(t.Context(), mountpoint, filepath.Join(t.TempDir(), "work2"))
	require.Error(t, err, "mount over an orphaned mount must fail with a descriptive error")
	assert.Nil(t, second, "no Mount handle for a rejected mount")
	assert.Contains(t, err.Error(), "already in use",
		"error must identify the stale mount rather than an opaque status-check failure")
	assert.NotContains(t, err.Error(), "transport endpoint is not connected",
		"error must not be the opaque ENOTCONN stat failure")
}
