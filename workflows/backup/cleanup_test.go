package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// stageRunTree populates a staging root with what a run leaves behind: two
// archives' worth of slices and PAR2 recovery files under the run directory, the
// durable recovery artifacts beside them, and the barcode-keyed mount, work, and
// index entries at the root. It returns the run directory.
//
// It mirrors the real layout exactly — prepare.go stages archives under NNN/ with
// slices named archive.NNN, par2.go writes archive*.par2 beside them, report.go
// writes report.pdf / recovery.iso / disc-manifest.sha256 into the run directory,
// and session.go creates mounts/<barcode>/, work/<barcode>/, and
// indexes/<barcode>.xml at the root.
func stageRunTree(t *testing.T, stagingRoot, runID string, barcodes ...string) string {
	t.Helper()

	runDir := filepath.Join(stagingRoot, runID)

	files := []string{
		filepath.Join(runDir, "000", "archive.000"),
		filepath.Join(runDir, "000", "archive.001"),
		filepath.Join(runDir, "000", "archive.par2"),
		filepath.Join(runDir, "000", "archive.vol000+01.par2"),
		filepath.Join(runDir, "001", "archive.000"),
		filepath.Join(runDir, "001", "archive.par2"),
		filepath.Join(runDir, reportFileName),
		filepath.Join(runDir, isoFileName),
		filepath.Join(runDir, discManifestFileName),
	}

	for _, barcode := range barcodes {
		files = append(files,
			filepath.Join(stagingRoot, "mounts", barcode, ".keep"),
			filepath.Join(stagingRoot, "work", barcode, "index.schema"),
			filepath.Join(stagingRoot, "indexes", barcode+".xml"),
		)
	}

	for _, path := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("staged"), 0o644))
	}

	return runDir
}

// assertNoStagedBulk asserts no archive slice or PAR2 file survives anywhere
// beneath runDir — the criterion that matters, stated over the whole tree rather
// than over the specific paths the fixture happened to create.
func assertNoStagedBulk(t *testing.T, runDir string) {
	t.Helper()

	var leftovers []string

	err := filepath.WalkDir(runDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		if !slices.Contains(retainedRunArtifacts, entry.Name()) {
			leftovers = append(leftovers, path)
		}

		return nil
	})
	require.NoError(t, err)

	assert.Empty(t, leftovers, "no staged bulk data may survive cleanup")
}

// TestCleanupRemovesBulkAndRetainsRecoveryArtifacts covers the success path: the
// staged slices and PAR2 files are gone while the durable recovery artifacts
// survive. The burning-disabled case exercises a run that never staged an ISO or
// disc manifest, proving cleanup does not depend on their presence.
func TestCleanupRemovesBulkAndRetainsRecoveryArtifacts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		opticalBurn  bool
		wantRetained []string
	}{
		{
			name:         "optical burning enabled",
			opticalBurn:  true,
			wantRetained: []string{reportFileName, isoFileName, discManifestFileName},
		},
		{
			name:         "optical burning disabled",
			opticalBurn:  false,
			wantRetained: []string{reportFileName},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			stagingRoot := t.TempDir()
			runDir := stageRunTree(t, stagingRoot, "run-id")

			if !test.opticalBurn {
				// A run without optical burning stages no ISO or disc manifest.
				require.NoError(t, os.Remove(filepath.Join(runDir, isoFileName)))
				require.NoError(t, os.Remove(filepath.Join(runDir, discManifestFileName)))
			}

			activities := newCleanupActivities(stagingRoot)
			require.NoError(t, activities.cleanup(t.Context(), runDir, nil))

			assertNoStagedBulk(t, runDir)

			entries, err := os.ReadDir(runDir)
			require.NoError(t, err)

			var survived []string
			for _, entry := range entries {
				survived = append(survived, entry.Name())
			}

			assert.ElementsMatch(t, test.wantRetained, survived,
				"exactly the durable recovery artifacts survive")
		})
	}
}

// TestCleanupRemovesBarcodeKeyedEntries asserts the per-tape LTFS mountpoint,
// work directory, and staged index file the run created are removed. They live at
// the staging root rather than under the run directory, so the run-directory sweep
// cannot reach them and they would otherwise accumulate per barcode forever.
func TestCleanupRemovesBarcodeKeyedEntries(t *testing.T) {
	t.Parallel()

	stagingRoot := t.TempDir()
	runDir := stageRunTree(t, stagingRoot, "run-id", "TAPE01L6", "TAPE02L6")

	// A barcode from an unrelated prior run that this run never loaded.
	other := filepath.Join(stagingRoot, "indexes", "OTHER99L6.xml")
	require.NoError(t, os.WriteFile(other, []byte("staged"), 0o644))

	activities := newCleanupActivities(stagingRoot)
	require.NoError(t, activities.cleanup(t.Context(), runDir, []string{"TAPE01L6", "TAPE02L6"}))

	for _, barcode := range []string{"TAPE01L6", "TAPE02L6"} {
		for _, path := range barcodeStagingPaths(stagingRoot, barcode) {
			assert.NoFileExists(t, path)
			assert.NoDirExists(t, path)
		}
	}

	assert.FileExists(t, other, "cleanup touches only the barcodes this run loaded")
}

// TestCleanupToleratesNothingStaged covers the run that fails before Prepare
// stages anything (e.g. Resolve fails): the run directory never existed, and
// cleanup must succeed rather than add a spurious error to a run that already has
// one.
func TestCleanupToleratesNothingStaged(t *testing.T) {
	t.Parallel()

	stagingRoot := t.TempDir()

	activities := newCleanupActivities(stagingRoot)

	require.NoError(t, activities.cleanup(t.Context(),
		filepath.Join(stagingRoot, "run-that-staged-nothing"), []string{"TAPE01L6"}))
}

// TestCleanupIsIdempotent asserts a second cleanup of the same run — an activity
// retry after a partial failure — succeeds rather than failing on the paths the
// first pass already removed.
func TestCleanupIsIdempotent(t *testing.T) {
	t.Parallel()

	stagingRoot := t.TempDir()
	runDir := stageRunTree(t, stagingRoot, "run-id", "TAPE01L6")

	activities := newCleanupActivities(stagingRoot)

	require.NoError(t, activities.cleanup(t.Context(), runDir, []string{"TAPE01L6"}))
	require.NoError(t, activities.cleanup(t.Context(), runDir, []string{"TAPE01L6"}))

	assertNoStagedBulk(t, runDir)
}

// TestCleanupStagingRequiresStagingRoot verifies the activity refuses an
// unconfigured staging root rather than deriving a path relative to the worker's
// working directory and recursively removing it.
func TestCleanupStagingRequiresStagingRoot(t *testing.T) {
	t.Parallel()

	activities := newCleanupActivities("")

	err := activities.CleanupStaging(t.Context(), CleanupInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "staging directory")

	// Misconfiguration recurs on every retry, and cleanup is deferred — a
	// retrying activity would hold the run open. It must be non-retryable.
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.True(t, appErr.NonRetryable(), "unconfigured staging root must be non-retryable")
}

// TestCleanupReportsEveryRemovalFailure asserts a path that cannot be removed is
// reported rather than silently skipped, and that one stubborn entry does not stop
// cleanup from reclaiming the rest.
func TestCleanupReportsEveryRemovalFailure(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permissions this test relies on")
	}

	stagingRoot := t.TempDir()
	runDir := stageRunTree(t, stagingRoot, "run-id")

	// Make one archive directory unremovable by clearing write permission on it:
	// its entries cannot be unlinked, so RemoveAll fails for that subtree only.
	locked := filepath.Join(runDir, "000")
	require.NoError(t, os.Chmod(locked, 0o555))

	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	activities := newCleanupActivities(stagingRoot)

	err := activities.cleanup(t.Context(), runDir, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), locked, "the unremovable path is named")

	// The other archive was still reclaimed.
	assert.NoDirExists(t, filepath.Join(runDir, "001"))
}

// TestCleanupBarcodes covers the pure barcode-selection logic: it reads the
// cumulative mounted list (not the in-flight drive-set), de-duplicates a barcode
// seen twice, and skips the empty barcode of a tape that never mounted.
func TestCleanupBarcodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state *runState
		want  []string
	}{
		{
			name:  "no tapes loaded",
			state: &runState{},
			want:  nil,
		},
		{
			name:  "skips an empty barcode",
			state: &runState{mountedBarcodes: []tape.Barcode{""}},
			want:  nil,
		},
		{
			name: "accumulates across drive-sets and dedupes",
			state: &runState{mountedBarcodes: []tape.Barcode{
				"TAPE01L6", "TAPE02L6", "TAPE01L6", "TAPE03L6",
			}},
			want: []string{"TAPE01L6", "TAPE02L6", "TAPE03L6"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := cleanupBarcodes(test.state)
			assert.Equal(t, test.want, got)
		})
	}
}

// recordingCleaner is a CleanupStaging stand-in that records the inputs it was
// called with, so a workflow test can assert the run dispatched cleanup on a given
// exit path. cleanupErr, when set, makes the activity fail — modelling a cleanup
// that cannot reclaim the staging directory.
type recordingCleaner struct {
	mu         sync.Mutex
	calls      []CleanupInput
	cleanupErr error
}

func (r *recordingCleaner) CleanupStaging(_ context.Context, input CleanupInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls = append(r.calls, input)

	return r.cleanupErr
}

func (r *recordingCleaner) recorded() []CleanupInput {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]CleanupInput(nil), r.calls...)
}

// cleanupTestParam is the serializable input to cleanupTestWorkflow.
type cleanupTestParam struct {
	Barcodes []tape.Barcode
	// Mode selects the exit path: "success" returns cleanly, "fail" returns an
	// error, "cancel" blocks until the test cancels the run.
	Mode string
}

// cleanupTestWorkflow exercises the workflow cleanup helper exactly as Backup
// does: it arms the cleanup before any phase runs, then exits via the requested
// path.
func cleanupTestWorkflow(ctx workflow.Context, param cleanupTestParam) error {
	state := &runState{mountedBarcodes: param.Barcodes}

	defer cleanupStaging(ctx, state)

	switch param.Mode {
	case "fail":
		return errors.New("phase Prepare: boom")
	case "cancel":
		// Block until the test cancels the run; on cancellation the deferred
		// cleanup still fires because it runs on a disconnected context.
		_ = workflow.Sleep(ctx, time.Hour)
	}

	return nil
}

func newCleanupEnv(cleaner *recordingCleaner) *testsuite.TestWorkflowEnvironment {
	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(cleanupTestWorkflow)
	// Registered under the real activity's name so the workflow's
	// ExecuteActivity(activities.CleanupStaging, ...) reference resolves to it.
	env.RegisterActivity(&CleanupActivities{})
	env.OnActivity((&CleanupActivities{}).CleanupStaging, mock.Anything, mock.Anything).
		Return(cleaner.CleanupStaging)

	return env
}

// TestCleanupRunsOnEveryExitPath covers the success and failure exit paths: the
// staged tree is reclaimed whether the run succeeds or fails.
func TestCleanupRunsOnEveryExitPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mode      string
		wantError require.ErrorAssertionFunc
	}{
		{name: "success", mode: "success", wantError: require.NoError},
		{name: "failure", mode: "fail", wantError: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cleaner := &recordingCleaner{}
			env := newCleanupEnv(cleaner)

			env.ExecuteWorkflow(cleanupTestWorkflow, cleanupTestParam{
				Barcodes: []tape.Barcode{"TAPE01L6"},
				Mode:     test.mode,
			})

			require.True(t, env.IsWorkflowCompleted())
			test.wantError(t, env.GetWorkflowError())

			calls := cleaner.recorded()
			require.Len(t, calls, 1, "the run cleaned up its staging directory")
			assert.Equal(t, []string{"TAPE01L6"}, calls[0].Barcodes)
		})
	}
}

// TestCleanupRunsOnCancellation asserts the run reclaims its staging directory
// even when cancelled — proving cleanupStaging runs on a disconnected context,
// mirroring TestHoldReleaseOnCancellation. A connected-context activity would
// never be dispatched once the workflow itself is cancelled.
func TestCleanupRunsOnCancellation(t *testing.T) {
	t.Parallel()

	cleaner := &recordingCleaner{}
	env := newCleanupEnv(cleaner)

	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, time.Millisecond)

	env.ExecuteWorkflow(cleanupTestWorkflow, cleanupTestParam{
		Barcodes: []tape.Barcode{"TAPE01L6"},
		Mode:     "cancel",
	})

	require.True(t, env.IsWorkflowCompleted())

	calls := cleaner.recorded()
	require.Len(t, calls, 1, "the run cleaned up its staging directory after being cancelled")
	assert.Equal(t, []string{"TAPE01L6"}, calls[0].Barcodes)
}

// TestCleanupFailureDoesNotMaskRunOutcome asserts a cleanup that cannot reclaim
// the staging directory neither fails a successful run nor replaces a failing
// run's own error. Leftover staging is observable on disk and manually clearable;
// the run's outcome is what the operator acts on.
func TestCleanupFailureDoesNotMaskRunOutcome(t *testing.T) {
	t.Parallel()

	t.Run("successful run still succeeds", func(t *testing.T) {
		t.Parallel()

		cleaner := &recordingCleaner{cleanupErr: errors.New("read-only file system")}
		env := newCleanupEnv(cleaner)

		env.ExecuteWorkflow(cleanupTestWorkflow, cleanupTestParam{Mode: "success"})

		require.True(t, env.IsWorkflowCompleted())
		require.NoError(t, env.GetWorkflowError(), "a cleanup failure must not fail a successful run")
	})

	t.Run("failing run keeps its own error", func(t *testing.T) {
		t.Parallel()

		cleaner := &recordingCleaner{cleanupErr: errors.New("read-only file system")}
		env := newCleanupEnv(cleaner)

		env.ExecuteWorkflow(cleanupTestWorkflow, cleanupTestParam{Mode: "fail"})

		require.True(t, env.IsWorkflowCompleted())

		err := env.GetWorkflowError()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "boom", "the run's own error surfaces, not the cleanup's")
		assert.NotContains(t, err.Error(), "read-only file system")
	})
}
