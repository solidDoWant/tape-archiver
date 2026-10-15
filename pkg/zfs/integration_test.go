//go:build integration

package zfs_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/k8ssnap"
	"github.com/solidDoWant/tape-archiver/pkg/zfs"
)

// TestSnapshotDirIntegration checks SnapshotDir against the make zpool-up pool;
// an absent snapshot must error.
func TestSnapshotDirIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)

	mount := testutil.PoolMount(t)

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

// TestMountpointIntegration checks Mountpoint resolves the dataset's mountpoint.
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

// TestMountedIntegration checks Mounted tracks the real mount state across
// "zfs unmount" (issue #135).
func TestMountedIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)

	mounted, err := zfs.Mounted(t.Context(), testutil.PoolDataset(t))
	require.NoError(t, err)
	assert.True(t, mounted, "the mounted test dataset should report mounted")

	_, err = zfs.Mounted(t.Context(), testutil.PoolDataset(t)+"/tape-archiver-nonexistent")
	require.Error(t, err, "a non-existent dataset should error")

	dataset := testutil.CreateEphemeralDataset(t, "tape-archiver-mounted-test")

	mounted, err = zfs.Mounted(t.Context(), dataset)
	require.NoError(t, err)
	assert.True(t, mounted, "a freshly created dataset should be mounted")

	testutil.UnmountDataset(t, dataset)

	mounted, err = zfs.Mounted(t.Context(), dataset)
	require.NoError(t, err)
	assert.False(t, mounted, "an unmounted dataset should report not mounted")
}

// TestLogicalReferencedIntegration checks the byte count is at least
// TAPE_TEST_MIN_BYTES when set.
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

// TestUserPropertiesIntegration checks only colon-namespaced keys are returned
// and a missing dataset errors (the existence check).
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

// poolRoot returns the pool name of the configured test dataset.
func poolRoot(t *testing.T) string {
	t.Helper()

	root, _, _ := strings.Cut(testutil.PoolDataset(t), "/")
	require.NotEmpty(t, root, "pool dataset should have a pool component")

	return root
}

// createDataset creates a dataset by exact name, destroying any leftover first
// and on cleanup. Skips unless root.
func createDataset(t *testing.T, name string) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("creating ZFS datasets requires root — run via 'make test-integration' (sudo)")
	}

	destroy := func() {
		_ = exec.Command("zfs", "destroy", "-r", name).Run()
	}

	destroy()

	require.NoError(t, exec.Command("zfs", "create", name).Run(),
		"create dataset %q", name)

	t.Cleanup(destroy)
}

// zfsGetRaw reads a property via the zfs CLI as the oracle for the parser.
func zfsGetRaw(t *testing.T, property, dataset string) string {
	t.Helper()

	out, err := exec.Command("zfs", "get", "-Hp", "-o", "value", property, dataset).Output()
	require.NoError(t, err, "zfs get %s %q", property, dataset)

	return strings.TrimSpace(string(out))
}

// TestLogicalReferencedSpacedNameIntegration checks a dataset name with a
// numeric third whitespace token still yields the true byte count.
func TestLogicalReferencedSpacedNameIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)

	dataset := poolRoot(t) + "/media disc 2"
	createDataset(t, dataset)

	want := zfsGetRaw(t, "logicalreferenced", dataset)

	got, err := zfs.LogicalReferenced(t.Context(), dataset)
	require.NoError(t, err, "spaced dataset name should parse, not error")

	assert.Equal(t, want, strconv.FormatInt(got, 10),
		"logicalreferenced should be the true byte count, not a token from the name")
	assert.NotEqual(t, int64(2), got,
		"the numeric name token '2' must not be mistaken for the value")
}

type spoofReader struct{}

func (spoofReader) UserProperty(ctx context.Context, dataset, property string) (string, error) {
	return zfs.UserProperty(ctx, dataset, property)
}

// TestUserPropertySpoofIntegration checks a newline-embedded fake
// managed_resource record cannot fool UserProperty or k8ssnap.Verify.
func TestUserPropertySpoofIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)

	dataset := poolRoot(t) + "/spoof target"
	createDataset(t, dataset)

	spoof := "harmless\ndemocratic-csi:managed_resource\ttrue"
	require.NoError(t,
		exec.Command("zfs", "set", "custom:note="+spoof, dataset).Run(),
		"set crafted user property")

	managed, err := zfs.UserProperty(t.Context(), dataset, "democratic-csi:managed_resource")
	require.NoError(t, err)
	assert.NotEqual(t, "true", managed,
		"a continuation line must not fabricate managed_resource=true")
	assert.Equal(t, "-", managed, "the property is genuinely unset")

	// Snapshot so the guard reads an @-snapshot path, as in production.
	require.NoError(t, exec.Command("zfs", "snapshot", dataset+"@snap").Run())

	snapshot := k8ssnap.Snapshot{Dataset: dataset, SnapshotName: "snap"}
	require.NoError(t,
		exec.Command("zfs", "set", "custom:note="+spoof, dataset+"@snap").Run(),
		"carry the crafted property onto the snapshot")

	err = k8ssnap.Verify(t.Context(), spoofReader{}, snapshot)
	require.Error(t, err, "the spoofed snapshot must be rejected as unmanaged")
}

// TestHoldReleaseIntegration checks a held snapshot cannot be destroyed until
// released, and that hold/release are idempotent. Uses its own snapshot.
func TestHoldReleaseIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)

	ctx := t.Context()
	dataset := testutil.PoolDataset(t)
	snapshot := dataset + "@tape-archiver-hold-test"
	tag := "tape-archiver-hold-test-run"

	_ = runZFS(t, ctx, "destroy", "-d", snapshot) // best-effort pre-clean (ignored if absent)
	requireZFS(t, ctx, "snapshot", snapshot)

	t.Cleanup(func() {
		// t.Context() is already cancelled during cleanup.
		cleanupCtx := context.Background()
		_ = zfs.Release(cleanupCtx, tag, snapshot)
		_ = runZFS(t, cleanupCtx, "destroy", snapshot)
	})

	holds, err := zfs.Holds(ctx, snapshot)
	require.NoError(t, err)
	assert.Empty(t, holds, "a fresh snapshot has no holds")

	require.NoError(t, zfs.Hold(ctx, tag, snapshot))

	holds, err = zfs.Holds(ctx, snapshot)
	require.NoError(t, err)
	assert.Contains(t, holds, tag, "the snapshot is held under the run tag")

	require.NoError(t, zfs.Hold(ctx, tag, snapshot), "re-holding the same tag is a no-op")

	// AC1: while held, an external `zfs destroy` of the snapshot is refused.
	require.Error(t, runZFS(t, ctx, "destroy", snapshot), "destroying a held snapshot must fail")

	holds, err = zfs.Holds(ctx, snapshot)
	require.NoError(t, err)
	assert.Contains(t, holds, tag, "the refused destroy left the snapshot held")

	require.NoError(t, zfs.Release(ctx, tag, snapshot))
	require.NoError(t, zfs.Release(ctx, tag, snapshot), "releasing an absent tag is a no-op")

	holds, err = zfs.Holds(ctx, snapshot)
	require.NoError(t, err)
	assert.NotContains(t, holds, tag, "the hold is gone after release")

	require.NoError(t, runZFS(t, ctx, "destroy", snapshot), "an unheld snapshot can be destroyed")

	// #387: a pruned snapshot fails the hold as ErrNotExist; releasing it is a no-op.
	require.ErrorIs(t, zfs.Hold(ctx, tag, snapshot), zfs.ErrNotExist)
	require.NoError(t, zfs.Release(ctx, tag, snapshot), "a destroyed snapshot holds nothing to release")
}

func requireZFS(t *testing.T, ctx context.Context, args ...string) {
	t.Helper()

	require.NoError(t, runZFS(t, ctx, args...), "zfs %v", args)
}

// runZFS runs a zfs subcommand, returning its error with stderr attached.
func runZFS(t *testing.T, ctx context.Context, args ...string) error {
	t.Helper()

	cmd := exec.CommandContext(ctx, "zfs", args...)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("zfs %v: %v: %s", args, err, out)

		return err
	}

	return nil
}
