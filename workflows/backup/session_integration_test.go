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

// TestSessionSplitWriteWithFinalizeRetry drives Format → WriteTree → a
// simulated post-unmount FinalizeTape failure → FinalizeTape retry against
// mhvtl, and checks the retry re-reads a valid index without re-copying the
// tree (#62, #152 AC3).
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
	stagingDir := t.TempDir()
	registry := newMountRegistry()
	acts := newWriteActivities(registry, stagingDir)

	require.NoError(t, acts.FormatTape(t.Context(), FormatInput{
		Device:  sgDev,
		Barcode: barcode,
	}), "FormatTape")

	// Empty archive list: WriteTree only mounts and writes an empty manifest.
	require.NoError(t, acts.WriteTree(t.Context(), WriteTreeInput{
		Device:  sgDev,
		Barcode: barcode,
	}), "WriteTree")

	mount, ok := registry.Get(sgDev)
	require.True(t, ok, "WriteTree must park mount in registry")

	// Proves after the retry that the tree was not re-copied.
	const sentinelName = "sentinel.txt"

	const sentinelContent = "session-model integration test"

	require.NoError(t,
		os.WriteFile(filepath.Join(mount.Mountpoint(), sentinelName), []byte(sentinelContent), 0o644),
		"write sentinel file to LTFS mountpoint",
	)

	// Simulate a prior FinalizeTape that unmounted but failed reading the index
	// back: the entry stays, marked unmounted.
	require.NoError(t, mount.Unmount(t.Context()), "simulate the prior attempt's successful unmount")
	registry.MarkUnmounted(sgDev)

	_, stillInRegistry := registry.Get(sgDev)
	require.True(t, stillInRegistry, "the entry must remain after a post-unmount failure so the retry is not mount-lost")

	indexPath, err := acts.FinalizeTape(t.Context(), FinalizeInput{Device: sgDev, Barcode: barcode})
	require.NoError(t, err, "FinalizeTape retry after a prior unmount must re-read the index, not fail mount-lost")

	_, stillInRegistryAfterSuccess := registry.Get(sgDev)
	assert.False(t, stillInRegistryAfterSuccess, "mount must be removed from registry after successful FinalizeTape")

	index, err := os.ReadFile(indexPath)
	require.NoError(t, err, "staged LTFS index must be readable from the path FinalizeTape returned")
	assert.NotEmpty(t, index, "captured LTFS index must not be empty")
	assert.Contains(t, string(index), "<ltfsindex", "captured index must be LTFS XML")
	assert.Contains(t, string(index), "<name>"+string(barcode)+"</name>",
		"index must name the volume by barcode")

	// Re-mount to confirm the sentinel survived the retry.
	require.NoError(t, acts.WriteTree(t.Context(), WriteTreeInput{
		Device:  sgDev,
		Barcode: barcode,
	}), "re-mount WriteTree must succeed")

	remountMount, remountOK := registry.Get(sgDev)
	require.True(t, remountOK, "re-mount WriteTree must park mount in registry")

	got, readErr := os.ReadFile(filepath.Join(remountMount.Mountpoint(), sentinelName))
	assert.NoError(t, readErr, "sentinel file must be readable after re-mount")
	assert.Equal(t, sentinelContent, string(got), "sentinel content must survive the retry cycle")

	_, finalErr := acts.FinalizeTape(t.Context(), FinalizeInput{Device: sgDev, Barcode: barcode})
	assert.NoError(t, finalErr, "final FinalizeTape on re-mount must succeed")
}
