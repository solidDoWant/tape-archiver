package backup

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// TestStagingPhasesBoundRetries (#385): a phase that keeps failing fails the run
// after a bounded number of attempts and fires the alert; a transient failure is
// retried and the run completes.
func TestStagingPhasesBoundRetries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		phase        string
		failures     int
		wantAttempts int
		assertErr    require.ErrorAssertionFunc
	}{
		{name: "resolve keeps failing", phase: PhaseResolve, failures: math.MaxInt, wantAttempts: resolveMaxAttempts, assertErr: require.Error},
		{name: "prepare keeps failing", phase: PhasePrepare, failures: math.MaxInt, wantAttempts: prepareMaxAttempts, assertErr: require.Error},
		{name: "generate PAR2 keeps failing", phase: PhaseGeneratePAR2, failures: math.MaxInt, wantAttempts: generatePAR2MaxAttempts, assertErr: require.Error},
		{name: "prepare recovers from a worker restart", phase: PhasePrepare, failures: 1, wantAttempts: 2, assertErr: require.NoError},
		{name: "generate PAR2 recovers from a worker restart", phase: PhaseGeneratePAR2, failures: 1, wantAttempts: 2, assertErr: require.NoError},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			env := newBackupEnv(t)
			expectReportDeliverSuccess(env)

			attempts := 0
			fail := func() error {
				attempts++
				if attempts <= test.failures {
					return errors.New("worker restarted")
				}

				return nil
			}

			env.OnActivity((&ResolveControlActivities{}).ResolveK8sSources, mock.Anything, mock.Anything).
				Return([]ResolvedArchive(nil), nil)

			if test.phase == PhaseResolve {
				env.OnActivity((&ResolveDataActivities{}).ResolveAndCheck, mock.Anything, mock.Anything).
					Return(func(context.Context, ResolveDataInput) ([]ResolvedArchive, error) { return nil, fail() })
			} else {
				env.OnActivity((&ResolveDataActivities{}).ResolveAndCheck, mock.Anything, mock.Anything).
					Return([]ResolvedArchive(nil), nil)
			}

			if test.phase == PhasePrepare {
				env.OnActivity((&PrepareActivities{}).PrepareArchives, mock.Anything, mock.Anything).
					Return(func(context.Context, PrepareInput) ([]StagedArchive, error) { return nil, fail() })
			}

			if test.phase == PhaseGeneratePAR2 {
				env.OnActivity((&GeneratePAR2Activities{}).GeneratePAR2, mock.Anything, mock.Anything).
					Return(func(context.Context, GeneratePAR2Input) ([]PAR2Set, error) { return nil, fail() })
			}

			var alerted FailureInput

			env.OnActivity((&FailureActivities{}).NotifyFailure, mock.Anything, mock.Anything).
				Return(func(_ context.Context, input FailureInput) error {
					alerted = input

					return nil
				}).Maybe()

			env.ExecuteWorkflow(Backup, validBackupConfig())

			require.True(t, env.IsWorkflowCompleted())
			test.assertErr(t, env.GetWorkflowError())
			assert.Equal(t, test.wantAttempts, attempts)

			if test.failures == math.MaxInt {
				assert.Equal(t, test.phase, alerted.Phase, "the failure alert must fire for the failing phase")
			}
		})
	}
}

// TestResolveAndCheckInfeasibleIsNonRetryable (#385): a source too large for one
// tape is deterministic, so Resolve fails without retrying.
func TestResolveAndCheckInfeasibleIsNonRetryable(t *testing.T) {
	t.Parallel()

	activities := &ResolveDataActivities{pool: fakePool{
		properties: map[string]map[string]string{"pool/raw@snap": {}},
		sizes:      map[string]int64{"pool/raw@snap": 1_000_000},
	}}

	input := ResolveDataInput{Config: config.Config{
		Sources: []config.Source{zfsSource("pool/raw@snap")},
		Library: config.Library{TapeCapacityBytes: 1000},
	}}

	_, err := activities.ResolveAndCheck(t.Context(), input)

	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.True(t, appErr.NonRetryable(), "an infeasible source must not be retried")
	assert.Contains(t, appErr.Error(), "exceeds one tape's usable capacity")
}

// notMountedLocator stages "pool/ok@s" from dir and rejects "pool/unmounted" as
// the zfs locator does an unmounted raw dataset, counting lookups per path.
type notMountedLocator struct {
	dir   string
	mu    sync.Mutex
	calls map[string]int
}

func (l *notMountedLocator) SnapshotDir(_ context.Context, snapshot ResolvedSnapshot) (string, error) {
	l.mu.Lock()
	l.calls[snapshot.ZFSPath]++
	l.mu.Unlock()

	if snapshot.ZFSPath == "pool/unmounted" {
		return "", temporal.NewNonRetryableApplicationError("raw ZFS dataset is not mounted", datasetNotMountedErrorType, nil)
	}

	return l.dir, nil
}

func preparePhaseWorkflow(ctx workflow.Context, cfg config.Config, archives []ResolvedArchive) error {
	return preparePhase(ctx, cfg, &runState{resolved: archives, sliceSizeBytes: 1 << 20})
}

// TestPrepareUnmountedSourceDoesNotRestage (#385): an unmounted raw dataset fails
// Prepare once, so the archives staged before it are not re-staged.
func TestPrepareUnmountedSourceDoesNotRestage(t *testing.T) {
	t.Parallel()

	_, recipient := generateKeypair(t)

	locator := &notMountedLocator{dir: writeTree(t, map[string]string{"file.txt": "data"}), calls: map[string]int{}}

	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(preparePhaseWorkflow)
	env.RegisterActivity(&PrepareActivities{stagingRoot: t.TempDir(), locator: locator})

	cfg := config.Config{Encryption: config.Encryption{Recipients: []string{recipient}}}
	archives := []ResolvedArchive{
		{SourceIndex: 0, Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/ok@s"}}},
		{SourceIndex: 1, Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/unmounted"}}},
	}

	env.ExecuteWorkflow(preparePhaseWorkflow, cfg, archives)

	require.True(t, env.IsWorkflowCompleted())
	require.ErrorContains(t, env.GetWorkflowError(), "not mounted")
	assert.Equal(t, 1, locator.calls["pool/ok@s"], "the staged archive must not be re-staged")
}
