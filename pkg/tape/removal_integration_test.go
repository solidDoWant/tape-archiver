//go:build integration

package tape_test

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// TestAllowMediumRemovalReleasesLock sets the lock a killed ltfs leaves behind
// and checks AllowMediumRemoval clears it so the tape unloads (#442). mhvtl
// (as of 1.8.0) accepts PREVENT but never enforces it, so here this proves the
// command path and idempotency; on real hardware it also proves the release.
func TestAllowMediumRemovalReleasesLock(t *testing.T) {
	testutil.SkipIfMhvtlUnavailable(t)

	if _, err := exec.LookPath("sg_raw"); err != nil {
		t.Skipf("sg_raw (sg3-utils) not on PATH: %v", err)
	}

	sgDev := testutil.Drive0SgDev(t)
	if _, err := os.Stat(sgDev); err != nil {
		t.Skipf("sg device %s not available: %v (set %s to override)", sgDev, err, testutil.EnvDrive0SgDev)
	}

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1)
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")

	require.NoError(t, tape.AllowMediumRemoval(t.Context(), sgDev),
		"ALLOW on an empty, unlocked drive must be harmless")

	slot := inv.Slots[0]
	driveAddr := inv.Drives[0].Address

	require.NoError(t, changer.Load(t.Context(), slot.Address, driveAddr), "load")

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()

		_ = tape.AllowMediumRemoval(ctx, sgDev)
		_ = changer.Unload(ctx, slot.Address, driveAddr)
	})

	testutil.SkipIfDriveNotReady(t, testutil.Drive0Dev(t))

	// PREVENT MEDIUM REMOVAL (PREVENT=01b), as ltfs issues at mount.
	out, err := exec.CommandContext(t.Context(), "sg_raw", sgDev, "1e", "00", "00", "00", "01", "00").CombinedOutput()
	require.NoError(t, err, "PREVENT MEDIUM REMOVAL: %s", out)

	require.NoError(t, tape.AllowMediumRemoval(t.Context(), sgDev), "ALLOW after PREVENT")
	require.NoError(t, tape.AllowMediumRemoval(t.Context(), sgDev), "a repeated ALLOW must be harmless")

	require.NoError(t, changer.Unload(t.Context(), slot.Address, driveAddr), "unload after ALLOW")

	inv, err = changer.Inventory(t.Context())
	require.NoError(t, err, "inventory after unload")
	assert.False(t, inv.Drives[0].Loaded, "drive must be empty after the unload")
	assert.True(t, inv.Slots[0].Full, "tape must be back in its slot")
}
