//go:build integration

package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// TestSessionSplitWriteWithFinalizeRetry exercises the Write phase's session
// model end-to-end against mhvtl:
//
//  1. FormatTape formats a blank virtual tape.
//  2. WriteTree mounts the LTFS volume and parks the mount in the registry.
//  3. A sentinel file is written directly to the mountpoint to verify the
//     mount is live and that the tree survives the retry cycle.
//  4. FinalizeTape is called with an already-cancelled context: the ctx.Err()
//     guard fires, the mount is left untouched in the registry.
//  5. FinalizeTape is called again with a valid context: it retrieves the
//     still-live mount, unmounts, and captures the index.
//  6. The captured index is validated as LTFS XML.
//  7. The volume is re-mounted and the sentinel file is confirmed present,
//     proving the tree was not re-copied on the retry.
//
// Covers ACs 1–5 of issue #62 (session-pinned split write, Finalize retry,
// cleanup, non-session phases unchanged, mhvtl integration).
func TestSessionSplitWriteWithFinalizeRetry(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)
	testutil.SkipIfLTFSUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "no drives found")
	require.NotEmpty(t, inv.Slots, "no storage slots found")
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")
	require.True(t, inv.Slots[0].Full, "slot 0 must have a tape")

	slot := inv.Slots[0]
	driveAddr := inv.Drives[0].Address
	barcode := slot.Barcode
	require.NotEmpty(t, barcode)

	require.NoError(t, changer.Load(t.Context(), slot.Address, driveAddr), "load tape")

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 60*time.Second)
		defer cancel()

		_ = changer.Unload(cleanupCtx, slot.Address, driveAddr)
	})

	testutil.SkipIfDriveNotReady(t, testutil.Drive0Dev(t))

	sgDev := testutil.Drive0SgDev(t)
	registry := newMountRegistry()
	acts := newWriteActivities(registry)

	// --- FormatTape (AC: tape formatted, LTFS volume name = barcode) ----------
	require.NoError(t, acts.FormatTape(t.Context(), FormatInput{
		Device:  sgDev,
		Barcode: barcode,
	}), "FormatTape")

	// --- WriteTree (AC: mount live; mount parked in registry) -----------------
	mountDir := t.TempDir()
	workDir := t.TempDir()

	require.NoError(t, acts.WriteTree(t.Context(), WriteTreeInput{
		Device:   sgDev,
		MountDir: mountDir,
		WorkDir:  workDir,
	}), "WriteTree")

	mount, ok := registry.Get(sgDev)
	require.True(t, ok, "WriteTree must park mount in registry")

	// Write a sentinel file through the live FUSE mount so we can verify after
	// the retry cycle that the tree was not re-copied.
	const sentinelName = "sentinel.txt"

	const sentinelContent = "session-model integration test"

	require.NoError(t,
		os.WriteFile(filepath.Join(mount.Mountpoint(), sentinelName), []byte(sentinelContent), 0o644),
		"write sentinel file to LTFS mountpoint",
	)

	// --- FinalizeTape attempt 1: cancelled context (AC: mount stays in registry)
	cancelledCtx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately so ctx.Err() fires at the top of FinalizeTape

	_, err = acts.FinalizeTape(cancelledCtx, FinalizeInput{Device: sgDev})
	require.Error(t, err, "FinalizeTape with cancelled context must fail")

	_, stillInRegistry := registry.Get(sgDev)
	require.True(t, stillInRegistry, "mount must remain in registry after failed FinalizeTape")

	// --- FinalizeTape attempt 2: valid context (AC: uses still-live mount) ----
	index, err := acts.FinalizeTape(t.Context(), FinalizeInput{Device: sgDev})
	require.NoError(t, err, "FinalizeTape retry must succeed using the live registry mount")

	_, stillInRegistryAfterSuccess := registry.Get(sgDev)
	assert.False(t, stillInRegistryAfterSuccess, "mount must be removed from registry after successful FinalizeTape")

	// --- Index validation (AC: index captured, valid LTFS XML) ----------------
	assert.NotEmpty(t, index, "captured LTFS index must not be empty")
	assert.Contains(t, string(index), "<ltfsindex", "captured index must be LTFS XML")
	assert.Contains(t, string(index), "<name>"+string(barcode)+"</name>",
		"index must name the volume by barcode")

	// --- Re-mount (AC: tree not re-copied; sentinel survives the retry cycle) -
	remountDir := t.TempDir()
	remountWorkDir := t.TempDir()

	require.NoError(t, acts.WriteTree(t.Context(), WriteTreeInput{
		Device:   sgDev,
		MountDir: remountDir,
		WorkDir:  remountWorkDir,
	}), "re-mount WriteTree must succeed")

	remountMount, remountOK := registry.Get(sgDev)
	require.True(t, remountOK, "re-mount WriteTree must park mount in registry")

	got, readErr := os.ReadFile(filepath.Join(remountMount.Mountpoint(), sentinelName))
	assert.NoError(t, readErr, "sentinel file must be readable after re-mount")
	assert.Equal(t, sentinelContent, string(got), "sentinel content must survive the retry cycle")

	// Clean up the re-mount.
	_, finalErr := acts.FinalizeTape(t.Context(), FinalizeInput{Device: sgDev})
	assert.NoError(t, finalErr, "final FinalizeTape on re-mount must succeed")
}
