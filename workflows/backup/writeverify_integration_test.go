//go:build integration

package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// writeVerifySlot is the storage slot both on-tape-contents tests use. It is
// distinct from the slots the sibling integration tests claim (0: session,
// 1: single-tape TestTapePath, 2: whole-run/AllowNonBlank, 4–7: multi-drive-set)
// so this fixture never collides with them. Integration tests run sequentially
// and each test blanks the tape on cleanup, so the two tests here can share it.
const writeVerifySlot = 3

// stageFile writes content to name under dir and returns a StagedSlice carrying
// the file's real SHA-256 and size. The precomputed digest is what WriteTree
// records in the manifest without re-reading the file in the write window
// (SPEC §14), so the tests verify against the exact digest the production path
// would have carried from Prepare/GeneratePAR2.
func stageFile(t *testing.T, dir, name string, content []byte) StagedSlice {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, content, 0o644), "stage %s", name)

	sum := sha256.Sum256(content)

	return StagedSlice{
		Path:      path,
		SHA256:    hex.EncodeToString(sum[:]),
		SizeBytes: int64(len(content)),
	}
}

// TestWriteTreeOnTapeContents writes a tape through the production Write path
// (FormatTape → WriteTree → FinalizeTape) with a real, non-empty archive tree,
// then re-mounts the finalized LTFS volume and byte-verifies the on-tape
// contents: every archive slice, every PAR2 recovery file, and manifest.json.
//
// This closes the coverage gap in issue #149 (AC1): the leaf copy/manifest
// functions are unit-tested against temp dirs, but nothing exercised the
// WriteTree glue (mount → copyTape → buildManifest → writeManifest) against a
// real LTFS volume with a non-empty archive list. A WriteTree that skips or
// miswires the archive copy fails the file/bytes assertions here; one that skips
// the manifest fails the manifest assertions. Skips when mhvtl or LTFS is absent.
func TestWriteTreeOnTapeContents(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)
	testutil.SkipIfLTFSUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "initial inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "at least 1 drive required")
	require.Greater(t, len(inv.Slots), writeVerifySlot, "at least %d storage slots required", writeVerifySlot+1)
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")
	require.Truef(t, inv.Slots[writeVerifySlot].Full, "slot %d must have a tape", writeVerifySlot)

	stDev := testutil.Drive0Dev(t)
	sgDev := testutil.Drive0SgDev(t)
	slotAddr := inv.Slots[writeVerifySlot].Address
	driveAddr := inv.Drives[0].Address
	expectedBarcode := inv.Slots[writeVerifySlot].Barcode
	require.NotEmptyf(t, expectedBarcode, "slot %d tape must have a barcode", writeVerifySlot)

	// Restore the library on exit: the tape blanked and parked back in its home
	// slot, drive 0 empty, so a repeat run and the sibling tests find the state
	// they expect.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 120*time.Second)
		defer cancel()

		returnAndBlank(cleanupCtx, changer, stDev, sgDev, driveAddr, slotAddr, expectedBarcode)
	})

	// Probe drive readiness up front, skipping cleanly if mhvtl left the drive
	// stuck "not ready", then unload so the Load activity below loads from scratch.
	require.NoError(t, changer.Load(t.Context(), slotAddr, driveAddr), "pre-load for readiness probe")
	testutil.SkipIfDriveNotReady(t, stDev)
	require.NoError(t, changer.Unload(t.Context(), slotAddr, driveAddr), "unload after readiness probe")

	// --- Phase: Load (resolves the SG device the way production does) ----------
	loadActs := newLoadActivities()

	loadedTapes, err := loadActs.Load(t.Context(), LoadInput{
		Changer: testutil.ChangerDev(t),
		Tapes: []TapeAssignment{
			{Drive: stDev, BlankSlot: slotAddr, TapeIndex: 0, CopyIndex: 0},
		},
	})
	require.NoError(t, err, "Load activity")
	require.Len(t, loadedTapes, 1, "Load must return one LoadedTape")

	lt := loadedTapes[0]
	require.Equal(t, expectedBarcode, lt.Barcode, "loaded barcode must match the slot tape")

	testutil.SkipIfDriveNotReady(t, stDev)

	// --- Stage a real, non-empty archive tree ---------------------------------
	// One archive (source index 0, label "photos") with two slice files and one
	// PAR2 recovery file, each with distinct bytes and a real SHA-256.
	stagingDir := t.TempDir()
	sliceZero := stageFile(t, stagingDir, "archive.000", []byte("on-tape slice zero: the quick brown fox"))
	sliceOne := stageFile(t, stagingDir, "archive.001", []byte("on-tape slice one: jumps over the lazy dog"))
	par2File := stageFile(t, stagingDir, "archive.par2", []byte("PAR2 recovery payload for the archive"))

	const label = "photos"

	archives := []TapeWriteArchive{
		{
			SourceIndex: 0,
			Label:       label,
			Slices:      []StagedSlice{sliceZero, sliceOne},
			PAR2Files:   []StagedSlice{par2File},
		},
	}

	// --- Phase: Write (Format → WriteTree → Finalize) -------------------------
	registry := newMountRegistry()
	writeActs := newWriteActivities(registry, stagingDir)

	require.NoError(t, writeActs.FormatTape(t.Context(), FormatInput{
		Device:  lt.SGDevice,
		Barcode: lt.Barcode,
	}), "FormatTape")

	require.NoError(t, writeActs.WriteTree(t.Context(), WriteTreeInput{
		Device:    lt.SGDevice,
		Barcode:   lt.Barcode,
		TapeIndex: lt.TapeIndex,
		CopyIndex: lt.CopyIndex,
		Archives:  archives,
	}), "WriteTree")

	// FinalizeTape unmounts, flushing the LTFS index to tape so the re-mount below
	// reads a durably written volume.
	_, err = writeActs.FinalizeTape(t.Context(), FinalizeInput{Device: lt.SGDevice, Barcode: lt.Barcode})
	require.NoError(t, err, "FinalizeTape")

	// --- Re-mount the finalized volume and verify on-tape contents ------------
	// Mount fresh (a different mount/work dir than WriteTree used) so the read
	// comes off the tape, not a lingering process or cache.
	mountDir := filepath.Join(t.TempDir(), "verify-mount")
	workDir := filepath.Join(t.TempDir(), "verify-work")

	mount, err := ltfs.NewVolume(lt.SGDevice).Mount(t.Context(), mountDir, workDir)
	require.NoError(t, err, "re-mount finalized volume for verification")

	t.Cleanup(func() {
		unmountCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 60*time.Second)
		defer cancel()

		_ = mount.Unmount(unmountCtx)
	})

	root := mount.Mountpoint()
	dir := archiveDirName(0, label) // archives/000-photos

	// Every staged slice and PAR2 file must be on tape with byte-identical content.
	for _, staged := range []StagedSlice{sliceZero, sliceOne, par2File} {
		tapePath := filepath.Join(root, dir, filepath.Base(staged.Path))

		got, readErr := os.ReadFile(tapePath)
		require.NoErrorf(t, readErr, "read %s back from tape", filepath.Base(staged.Path))

		want, readErr := os.ReadFile(staged.Path)
		require.NoError(t, readErr, "read source %s", filepath.Base(staged.Path))

		assert.Equalf(t, want, got, "on-tape bytes of %s must match the source", filepath.Base(staged.Path))
	}

	// manifest.json must be present at the LTFS root and describe the tape and
	// every file with the precomputed digests.
	manifestBytes, err := os.ReadFile(filepath.Join(root, manifestName))
	require.NoError(t, err, "manifest.json must be present at the LTFS root")

	var manifest TapeManifest
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest), "manifest.json must be valid JSON")

	assert.Equal(t, lt.Barcode, manifest.Barcode, "manifest barcode")
	assert.Equal(t, lt.TapeIndex, manifest.TapeIndex, "manifest tape index")
	assert.Equal(t, lt.CopyIndex, manifest.CopyIndex, "manifest copy index")

	require.Len(t, manifest.Archives, 1, "manifest must list the one archive")
	archiveManifest := manifest.Archives[0]
	assert.Equal(t, 0, archiveManifest.SourceIndex, "archive source index")

	wantFiles := []ManifestFile{
		{TapePath: filepath.Join(dir, "archive.000"), SHA256: sliceZero.SHA256, SizeBytes: sliceZero.SizeBytes},
		{TapePath: filepath.Join(dir, "archive.001"), SHA256: sliceOne.SHA256, SizeBytes: sliceOne.SizeBytes},
	}
	assert.Equal(t, wantFiles, archiveManifest.Files, "manifest slice entries")

	wantPAR2 := []ManifestFile{
		{TapePath: filepath.Join(dir, "archive.par2"), SHA256: par2File.SHA256, SizeBytes: par2File.SizeBytes},
	}
	assert.Equal(t, wantPAR2, archiveManifest.PAR2Files, "manifest PAR2 entries")
}

// TestWriteTreeCopyFailureLeavesNoManifest proves the SPEC §6 completeness
// signal end-to-end: when the archive-tree copy fails before completing,
// manifest.json must be absent from the finalized volume, so a future recoverer
// can discard an incompletely-written tape by the missing manifest alone.
//
// It induces a copyTape failure by pointing a slice at a nonexistent staged
// file: copyFile's os.Open fails, so WriteTree returns an error before
// writeManifest runs. The mount is parked in the registry before the copy, so
// FinalizeTape still unmounts/flushes the partial tape; re-mounting it must show
// no manifest.json. This closes issue #149 AC2. Skips when mhvtl or LTFS is absent.
func TestWriteTreeCopyFailureLeavesNoManifest(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)
	testutil.SkipIfLTFSUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "initial inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "at least 1 drive required")
	require.Greater(t, len(inv.Slots), writeVerifySlot, "at least %d storage slots required", writeVerifySlot+1)
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")
	require.Truef(t, inv.Slots[writeVerifySlot].Full, "slot %d must have a tape", writeVerifySlot)

	stDev := testutil.Drive0Dev(t)
	sgDev := testutil.Drive0SgDev(t)
	slotAddr := inv.Slots[writeVerifySlot].Address
	driveAddr := inv.Drives[0].Address
	barcode := inv.Slots[writeVerifySlot].Barcode
	require.NotEmptyf(t, barcode, "slot %d tape must have a barcode", writeVerifySlot)

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 120*time.Second)
		defer cancel()

		returnAndBlank(cleanupCtx, changer, stDev, sgDev, driveAddr, slotAddr, barcode)
	})

	// Load and wait for readiness; keep the tape in the drive so FormatTape can
	// format it in place.
	require.NoError(t, changer.Load(t.Context(), slotAddr, driveAddr), "load tape")
	testutil.SkipIfDriveNotReady(t, stDev)

	stagingDir := t.TempDir()
	registry := newMountRegistry()
	writeActs := newWriteActivities(registry, stagingDir)

	require.NoError(t, writeActs.FormatTape(t.Context(), FormatInput{
		Device:  sgDev,
		Barcode: barcode,
	}), "FormatTape")

	// WriteTree with a slice pointing at a file that does not exist: copyTape's
	// copyFile can't open it, so WriteTree fails during the copy — before
	// writeManifest is reached.
	missingSlice := StagedSlice{
		Path:      filepath.Join(stagingDir, "does-not-exist.000"),
		SHA256:    "deadbeef",
		SizeBytes: 42,
	}

	err = writeActs.WriteTree(t.Context(), WriteTreeInput{
		Device:    sgDev,
		Barcode:   barcode,
		TapeIndex: 0,
		CopyIndex: 0,
		Archives: []TapeWriteArchive{
			{SourceIndex: 0, Label: "photos", Slices: []StagedSlice{missingSlice}},
		},
	})
	require.Error(t, err, "WriteTree must fail when a staged slice is missing")

	// WriteTree parked the mount before the copy; finalize it to unmount/flush the
	// partially written tape, exactly as the production Teardown/Finalize path
	// would leave it.
	_, err = writeActs.FinalizeTape(t.Context(), FinalizeInput{Device: sgDev, Barcode: barcode})
	require.NoError(t, err, "FinalizeTape must unmount the partially written tape")

	// Re-mount and assert manifest.json is absent — the completeness signal must
	// not be present on an incompletely written tape.
	mountDir := filepath.Join(t.TempDir(), "verify-mount")
	workDir := filepath.Join(t.TempDir(), "verify-work")

	mount, err := ltfs.NewVolume(sgDev).Mount(t.Context(), mountDir, workDir)
	require.NoError(t, err, "re-mount partially written volume")

	t.Cleanup(func() {
		unmountCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 60*time.Second)
		defer cancel()

		_ = mount.Unmount(unmountCtx)
	})

	_, statErr := os.Stat(filepath.Join(mount.Mountpoint(), manifestName))
	require.Error(t, statErr, "manifest.json must not exist on an incompletely written tape")
	assert.True(t, os.IsNotExist(statErr), "stat error must be not-exist, got: %v", statErr)
}
