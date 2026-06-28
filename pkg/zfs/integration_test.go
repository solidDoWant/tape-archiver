//go:build integration

package zfs_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/zfs"
)

// TestSnapshotDirIntegration verifies SnapshotDir against the real pool:
// resolving an existing snapshot returns its .zfs/snapshot path, and an absent
// snapshot returns an error. The positive case is skipped when the pool has no
// snapshots; the negative case always runs.
func TestSnapshotDirIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)

	mount := testutil.PoolMount(t)

	// A snapshot name that does not exist must error regardless of pool state.
	_, err := zfs.SnapshotDir(mount, "tape-archiver-nonexistent-snapshot")
	require.Error(t, err, "absent snapshot should return an error")

	entries, err := os.ReadDir(filepath.Join(mount, ".zfs", "snapshot"))
	if err != nil || len(entries) == 0 {
		t.Skipf("no snapshots under %s/.zfs/snapshot to exercise the positive path", mount)
	}

	name := entries[0].Name()

	dir, err := zfs.SnapshotDir(mount, name)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(mount, ".zfs", "snapshot", name), dir)
	assert.True(t, filepath.IsAbs(dir))
}

// TestLogicalReferencedIntegration verifies LogicalReferenced returns a
// non-negative byte count for the pool's root dataset via "zfs get".
func TestLogicalReferencedIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)

	if _, err := exec.LookPath("zfs"); err != nil {
		t.Skip("zfs binary not available on PATH")
	}

	dataset := testutil.PoolDataset(t)

	bytes, err := zfs.LogicalReferenced(t.Context(), dataset)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, bytes, int64(0))
}
