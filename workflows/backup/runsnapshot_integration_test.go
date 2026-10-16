//go:build integration

package backup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/zfs"
)

func requireZFSCommand(t *testing.T, args ...string) {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "zfs", args...).CombinedOutput()
	require.NoError(t, err, "zfs %v: %s", args, out)
}

// TestRunScopedSnapshotIntegration (#419): a live dataset is staged from a
// snapshot this run took and held, and the snapshot is gone once the run ends,
// whether it succeeded or failed.
func TestRunScopedSnapshotIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)

	tests := []struct {
		name       string
		prepareErr error
	}{
		{name: "success"},
		{name: "failure", prepareErr: errors.New("prepare boom")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataset := testutil.CreateEphemeralDataset(t, "tape-archiver-run-scoped-test")

			mount, err := zfs.Mountpoint(t.Context(), dataset)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(mount, "before.txt"), []byte("before"), 0o600))

			snapshot := dataset + "@" + RunSnapshotName(testRunID)

			env := newBackupEnvWith(t, newResolveDataActivities(""))
			expectReportDeliverSuccess(env)

			staged := false

			env.OnActivity((&PrepareActivities{}).PrepareArchives, mock.Anything, mock.Anything).
				Return(func(ctx context.Context, input PrepareInput) ([]StagedArchive, error) {
					staged = true

					if !assert.Len(t, input.Archives, 1) || !assert.Len(t, input.Archives[0].Snapshots, 1) {
						return nil, errors.New("unexpected work list")
					}

					source := input.Archives[0].Snapshots[0]
					assert.Equal(t, snapshot, source.ZFSPath)
					assert.False(t, source.CreatedAt.IsZero(), "the creation time is recorded for the report")

					owner, err := zfs.UserProperty(ctx, snapshot, RunIDProperty)
					assert.NoError(t, err)
					assert.Equal(t, testRunID, owner, "created with this run's id")

					holds, err := zfs.Holds(ctx, snapshot)
					assert.NoError(t, err)
					assert.Contains(t, holds, HoldTag(testRunID), "held like any source snapshot")

					// Written after the snapshot, so it must not be staged.
					assert.NoError(t, os.WriteFile(filepath.Join(mount, "after.txt"), []byte("after"), 0o600))

					dir, err := zfsLocator{}.SnapshotDir(ctx, source)
					assert.NoError(t, err)
					assert.FileExists(t, filepath.Join(dir, "before.txt"))
					assert.NoFileExists(t, filepath.Join(dir, "after.txt"), "staged from the point-in-time snapshot")

					return nil, test.prepareErr
				})

			cfg := validBackupConfig()
			cfg.Sources = []config.Source{{ZFSPath: &config.ZFSPathSource{Name: dataset}}}

			env.ExecuteWorkflow(Backup, cfg)
			require.True(t, env.IsWorkflowCompleted())

			if test.prepareErr != nil {
				require.ErrorContains(t, env.GetWorkflowError(), "prepare boom")
			} else {
				require.NoError(t, env.GetWorkflowError())
			}

			assert.True(t, staged)

			_, err = zfs.Creation(t.Context(), snapshot)
			require.ErrorContains(t, err, zfs.ErrNotExist.Error(), "the run-scoped snapshot is destroyed when the run ends")
		})
	}
}

// TestRunSnapshotLeakCleanupIntegration (#419): a run removes only snapshots
// carrying another run's id under that run's snapshot name.
func TestRunSnapshotLeakCleanupIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)

	dataset := testutil.CreateEphemeralDataset(t, "tape-archiver-leak-test")
	inheriting := testutil.CreateEphemeralDataset(t, "tape-archiver-leak-inherited")

	leaked := dataset + "@" + RunSnapshotName("old-run")
	requireZFSCommand(t, "snapshot", "-o", RunIDProperty+"=old-run", leaked)
	requireZFSCommand(t, "hold", HoldTag("old-run"), leaked)

	current := dataset + "@" + RunSnapshotName("current-run")
	requireZFSCommand(t, "snapshot", "-o", RunIDProperty+"=current-run", current)

	noProperty := dataset + "@" + RunSnapshotName("no-property")
	requireZFSCommand(t, "snapshot", noProperty)

	foreignHold := dataset + "@" + RunSnapshotName("held-run")
	requireZFSCommand(t, "snapshot", "-o", RunIDProperty+"=held-run", foreignHold)
	requireZFSCommand(t, "hold", "someone-else", foreignHold)
	t.Cleanup(func() { _ = exec.Command("zfs", "release", "someone-else", foreignHold).Run() })

	requireZFSCommand(t, "set", RunIDProperty+"=old-run", inheriting)

	inherited := inheriting + "@" + RunSnapshotName("old-run")
	requireZFSCommand(t, "snapshot", inherited)

	_, err := newRunSnapshotActivities().CreateRunSnapshots(t.Context(), RunSnapshotInput{RunID: "current-run"})
	require.NoError(t, err)

	_, err = zfs.Creation(t.Context(), leaked)
	require.ErrorContains(t, err, zfs.ErrNotExist.Error(), "a leaked run snapshot is destroyed, its hold released first")

	for _, kept := range []string{current, noProperty, foreignHold, inherited} {
		_, err := zfs.Creation(t.Context(), kept)
		assert.NoError(t, err, "%s must survive", kept)
	}

	holds, err := zfs.Holds(t.Context(), foreignHold)
	require.NoError(t, err)
	assert.Equal(t, []string{"someone-else"}, holds, "another tool's hold is never released")
}

// TestPropertySourceIntegration (#419): only datasets with the property set
// locally are selected, never the staging dataset or anything under it.
func TestPropertySourceIntegration(t *testing.T) {
	testutil.SkipIfPoolUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)

	const property = "tape-archiver-test:archive"

	parent := testutil.CreateEphemeralDataset(t, "tape-archiver-property-test")
	for _, child := range []string{"a", "a/child", "b", "staging", "staging/kid"} {
		requireZFSCommand(t, "create", parent+"/"+child)
	}

	requireZFSCommand(t, "set", property+"=true", parent+"/a")
	requireZFSCommand(t, "set", property+"=false", parent+"/b")
	requireZFSCommand(t, "set", property+"=true", parent+"/staging")
	requireZFSCommand(t, "set", property+"=true", parent+"/staging/kid")

	stagingMount, err := zfs.Mountpoint(t.Context(), parent+"/staging")
	require.NoError(t, err)

	activities := newResolveDataActivities(filepath.Join(stagingMount, ".tape-staging"))

	cfg := config.Config{Sources: []config.Source{{ZFSProperty: &config.ZFSPropertySource{Name: property, Value: "true", Under: parent}}}}

	matches, err := activities.ResolvePropertySources(t.Context(), cfg)
	require.NoError(t, err)
	assert.Equal(t, []PropertyMatches{{SourceIndex: 0, Datasets: []string{parent + "/a"}}}, matches,
		"a/child inherits the value and the staging datasets are excluded")

	cfg.Sources[0].ZFSProperty.Value = "nope"
	_, err = activities.ResolvePropertySources(t.Context(), cfg)
	require.ErrorContains(t, err, "sources[0] zfsProperty "+property, "no match fails Resolve naming the source")
}
