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

// stageRunTree builds the real staging layout a run leaves behind (archive
// slices and PAR2 under NNN/, retained artifacts, and barcode-keyed entries at
// the root) and returns the run directory.
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

// assertNoStagedBulk asserts no slice or PAR2 file survives anywhere under runDir.
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

// TestCleanupRemovesBulkAndRetainsRecoveryArtifacts asserts bulk data is removed
// and recovery artifacts survive, including when none were staged.
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

// TestCleanupRemovesBarcodeKeyedEntries asserts the root-level per-tape entries,
// which the run-directory sweep cannot reach, are removed.
func TestCleanupRemovesBarcodeKeyedEntries(t *testing.T) {
	t.Parallel()

	stagingRoot := t.TempDir()
	runDir := stageRunTree(t, stagingRoot, "run-id", "TAPE01L6", "TAPE02L6")

	// From an unrelated prior run; must survive.
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

// TestCleanupToleratesNothingStaged: a run that never staged anything cleans up
// without error.
func TestCleanupToleratesNothingStaged(t *testing.T) {
	t.Parallel()

	stagingRoot := t.TempDir()

	activities := newCleanupActivities(stagingRoot)

	require.NoError(t, activities.cleanup(t.Context(),
		filepath.Join(stagingRoot, "run-that-staged-nothing"), []string{"TAPE01L6"}))
}

// TestCleanupIsIdempotent: a retried cleanup succeeds.
func TestCleanupIsIdempotent(t *testing.T) {
	t.Parallel()

	stagingRoot := t.TempDir()
	runDir := stageRunTree(t, stagingRoot, "run-id", "TAPE01L6")

	activities := newCleanupActivities(stagingRoot)

	require.NoError(t, activities.cleanup(t.Context(), runDir, []string{"TAPE01L6"}))
	require.NoError(t, activities.cleanup(t.Context(), runDir, []string{"TAPE01L6"}))

	assertNoStagedBulk(t, runDir)
}

// TestCleanupStagingRequiresStagingRoot asserts an empty staging root is refused
// rather than resolving relative to the working directory.
func TestCleanupStagingRequiresStagingRoot(t *testing.T) {
	t.Parallel()

	activities := newCleanupActivities("")

	err := activities.CleanupStaging(t.Context(), CleanupInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "staging directory")

	// Deferred cleanup must not retry a misconfiguration forever.
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.True(t, appErr.NonRetryable(), "unconfigured staging root must be non-retryable")
}

// TestCleanupReportsEveryRemovalFailure asserts an unremovable path is reported
// without stopping the rest of the cleanup.
func TestCleanupReportsEveryRemovalFailure(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permissions this test relies on")
	}

	stagingRoot := t.TempDir()
	runDir := stageRunTree(t, stagingRoot, "run-id")

	locked := filepath.Join(runDir, "000")
	require.NoError(t, os.Chmod(locked, 0o555))

	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	activities := newCleanupActivities(stagingRoot)

	err := activities.cleanup(t.Context(), runDir, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), locked, "the unremovable path is named")

	assert.NoDirExists(t, filepath.Join(runDir, "001"))
}

// TestCleanupBarcodes asserts the cumulative mounted list is used, de-duplicated,
// skipping empty barcodes.
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

// recordingCleaner records CleanupStaging inputs; cleanupErr makes it fail.
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

type cleanupTestParam struct {
	Barcodes []tape.Barcode
	// Mode is "success", "fail", or "cancel" (blocks until the test cancels).
	Mode string
}

// cleanupTestWorkflow mirrors Backup: arm cleanup first, then exit via Mode.
func cleanupTestWorkflow(ctx workflow.Context, param cleanupTestParam) error {
	state := &runState{mountedBarcodes: param.Barcodes}

	defer cleanupStaging(ctx, state)

	switch param.Mode {
	case "fail":
		return errors.New("phase Prepare: boom")
	case "cancel":
		// The deferred cleanup still fires: it runs on a disconnected context.
		_ = workflow.Sleep(ctx, time.Hour)
	}

	return nil
}

func newCleanupEnv(cleaner *recordingCleaner) *testsuite.TestWorkflowEnvironment {
	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(cleanupTestWorkflow)
	// Registered under the real activity's name.
	env.RegisterActivity(&CleanupActivities{})
	env.OnActivity((&CleanupActivities{}).CleanupStaging, mock.Anything, mock.Anything).
		Return(cleaner.CleanupStaging)

	return env
}

// TestCleanupRunsOnEveryExitPath: staging is reclaimed on success and failure.
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

// TestCleanupRunsOnCancellation: staging is reclaimed on cancellation, proving
// cleanup runs on a disconnected context.
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

// TestCleanupFailureDoesNotMaskRunOutcome: a failed cleanup neither fails a
// successful run nor replaces a failing run's error.
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
