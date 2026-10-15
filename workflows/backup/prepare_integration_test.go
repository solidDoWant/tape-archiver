//go:build integration

package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
)

// TestZFSLocatorUnmountedDataset: a mounted raw dataset resolves to its
// mountpoint (AC2); an unmounted one fails naming the dataset (AC1, #135).
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

	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.True(t, appErr.NonRetryable(), "an unmounted dataset must fail Prepare without retrying (#385)")
}
