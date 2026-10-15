//go:build integration

package ltfs_test

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
	"github.com/solidDoWant/tape-archiver/pkg/ltfs"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// loadFormattedVolume loads slot 0 into drive 0, formats it, and registers an
// unload cleanup that survives test cancellation.
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

// TestMountRejectsPreexistingMountNoDrive bind-mounts a directory onto itself so
// the pre-start guard can be tested without a drive.
func TestMountRejectsPreexistingMountNoDrive(t *testing.T) {
	testutil.SkipIfLTFSUnavailable(t)

	mountpoint := filepath.Join(t.TempDir(), "mnt")
	require.NoError(t, os.MkdirAll(mountpoint, 0o755))

	if out, err := exec.CommandContext(t.Context(), "mount", "--bind", mountpoint, mountpoint).CombinedOutput(); err != nil {
		t.Skipf("cannot create a bind mount (needs privilege): %v: %s", err, out)
	}

	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "umount", mountpoint).Run()
	})

	// The guard fires before the device is touched.
	volume := ltfs.NewVolume("/dev/null")

	mount, err := volume.Mount(t.Context(), mountpoint, filepath.Join(t.TempDir(), "work"))
	require.Error(t, err, "Mount over a pre-existing mount must fail, not report success")
	assert.Nil(t, mount, "no Mount handle for a rejected mount")
	assert.Contains(t, err.Error(), "already in use",
		"error must identify the pre-existing mount")
	assert.Contains(t, err.Error(), mountpoint, "error must name the mountpoint")
}

// TestUnmountIdempotent: a second Unmount after ltfs exited cleanly must succeed.
func TestUnmountIdempotent(t *testing.T) {
	volume := loadFormattedVolume(t)

	mount, err := volume.Mount(t.Context(), filepath.Join(t.TempDir(), "mnt"), filepath.Join(t.TempDir(), "work"))
	require.NoError(t, err, "mount LTFS volume")

	writeFiles(t, mount.Mountpoint(), map[string]string{"manifest.txt": "idempotent-unmount\n"})

	require.NoError(t, mount.Unmount(t.Context()), "first unmount")

	require.NoError(t, mount.Unmount(t.Context()), "retry unmount must succeed idempotently")
}

// TestMountRejectsLivePriorMount: a live prior mount at the mountpoint must be
// refused, not mistaken for our own.
func TestMountRejectsLivePriorMount(t *testing.T) {
	volume := loadFormattedVolume(t)

	mountpoint := filepath.Join(t.TempDir(), "mnt")

	live, err := volume.Mount(t.Context(), mountpoint, filepath.Join(t.TempDir(), "work"))
	require.NoError(t, err, "initial mount")

	t.Cleanup(func() {
		_ = live.Unmount(context.WithoutCancel(t.Context()))
	})

	second, err := volume.Mount(t.Context(), mountpoint, filepath.Join(t.TempDir(), "work2"))
	require.Error(t, err, "mount over a live prior mount must fail, not report success")
	assert.Nil(t, second, "no Mount handle for a rejected mount")
	assert.Contains(t, err.Error(), "already in use",
		"error must identify the pre-existing mount")
	assert.Contains(t, err.Error(), mountpoint, "error must name the mountpoint")
}

// TestMountRejectsOrphanedMount: a SIGKILL-orphaned FUSE mount (ENOTCONN) must
// yield a descriptive error, not an opaque stat failure.
func TestMountRejectsOrphanedMount(t *testing.T) {
	volume := loadFormattedVolume(t)

	mountpoint := filepath.Join(t.TempDir(), "mnt")

	orphan, err := volume.Mount(t.Context(), mountpoint, filepath.Join(t.TempDir(), "work"))
	require.NoError(t, err, "initial mount")

	require.NoError(t, orphan.Kill(), "kill ltfs to orphan the mount")

	t.Cleanup(func() {
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
