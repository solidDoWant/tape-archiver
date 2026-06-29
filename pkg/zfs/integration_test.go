//go:build integration

package zfs_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/zfs"
)

// TestSnapshotDirIntegration verifies SnapshotDir against a real ZFS pool. The
// ephemeral pool from "make zpool-up" provides a snapshot (TAPE_TEST_SNAPSHOT)
// whose directory must resolve and contain the staged payload; an absent
// snapshot must error. When no snapshot is configured, the positive path falls
// back to discovering one under .zfs/snapshot/ and is skipped if none exist.
func TestSnapshotDirIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)

	mount := testutil.PoolMount(t)

	// An absent snapshot must error regardless of pool state.
	_, err := zfs.SnapshotDir(mount, "tape-archiver-nonexistent-snapshot")
	require.Error(t, err, "absent snapshot should return an error")

	snapshot := testutil.TestSnapshot(t)
	if snapshot == "" {
		entries, err := os.ReadDir(filepath.Join(mount, ".zfs", "snapshot"))
		if err != nil || len(entries) == 0 {
			t.Skipf("no snapshot configured (%s) and none found under %s/.zfs/snapshot",
				testutil.EnvTestSnapshot, mount)
		}

		snapshot = entries[0].Name()
	}

	dir, err := zfs.SnapshotDir(mount, snapshot)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(mount, ".zfs", "snapshot", snapshot), dir)
	assert.True(t, filepath.IsAbs(dir), "returned path should be absolute")

	info, err := os.Stat(dir)
	require.NoError(t, err, "resolved snapshot directory should be stat-able")
	assert.True(t, info.IsDir())
}

// TestMountpointIntegration verifies Mountpoint reads a dataset's filesystem
// mountpoint via "zfs get" and that the resolved path is the directory whose
// .zfs/snapshot tree the Prepare phase reads. A non-existent dataset must error.
func TestMountpointIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)

	mount, err := zfs.Mountpoint(t.Context(), testutil.PoolDataset(t))
	require.NoError(t, err)
	assert.Equal(t, testutil.PoolMount(t), mount,
		"mountpoint should match the dataset's filesystem mount")

	info, err := os.Stat(mount)
	require.NoError(t, err, "resolved mountpoint should be stat-able")
	assert.True(t, info.IsDir())

	_, err = zfs.Mountpoint(t.Context(), testutil.PoolDataset(t)+"/tape-archiver-nonexistent")
	require.Error(t, err, "a non-existent dataset should error")
}

// TestLogicalReferencedIntegration verifies LogicalReferenced returns the
// dataset's logicalreferenced byte count via "zfs get". When the harness
// reports the staged payload size (TAPE_TEST_MIN_BYTES), the value must be at
// least that; otherwise it must merely be non-negative.
func TestLogicalReferencedIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)

	dataset := testutil.PoolDataset(t)
	if snapshot := testutil.TestSnapshot(t); snapshot != "" {
		dataset += "@" + snapshot
	}

	bytes, err := zfs.LogicalReferenced(t.Context(), dataset)
	require.NoError(t, err)

	if minBytes := testutil.TestMinBytes(t); minBytes > 0 {
		assert.GreaterOrEqual(t, bytes, minBytes,
			"logicalreferenced should cover the staged payload")
	} else {
		assert.GreaterOrEqual(t, bytes, int64(0))
	}
}

// TestUserPropertiesIntegration verifies UserProperties reads a dataset's ZFS
// user properties via "zfs get" and that a non-existent dataset errors (the
// existence check the resolve pipeline depends on). The ephemeral test pool need
// not carry any user properties, so the positive case asserts only a non-nil map
// and that every returned key is colon-namespaced.
func TestUserPropertiesIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)

	dataset := testutil.PoolDataset(t)
	if snapshot := testutil.TestSnapshot(t); snapshot != "" {
		dataset += "@" + snapshot
	}

	properties, err := zfs.UserProperties(t.Context(), dataset)
	require.NoError(t, err)
	require.NotNil(t, properties)

	for name := range properties {
		assert.Contains(t, name, ":", "user property names are colon-namespaced")
	}

	_, err = zfs.UserProperties(t.Context(), testutil.PoolDataset(t)+"@tape-archiver-nonexistent-snapshot")
	require.Error(t, err, "a non-existent snapshot should error")
}
