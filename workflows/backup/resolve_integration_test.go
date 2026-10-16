//go:build integration

package backup

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/zfs"
)

// TestResolveNewestSnapshotIntegration (#418): snapshot: newest resolves a real
// dataset to its newest prefix-matching snapshot, which is then held, staged
// from, and named in the report.
func TestResolveNewestSnapshotIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)

	dataset := testutil.CreateEphemeralDataset(t, "tape-archiver-newest-test")

	for _, name := range []string{"daily-1", "weekly-1", "daily-2", "weekly-2"} {
		out, err := exec.CommandContext(t.Context(), "zfs", "snapshot", dataset+"@"+name).CombinedOutput()
		require.NoError(t, err, "zfs snapshot %s: %s", name, out)
	}

	want := dataset + "@daily-2"
	cfg := config.Config{
		Sources: []config.Source{{ZFSPath: &config.ZFSPathSource{
			Name: dataset, Snapshot: config.SnapshotNewest, Prefix: "daily-",
		}}},
		Library:    config.Library{TapeCapacityBytes: 2_500_000_000_000},
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10)},
	}

	resolved, err := newResolveDataActivities("").ResolveAndCheck(t.Context(), ResolveDataInput{Config: cfg})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	assert.Equal(t, []ResolvedSnapshot{{ZFSPath: want}}, resolved[0].Snapshots)
	assert.Equal(t, []string{want}, sourceSnapshots(resolved[0]), "the report names the chosen snapshot")

	paths := holdSnapshotPaths(&runState{resolved: resolved})
	require.Equal(t, []string{want}, paths)

	input := HoldInput{Tag: HoldTag("newest-integration"), Snapshots: paths}

	t.Cleanup(func() { _ = newHoldActivities().ReleaseSnapshots(context.Background(), input) })
	require.NoError(t, newHoldActivities().HoldSnapshots(t.Context(), input))

	holds, err := zfs.Holds(t.Context(), want)
	require.NoError(t, err)
	assert.Contains(t, holds, input.Tag, "the chosen snapshot is held")

	dir, err := zfsLocator{}.SnapshotDir(t.Context(), resolved[0].Snapshots[0])
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(dir, filepath.Join(".zfs", "snapshot", "daily-2")),
		"Prepare stages from the chosen snapshot, got %s", dir)

	cfg.Sources[0].ZFSPath.Prefix = "monthly-"
	_, err = newResolveDataActivities("").ResolveAndCheck(t.Context(), ResolveDataInput{Config: cfg})
	require.ErrorContains(t, err, "sources[0]", "no match fails Resolve naming the source")
}
