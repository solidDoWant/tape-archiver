//go:build integration

package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
)

// TestZFSLocatorUnmountedDataset exercises the production zfsLocator against a
// real ZFS pool for a raw dataset source (no "@"). A mounted dataset resolves to
// its live mountpoint (AC2); an unmounted dataset — whose mountpoint can survive
// as a shadow directory — fails with an error naming the dataset (AC1, issue
// #135), so the run aborts in Prepare before any tape is written.
func TestZFSLocatorUnmountedDataset(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)

	dataset := testutil.CreateEphemeralDataset(t, "tape-archiver-locator-test")

	locator := zfsLocator{}
	snapshot := ResolvedSnapshot{ZFSPath: dataset}

	// AC2: a mounted raw dataset resolves to its live mountpoint.
	dir, err := locator.SnapshotDir(t.Context(), snapshot)
	require.NoError(t, err)
	assert.NotEmpty(t, dir, "a mounted dataset should resolve to its mountpoint")

	// AC1: an unmounted raw dataset fails, and the error names the dataset.
	testutil.UnmountDataset(t, dataset)

	_, err = locator.SnapshotDir(t.Context(), snapshot)
	require.Error(t, err, "an unmounted dataset must not resolve to its shadow mountpoint")
	assert.Contains(t, err.Error(), dataset, "the error should identify the unmounted dataset")
}
