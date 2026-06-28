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

// TestFormatMountWriteUnmountReadIndex exercises the full LTFS path against the
// mhvtl virtual library: format a blank tape, mount it with the index sync
// deferred to unmount, write a tree of files, unmount (writing the index once),
// read the captured index back, and finally re-mount to confirm the files
// persisted on tape. This covers the package's acceptance criteria (issue #12).
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
		// Survive the test's own cancellation so the tape is always returned.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 60*time.Second)
		defer cancel()

		_ = changer.Unload(ctx, slot.Address, driveAddr)
	})

	testutil.SkipIfDriveNotReady(t, testutil.Drive0Dev(t))

	// pkg/ltfs drives the tape through the SCSI generic node (the reference LTFS
	// "sg" backend), not the nst node.
	volume := ltfs.NewVolume(testutil.Drive0SgDev(t))

	// --- Format (AC: tape formatted, LTFS volume name set to the barcode) ------
	require.NoError(t, volume.Format(t.Context(), barcode), "format tape")

	// --- Mount + write (AC: FUSE mount with sync_type=unmount; writes persist) -
	files := map[string]string{
		"manifest.txt":     "tape-archiver integration manifest\n",
		"provenance.txt":   "run-id integration barcode " + string(barcode) + "\n",
		"slices/archive.0": "first slice payload\n",
	}

	mount, err := volume.Mount(t.Context(), filepath.Join(t.TempDir(), "mnt"), filepath.Join(t.TempDir(), "work"))
	require.NoError(t, err, "mount LTFS volume")

	writeFiles(t, mount.Mountpoint(), files)

	// --- Unmount (AC: index written exactly once, at unmount; clean release) ---
	// A nil error here means the single deferred index write completed; see
	// Unmount. The drive is released so the changer can unload it.
	require.NoError(t, mount.Unmount(t.Context()), "unmount LTFS volume")

	// --- ReadIndex (AC: returns the LTFS XML index for the tape) ---------------
	index, err := mount.ReadIndex(t.Context())
	require.NoError(t, err, "read captured LTFS index")

	assert.True(t, bytes.HasPrefix(bytes.TrimSpace(index), []byte("<?xml")), "index should be XML")
	// The LTFS volume name (the tape's root directory) is the barcode (SPEC §6).
	assert.Contains(t, string(index), "<name>"+string(barcode)+"</name>", "index names the volume by barcode")

	for name := range files {
		assert.Contains(t, string(index), "<name>"+filepath.Base(name)+"</name>",
			"index should list %q", name)
	}

	// --- Re-mount (AC: files present and readable when the tape is re-mounted) -
	remount, err := volume.Mount(t.Context(), filepath.Join(t.TempDir(), "mnt2"), filepath.Join(t.TempDir(), "work2"))
	require.NoError(t, err, "re-mount LTFS volume")

	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(remount.Mountpoint(), name))
		require.NoError(t, err, "read back %q", name)
		assert.Equal(t, want, string(got), "content of %q should survive the write/re-mount cycle", name)
	}

	require.NoError(t, remount.Unmount(t.Context()), "unmount after verification")
}

// writeFiles writes each name->content pair under root, creating parent
// directories on the LTFS mount as needed.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(root, name)

		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "mkdir for %q", name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644), "write %q", name)
	}
}
