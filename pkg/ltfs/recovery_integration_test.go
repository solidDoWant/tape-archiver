//go:build integration

package ltfs_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
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

// The SCSI-backed raw reader must satisfy the extractor's block-read seam. This
// is asserted here (not in pkg/tape) because pkg/ltfs imports pkg/tape, so the
// reverse assertion would be an import cycle.
var _ ltfs.BlockReader = (*tape.RawReader)(nil)

// TestIndexLossRecovery_CapturedExtents is the tier-2 index-loss scenario
// (docs/recovery-procedure.md; issue #21): with the on-tape LTFS index unusable
// and no LTFS mount, reconstruct a file's exact bytes using only the captured
// index's extents plus raw SCSI LOCATE/READ. It writes files via LTFS, captures
// the index, unmounts, then — without mounting LTFS — parses the captured index
// and extracts each file straight off the tape by (partition, block), asserting
// byte-for-byte equality. This proves the captured index is a sufficient,
// load-bearing recovery path and exercises the partition-label -> SCSI-partition
// mapping end-to-end against mhvtl.
func TestIndexLossRecovery_CapturedExtents(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)
	testutil.SkipIfLTFSUnavailable(t)

	drive, barcode := loadFirstTape(t)

	testutil.SkipIfDriveNotReady(t, testutil.Drive0Dev(t))

	volume := ltfs.NewVolume(testutil.Drive0SgDev(t))
	require.NoError(t, volume.Format(t.Context(), barcode), "format tape")

	// A multi-block file (forces an extent that spans several tape blocks) and a
	// small one, both with deterministic content so recovery is checkable to the
	// byte.
	files := map[string]string{
		"archives/000/archive.000": patternPayload(1_300_000),
		"archives/000/small.txt":   "tape-archiver index-loss recovery\n",
	}

	mount, err := volume.Mount(t.Context(), filepath.Join(t.TempDir(), "mnt"), filepath.Join(t.TempDir(), "work"))
	require.NoError(t, err, "mount LTFS volume")

	writeFiles(t, mount.Mountpoint(), files)

	require.NoError(t, mount.Unmount(t.Context()), "unmount LTFS volume")

	indexXML, err := mount.ReadIndex(t.Context())
	require.NoError(t, err, "read captured LTFS index")

	index, err := ltfs.ParseIndex(indexXML)
	require.NoError(t, err, "parse captured LTFS index")

	// The recovery path: raw block reads through the drive's sg node, with no
	// LTFS mount anywhere below this line.
	reader, err := drive.OpenRawReader()
	require.NoError(t, err, "open raw block reader")

	t.Cleanup(func() { _ = reader.Close() })

	for name, want := range files {
		got, err := index.ExtractFile(t.Context(), name, reader)
		require.NoErrorf(t, err, "extract %q from captured extents", name)
		assert.Equalf(t, want, string(got), "recovered bytes of %q must match what was written", name)
	}
}

// TestIndexLossRecovery_DeepRecovery is the tier-1 index-loss scenario: the
// index partition is damaged but the data partition (which holds a copy of the
// index, written on every LTFS sync) is intact, so `ltfsck --deep-recovery`
// rebuilds the volume. It writes a file, corrupts the index partition, confirms
// a normal mount now fails, runs deep recovery, and asserts the file reads back
// intact.
func TestIndexLossRecovery_DeepRecovery(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)
	testutil.SkipIfLTFSUnavailable(t)

	_, barcode := loadFirstTape(t)

	testutil.SkipIfDriveNotReady(t, testutil.Drive0Dev(t))

	sgDevice := testutil.Drive0SgDev(t)

	volume := ltfs.NewVolume(sgDevice)
	require.NoError(t, volume.Format(t.Context(), barcode), "format tape")

	const (
		fileName = "archives/000/archive.000"
		want     = "deep-recovery payload — index partition damaged, data intact\n"
	)

	mount, err := volume.Mount(t.Context(), filepath.Join(t.TempDir(), "mnt"), filepath.Join(t.TempDir(), "work"))
	require.NoError(t, err, "mount LTFS volume")

	writeFiles(t, mount.Mountpoint(), map[string]string{fileName: want})

	require.NoError(t, mount.Unmount(t.Context()), "unmount LTFS volume")

	// Learn where this index generation was written from the captured index, so
	// the corruption hits the index itself and not the partition labels (which
	// deep recovery also needs). LTFS writes the index to partition "a".
	indexXML, err := mount.ReadIndex(t.Context())
	require.NoError(t, err, "read captured LTFS index")

	parsed, err := ltfs.ParseIndex(indexXML)
	require.NoError(t, err, "parse captured LTFS index")
	require.Equal(t, "a", parsed.Location.Partition, "index generation should live in the index partition")

	// Overwrite the index at its recorded location on the index partition,
	// leaving the volume labels (earlier blocks) and the data partition's index
	// copy intact — the exact damage `ltfsck --deep-recovery` is meant to heal.
	damageIndexPartition(t, sgDevice, parsed.Location.StartBlock)

	// A normal mount should now fail (the index partition is unreadable).
	badMount, err := volume.Mount(t.Context(), filepath.Join(t.TempDir(), "mnt-bad"), filepath.Join(t.TempDir(), "work-bad"))
	if err == nil {
		_ = badMount.Unmount(t.Context())
		t.Fatal("expected a normal mount to fail after the index partition was damaged")
	}

	// Deep recovery rebuilds the index from the data-partition copy.
	runLTFSck(t, sgDevice)

	remount, err := volume.Mount(t.Context(), filepath.Join(t.TempDir(), "mnt-ok"), filepath.Join(t.TempDir(), "work-ok"))
	require.NoError(t, err, "re-mount after deep recovery")

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 60*time.Second)
		defer cancel()

		_ = remount.Unmount(ctx)
	})

	got, err := readMountFile(remount.Mountpoint(), fileName)
	require.NoError(t, err, "read back the file after deep recovery")
	assert.Equal(t, want, got, "file must survive index-partition damage + deep recovery")
}

// loadFirstTape loads the first storage tape into drive 0 and returns a Drive
// handle for it and its barcode, arranging for the tape to be unloaded on
// cleanup. It mirrors the setup in TestFormatMountWriteUnmountReadIndex.
func loadFirstTape(t *testing.T) (*tape.Drive, tape.Barcode) {
	t.Helper()

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

	return tape.NewDrive(testutil.Drive0Dev(t), tape.WithSGDevice(testutil.Drive0SgDev(t))), barcode
}

// patternPayload returns n bytes of a deterministic, non-repeating-per-block
// pattern so a byte-for-byte comparison would catch a misread block boundary or
// a wrong-partition read.
func patternPayload(n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = byte((i*31 + 7) % 251)
	}

	return string(buf)
}

// readMountFile reads a file from an LTFS mountpoint.
func readMountFile(mountpoint, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(mountpoint, name))

	return string(data), err
}

// damageIndexPartition overwrites blocks in the index partition ("a", SCSI
// partition 0) starting at startBlock — the block the captured index records as
// the index's own location — so a normal LTFS mount fails while the volume labels
// (earlier blocks) and the data partition's index copy stay intact for deep
// recovery. It uses raw SCSI via sg_raw (the same idiom as the e2e ERASE helper):
// LOCATE(16) to the index location, then WRITE(6) several garbage blocks. The st
// driver here rejects mt setpartition, so raw SCSI is the reliable path.
func damageIndexPartition(t *testing.T, sgDevice string, startBlock uint64) {
	t.Helper()

	garbage := filepath.Join(t.TempDir(), "garbage.bin")
	require.NoError(t, os.WriteFile(garbage, bytes.Repeat([]byte{0xFF}, 65536), 0o644))

	// LOCATE(16): CP=1 (byte 1 = 0x02), partition 0 (byte 3 = index partition "a"),
	// logical block = startBlock (bytes 4-11, big-endian).
	lba := make([]byte, 8)
	binary.BigEndian.PutUint64(lba, startBlock)

	locate := []string{"92", "02", "00", "00"}
	for _, b := range lba {
		locate = append(locate, fmt.Sprintf("%02x", b))
	}

	locate = append(locate, "00", "00", "00", "00")
	runCmd(t, "sg_raw", append([]string{sgDevice}, locate...)...)

	// WRITE(6) variable-length (FIXED=0), 0x10000 = 65536 bytes per block. Each
	// write advances one block, clobbering several index blocks from startBlock on.
	for range 6 {
		runCmd(t, "sg_raw", "-s", "65536", "-i", garbage, sgDevice,
			"0a", "00", "01", "00", "00", "00")
	}
}

// runLTFSck runs `ltfsck --deep-recovery` against the drive's sg node. Like
// fsck, ltfsck exits non-zero when it *makes* repairs, so its exit code is not
// treated as failure — the authoritative proof of recovery is the remount and
// byte-for-byte read-back the caller performs next.
func runLTFSck(t *testing.T, sgDevice string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ltfsck", "--deep-recovery", sgDevice).CombinedOutput()
	t.Logf("ltfsck --deep-recovery exit=%v\n%s", err, out)
}

// runCmd runs a command, failing the test with its combined output on error.
func runCmd(t *testing.T, name string, args ...string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	require.NoErrorf(t, err, "%s %v: %s", name, args, out)
}
