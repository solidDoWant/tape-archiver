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

// poolRoot returns the top-level pool name from the configured test dataset
// (e.g. "tape_test" from "tape_test/archive"), so helper datasets can be created
// as pool children with names the test fully controls.
func poolRoot(t *testing.T) string {
	t.Helper()

	root, _, _ := strings.Cut(testutil.PoolDataset(t), "/")
	require.NotEmpty(t, root, "pool dataset should have a pool component")

	return root
}

// createDataset creates a ZFS dataset by exact name (which may contain spaces)
// and registers its recursive destruction for test cleanup. Any pre-existing
// dataset of the same name is destroyed first so a prior failed run cannot leak
// state. Requires root (the integration suite runs under sudo), so the test is
// skipped when not privileged.
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

// zfsGetRaw reads a property's raw value directly via the zfs CLI, as the truth
// oracle the parser under test is compared against.
func zfsGetRaw(t *testing.T, property, dataset string) string {
	t.Helper()

	out, err := exec.Command("zfs", "get", "-Hp", "-o", "value", property, dataset).Output()
	require.NoError(t, err, "zfs get %s %q", property, dataset)

	return strings.TrimSpace(string(out))
}

// TestLogicalReferencedSpacedNameIntegration proves AC1/AC2 against a real pool:
// a dataset whose name contains a space — and whose third whitespace token is the
// numeric "2", the worst case for whitespace splitting — yields the true
// logicalreferenced byte count, not a number parsed out of the name.
func TestLogicalReferencedSpacedNameIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)

	// "media disc 2": legal in OpenZFS, contains spaces, and its third
	// whitespace-separated token is the numeric "2" that a Fields-based parser
	// would wrongly return.
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

// spoofReader adapts zfs.UserProperty to k8ssnap.PropertyReader so the ownership
// guard can be exercised end-to-end against the real pool.
type spoofReader struct{}

func (spoofReader) UserProperty(ctx context.Context, dataset, property string) (string, error) {
	return zfs.UserProperty(ctx, dataset, property)
}

// TestUserPropertySpoofIntegration proves AC3 against a real pool: a user property
// whose value embeds a newline followed by "democratic-csi:managed_resource<tab>true"
// cannot fabricate that property. The by-name UserProperty read returns the unset
// marker "-" (not "true"), and k8ssnap.Verify rejects the snapshot.
func TestUserPropertySpoofIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)

	dataset := poolRoot(t) + "/spoof target"
	createDataset(t, dataset)

	// A crafted value that, in "zfs get all" newline-delimited output, looks
	// exactly like a real democratic-csi:managed_resource=true record.
	spoof := "harmless\ndemocratic-csi:managed_resource\ttrue"
	require.NoError(t,
		exec.Command("zfs", "set", "custom:note="+spoof, dataset).Run(),
		"set crafted user property")

	managed, err := zfs.UserProperty(t.Context(), dataset, "democratic-csi:managed_resource")
	require.NoError(t, err)
	assert.NotEqual(t, "true", managed,
		"a continuation line must not fabricate managed_resource=true")
	assert.Equal(t, "-", managed, "the property is genuinely unset")

	// End-to-end: the ownership guard must reject a dataset it does not own,
	// even in the presence of the spoof value. Snapshot the dataset so the guard
	// reads an @-snapshot path, mirroring production.
	require.NoError(t, exec.Command("zfs", "snapshot", dataset+"@snap").Run())

	snapshot := k8ssnap.Snapshot{Dataset: dataset, SnapshotName: "snap"}
	require.NoError(t,
		exec.Command("zfs", "set", "custom:note="+spoof, dataset+"@snap").Run(),
		"carry the crafted property onto the snapshot")

	err = k8ssnap.Verify(t.Context(), spoofReader{}, snapshot)
	require.Error(t, err, "the spoofed snapshot must be rejected as unmanaged")
}
