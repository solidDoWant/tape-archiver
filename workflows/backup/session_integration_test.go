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
//  4. The prior FinalizeTape attempt's post-unmount failure is simulated:
//     the volume is unmounted (writing the LTFS index to tape) and the registry
//     entry is marked unmounted but left in place — exactly the state a retry
//     inherits when a prior attempt unmounted successfully but failed reading
//     the index back (issue #152 AC3).
//  5. FinalizeTape is called again: it finds the entry still present and already
//     unmounted, so it does not re-diagnose a lost mount and does not re-drive
//     the tape — it re-reads the captured index and succeeds.
//  6. The captured index is validated as LTFS XML.
//  7. The volume is re-mounted and the sentinel file is confirmed present,
//     proving the tree was not re-copied on the retry.
//
// Covers ACs 1–5 of issue #62 (session-pinned split write, Finalize retry,
// cleanup, non-session phases unchanged, mhvtl integration) and issue #152 AC3
// (the idempotent post-unmount retry replaces the removed ctx.Err() guard).
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

	// --- FormatTape (AC: tape formatted, LTFS volume name = barcode) ----------
	require.NoError(t, acts.FormatTape(t.Context(), FormatInput{
		Device:  sgDev,
		Barcode: barcode,
	}), "FormatTape")

	// --- WriteTree (AC: mount live; mount parked in registry) -----------------
	// Pass an empty archive list so WriteTree only mounts and writes an empty
	// manifest; the sentinel file is written directly through the live mount.
	require.NoError(t, acts.WriteTree(t.Context(), WriteTreeInput{
		Device:  sgDev,
		Barcode: barcode,
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

	// --- Simulate a prior FinalizeTape attempt that unmounted successfully but
	// failed reading the index back (issue #152 AC3). Unmount the volume (which
	// writes the LTFS index to tape) and mark the registry entry unmounted while
	// leaving it in place, exactly as FinalizeTape does on a ReadIndex failure.
	require.NoError(t, mount.Unmount(t.Context()), "simulate the prior attempt's successful unmount")
	registry.MarkUnmounted(sgDev)

	_, stillInRegistry := registry.Get(sgDev)
	require.True(t, stillInRegistry, "the entry must remain after a post-unmount failure so the retry is not mount-lost")

	// --- FinalizeTape retry: the entry is present and already unmounted, so it
	// must skip re-unmounting a detached volume, re-read the captured index, and
	// succeed — never re-driving the finalized tape or reporting a lost mount.
	index, err := acts.FinalizeTape(t.Context(), FinalizeInput{Device: sgDev})
	require.NoError(t, err, "FinalizeTape retry after a prior unmount must re-read the index, not fail mount-lost")

	_, stillInRegistryAfterSuccess := registry.Get(sgDev)
	assert.False(t, stillInRegistryAfterSuccess, "mount must be removed from registry after successful FinalizeTape")

	// --- Index validation (AC: index captured, valid LTFS XML) ----------------
	assert.NotEmpty(t, index, "captured LTFS index must not be empty")
	assert.Contains(t, string(index), "<ltfsindex", "captured index must be LTFS XML")
	assert.Contains(t, string(index), "<name>"+string(barcode)+"</name>",
		"index must name the volume by barcode")

	// --- Re-mount (AC: tree not re-copied; sentinel survives the retry cycle) -
	// WriteTree re-uses the same mount and work dirs under stagingDir. The prior
	// work dir contents (captured index) are left in place; LTFS may add new
	// generation-suffixed index files on the second unmount.
	require.NoError(t, acts.WriteTree(t.Context(), WriteTreeInput{
		Device:  sgDev,
		Barcode: barcode,
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
