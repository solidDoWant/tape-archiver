//go:build integration

package ltfs_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/ltfs"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// TestFormatMountWriteUnmountReadIndex runs format, mount, write, unmount, read
// index and re-mount against mhvtl (#12).
func TestFormatMountWriteUnmountReadIndex(t *testing.T) {
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

	files := map[string]string{
		"manifest.txt":     "tape-archiver integration manifest\n",
		"provenance.txt":   "run-id integration barcode " + string(barcode) + "\n",
		"slices/archive.0": "first slice payload\n",
	}

	mount, err := volume.Mount(t.Context(), filepath.Join(t.TempDir(), "mnt"), filepath.Join(t.TempDir(), "work"))
	require.NoError(t, err, "mount LTFS volume")

	writeFiles(t, mount.Mountpoint(), files)

	// A nil error means the single deferred index write completed.
	require.NoError(t, mount.Unmount(t.Context()), "unmount LTFS volume")

	index, err := mount.ReadIndex(t.Context())
	require.NoError(t, err, "read captured LTFS index")

	assert.True(t, bytes.HasPrefix(bytes.TrimSpace(index), []byte("<?xml")), "index should be XML")
	// The LTFS volume name is the barcode (SPEC §6).
	assert.Contains(t, string(index), "<name>"+string(barcode)+"</name>", "index names the volume by barcode")

	for name := range files {
		assert.Contains(t, string(index), "<name>"+filepath.Base(name)+"</name>",
			"index should list %q", name)
	}

	remount, err := volume.Mount(t.Context(), filepath.Join(t.TempDir(), "mnt2"), filepath.Join(t.TempDir(), "work2"))
	require.NoError(t, err, "re-mount LTFS volume")

	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(remount.Mountpoint(), name))
		require.NoError(t, err, "read back %q", name)
		assert.Equal(t, want, string(got), "content of %q should survive the write/re-mount cycle", name)
	}

	require.NoError(t, remount.Unmount(t.Context()), "unmount after verification")
}

// writeFiles writes each name->content pair under root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(root, name)

		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "mkdir for %q", name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644), "write %q", name)
	}
}
