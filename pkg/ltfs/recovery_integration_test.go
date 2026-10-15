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

// Asserted here because the reverse would be an import cycle.
var _ ltfs.BlockReader = (*tape.RawReader)(nil)

// TestIndexLossRecovery_CapturedExtents is the tier-2 index-loss scenario (#21):
// with no LTFS mount, rebuild files byte-for-byte from the captured index's
// extents via raw SCSI reads.
func TestIndexLossRecovery_CapturedExtents(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)
	testutil.SkipIfLTFSUnavailable(t)

	drive, barcode := loadFirstTape(t)

	testutil.SkipIfDriveNotReady(t, testutil.Drive0Dev(t))

	volume := ltfs.NewVolume(testutil.Drive0SgDev(t))
	require.NoError(t, volume.Format(t.Context(), barcode), "format tape")

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

	// No LTFS mount anywhere below this line.
	reader, err := drive.OpenRawReader()
	require.NoError(t, err, "open raw block reader")

	t.Cleanup(func() { _ = reader.Close() })

	for name, want := range files {
		got, err := index.ExtractFile(t.Context(), name, reader)
		require.NoErrorf(t, err, "extract %q from captured extents", name)
		assert.Equalf(t, want, string(got), "recovered bytes of %q must match what was written", name)
	}
}

// TestIndexLossRecovery_DeepRecovery is the tier-1 scenario: corrupt the index
// partition and rebuild it with `ltfsck --deep-recovery` from the data
// partition's copy.
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

	// Target the index itself, not the partition labels deep recovery also needs.
	indexXML, err := mount.ReadIndex(t.Context())
	require.NoError(t, err, "read captured LTFS index")

	parsed, err := ltfs.ParseIndex(indexXML)
	require.NoError(t, err, "parse captured LTFS index")
	require.Equal(t, "a", parsed.Location.Partition, "index generation should live in the index partition")

	damageIndexPartition(t, sgDevice, parsed.Location.StartBlock)

	badMount, err := volume.Mount(t.Context(), filepath.Join(t.TempDir(), "mnt-bad"), filepath.Join(t.TempDir(), "work-bad"))
	if err == nil {
		_ = badMount.Unmount(t.Context())
		t.Fatal("expected a normal mount to fail after the index partition was damaged")
	}

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

// loadFirstTape loads the first storage tape into drive 0, unloading on cleanup.
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

// patternPayload returns n bytes whose pattern exposes misread block boundaries
// or wrong-partition reads.
func patternPayload(n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = byte((i*31 + 7) % 251)
	}

	return string(buf)
}

func readMountFile(mountpoint, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(mountpoint, name))

	return string(data), err
}

// damageIndexPartition overwrites index-partition blocks from startBlock via
// sg_raw LOCATE(16) + WRITE(6), leaving the labels and the data-partition copy
// intact. The st driver here rejects mt setpartition.
func damageIndexPartition(t *testing.T, sgDevice string, startBlock uint64) {
	t.Helper()

	garbage := filepath.Join(t.TempDir(), "garbage.bin")
	require.NoError(t, os.WriteFile(garbage, bytes.Repeat([]byte{0xFF}, 65536), 0o644))

	// LOCATE(16): CP=1, partition 0, block = startBlock (big-endian).
	lba := make([]byte, 8)
	binary.BigEndian.PutUint64(lba, startBlock)

	locate := []string{"92", "02", "00", "00"}
	for _, b := range lba {
		locate = append(locate, fmt.Sprintf("%02x", b))
	}

	locate = append(locate, "00", "00", "00", "00")
	runCmd(t, "sg_raw", append([]string{sgDevice}, locate...)...)

	// WRITE(6) variable-length, 65536-byte blocks.
	for range 6 {
		runCmd(t, "sg_raw", "-s", "65536", "-i", garbage, sgDevice,
			"0a", "00", "01", "00", "00", "00")
	}
}

// runLTFSck runs `ltfsck --deep-recovery`. Like fsck it exits non-zero when it
// repairs, so the caller's read-back is the real proof.
func runLTFSck(t *testing.T, sgDevice string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ltfsck", "--deep-recovery", sgDevice).CombinedOutput()
	t.Logf("ltfsck --deep-recovery exit=%v\n%s", err, out)
}

func runCmd(t *testing.T, name string, args ...string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	require.NoErrorf(t, err, "%s %v: %s", name, args, out)
}
