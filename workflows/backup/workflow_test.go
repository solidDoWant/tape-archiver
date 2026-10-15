package backup

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// orderedPhases is the pipeline order SPEC §4.3 prescribes.
var orderedPhases = []string{
	PhaseResolve,
	PhasePrepare,
	PhasePack,
	PhaseGeneratePAR2,
	PhaseVerify,
	PhaseLoad,
	PhaseWrite,
	PhaseEject,
	PhaseReport,
	PhaseBurn,
	PhaseDeliver,
}

// newBackupEnv returns a test environment with Backup and every phase activity
// registered.
func newBackupEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()

	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(Backup)

	env.RegisterActivity(&ResolveControlActivities{})
	env.RegisterActivity(&ResolveDataActivities{})
	env.RegisterActivity(newHoldActivities())
	// A real staging root, since Prepare rejects an empty one.
	env.RegisterActivity(newPrepareActivities(t.TempDir()))
	env.RegisterActivity(newPackActivities())
	env.RegisterActivity(newGeneratePAR2Activities())
	env.RegisterActivity(newVerifyActivities())
	env.RegisterActivity(newLoadActivities())
	env.RegisterActivity(newEjectActivities())
	// Write creates a Temporal session, which needs the session worker.
	env.SetWorkerOptions(worker.Options{EnableSessionWorker: true})

	registry := newMountRegistry()
	env.RegisterActivity(newWriteActivities(registry, t.TempDir()))
	env.RegisterActivity(newTeardownActivities(registry))
	// Report and Deliver cannot no-op on an empty config; tests mock them via
	// expectReportDeliverSuccess.
	env.RegisterActivity(newReportActivities(t.TempDir(), t.TempDir(), t.TempDir()))
	env.RegisterActivity(newDeliverActivities())
	env.RegisterActivity(&FailureActivities{})
	env.RegisterActivity(newCleanupActivities(t.TempDir()))

	return env
}

// expectReportDeliverSuccess mocks Report and Deliver to succeed; these tests
// assert sequencing, not artifact content.
func expectReportDeliverSuccess(env *testsuite.TestWorkflowEnvironment) {
	env.OnActivity((&ReportActivities{}).BuildReport, mock.Anything, mock.Anything).
		Return(ReportOutput{}, nil)
	env.OnActivity((&DeliverActivities{}).Deliver, mock.Anything, mock.Anything).
		Return(&DeliverResult{}, nil)
}

// validBackupConfig returns a config that passes config.Validate, mirroring
// internal/config's validConfig.
func validBackupConfig() config.Config {
	targetPercentage := 10.0

	return config.Config{
		Sources: []config.Source{
			{ZFSPath: &config.ZFSPathSource{Name: "bulk-pool-01/archive@snap-20240101"}},
		},
		Copies: 2,
		Library: config.Library{
			Changer:           "/dev/sch0",
			Drives:            []string{"/dev/nst0", "/dev/nst1"},
			BlankSlots:        []int{1, 2},
			TapeCapacityBytes: 2_500_000_000_000,
		},
		Redundancy: config.Redundancy{
			TargetPercentage: &targetPercentage,
		},
		Encryption: config.Encryption{
			Recipients: []string{"age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"},
			Identity:   "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREALIDENTITY000000000000000000000000000000000",
		},
		Delivery: config.Delivery{
			WebhookURL: "https://discord.com/api/webhooks/123/abc",
		},
	}
}

// expectResolveEmpty mocks Resolve to an empty work list, so the data phases and
// tape path no-op without touching zfs, tar, or the library.
func expectResolveEmpty(env *testsuite.TestWorkflowEnvironment) {
	env.OnActivity((&ResolveControlActivities{}).ResolveK8sSources, mock.Anything, mock.Anything).
		Return([]ResolvedArchive(nil), nil)
	env.OnActivity((&ResolveDataActivities{}).ResolveAndCheck, mock.Anything, mock.Anything).
		Return([]ResolvedArchive(nil), nil)
}

// expectAllPhasesSucceed mocks Resolve empty and Report/Deliver successful, so a
// valid config runs every phase to completion.
func expectAllPhasesSucceed(env *testsuite.TestWorkflowEnvironment) {
	expectResolveEmpty(env)
	expectReportDeliverSuccess(env)
}

func TestBackupRunsAllPhasesInOrder(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)
	expectAllPhasesSucceed(env)

	env.ExecuteWorkflow(Backup, validBackupConfig())

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result Result
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, orderedPhases, result.CompletedPhases)
}

// TestReportIsStampedWithTheRunID pins issue #340: every run shares the
// singleton workflow id (SPEC §4.2), so the report must carry the run id or every
// printout would be indistinguishable.
func TestReportIsStampedWithTheRunID(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)
	// The real singleton id, not the test environment's default.
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: WorkflowID, TaskQueue: TaskQueue})
	expectResolveEmpty(env)

	var captured ReportInput

	env.OnActivity((&ReportActivities{}).BuildReport, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input ReportInput) (ReportOutput, error) {
			captured = input

			return ReportOutput{}, nil
		})
	env.OnActivity((&DeliverActivities{}).Deliver, mock.Anything, mock.Anything).
		Return(&DeliverResult{}, nil)

	env.ExecuteWorkflow(Backup, validBackupConfig())

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.NotEmpty(t, captured.RunID, "the report must be stamped with the run's identity")
	assert.NotEqual(t, WorkflowID, captured.RunID,
		"the report must carry the per-run run id, not the workflow id every run shares")
}

func TestLastCompletedPhaseQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// failAt is the failing phase; empty runs to completion.
		failAt string
		want   string
	}{
		{name: "before any phase completes", failAt: PhaseResolve, want: ""},
		{name: "mid-run after several phases", failAt: PhaseGeneratePAR2, want: PhasePack},
		{name: "after the run completes", failAt: "", want: PhaseDeliver},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			env := newBackupEnv(t)

			switch test.failAt {
			case "":
				expectAllPhasesSucceed(env)
			case PhaseResolve:
				// Resolve's own failure; an empty-resolve baseline would double-mock it.
				failPhase(t, env, test.failAt)
			default:
				expectResolveEmpty(env)
				failPhase(t, env, test.failAt)
			}

			env.ExecuteWorkflow(Backup, validBackupConfig())

			require.True(t, env.IsWorkflowCompleted())

			value, err := env.QueryWorkflow(LastCompletedPhaseQuery)
			require.NoError(t, err)

			var got string
			require.NoError(t, value.Get(&got))
			require.Equal(t, test.want, got)
		})
	}
}

// TestCurrentPauseQueryDefaultsToNone asserts a run that never pauses reports
// PauseNone. Active pauses are covered in the per-site pause tests.
func TestCurrentPauseQueryDefaultsToNone(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)
	expectAllPhasesSucceed(env)

	env.ExecuteWorkflow(Backup, validBackupConfig())

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	value, err := env.QueryWorkflow(CurrentPauseQuery)
	require.NoError(t, err)

	var pause CurrentPause
	require.NoError(t, value.Get(&pause))
	assert.Equal(t, PauseNone, pause.Kind)
	assert.Equal(t, CurrentPause{}, pause, "no pause ever fired, so every field stays the zero value")
}

// failIfPrepareRuns fails the test if Prepare, the first phase touching bulk
// data, ever runs.
func failIfPrepareRuns(t *testing.T, env *testsuite.TestWorkflowEnvironment) {
	t.Helper()

	env.OnActivity((&PrepareActivities{}).PrepareArchives, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ PrepareInput) ([]StagedArchive, error) {
			t.Error("Prepare ran despite an invalid config")

			return nil, nil
		})
}

// TestBackupRejectsInvalidConfig: an invalid config submitted directly to
// Temporal fails validation before any staging.
func TestBackupRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)
	failIfPrepareRuns(t, env)

	invalid := validBackupConfig()
	invalid.Copies = 0

	env.ExecuteWorkflow(Backup, invalid)

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err)
	assert.ErrorContains(t, err, "invalid config")
	assert.ErrorContains(t, err, "copies")
}

// TestBackupRejectsZeroSources: zero sources fails validation rather than
// succeeding vacuously.
func TestBackupRejectsZeroSources(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)
	failIfPrepareRuns(t, env)

	invalid := validBackupConfig()
	invalid.Sources = nil

	env.ExecuteWorkflow(Backup, invalid)

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err)
	assert.ErrorContains(t, err, "invalid config")
	assert.ErrorContains(t, err, "sources")
}

// TestBackupReleasesHoldsWhenHoldFails asserts Backup's defer ordering: a failed
// HoldSnapshots still dispatches ReleaseSnapshots over every resolved snapshot.
func TestBackupReleasesHoldsWhenHoldFails(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)

	archives := []ResolvedArchive{{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/a@snap1"}}}}

	env.OnActivity((&ResolveControlActivities{}).ResolveK8sSources, mock.Anything, mock.Anything).
		Return([]ResolvedArchive(nil), nil)
	env.OnActivity((&ResolveDataActivities{}).ResolveAndCheck, mock.Anything, mock.Anything).
		Return(archives, nil)

	env.OnActivity((&HoldActivities{}).HoldSnapshots, mock.Anything, mock.Anything).
		Return(errors.New("hold boom"))

	var releaseInput HoldInput

	env.OnActivity((&HoldActivities{}).ReleaseSnapshots, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input HoldInput) error {
			releaseInput = input

			return nil
		})

	env.ExecuteWorkflow(Backup, validBackupConfig())

	require.True(t, env.IsWorkflowCompleted())

	require.ErrorContains(t, env.GetWorkflowError(), "phase Hold")

	assert.Contains(t, releaseInput.Tag, holdTagPrefix)
	assert.ElementsMatch(t, []string{"pool/a@snap1"}, releaseInput.Snapshots)
}

// failPhase mocks the named phase's activity to fail. Report and Deliver are not
// handled; tests target phases up to Eject.
func failPhase(t *testing.T, env *testsuite.TestWorkflowEnvironment, name string) {
	t.Helper()

	switch name {
	case PhaseResolve:
		env.OnActivity((&ResolveControlActivities{}).ResolveK8sSources, mock.Anything, mock.Anything).
			Return(nil, errors.New("boom"))

		return
	case PhasePrepare:
		env.OnActivity((&PrepareActivities{}).PrepareArchives, mock.Anything, mock.Anything).
			Return(nil, errors.New("boom"))

		return
	case PhasePack:
		env.OnActivity((&PackActivities{}).Pack, mock.Anything, mock.Anything).
			Return(TapePlan{}, errors.New("boom"))

		return
	case PhaseGeneratePAR2:
		env.OnActivity((&GeneratePAR2Activities{}).GeneratePAR2, mock.Anything, mock.Anything).
			Return(nil, errors.New("boom"))

		return
	case PhaseVerify:
		env.OnActivity((&VerifyActivities{}).Verify, mock.Anything, mock.Anything).
			Return(VerifiedPlan{}, errors.New("boom"))

		return
	case PhaseLoad:
		env.OnActivity((&LoadActivities{}).Load, mock.Anything, mock.Anything).
			Return(nil, errors.New("boom"))

		return
	case PhaseWrite:
		env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).
			Return(errors.New("boom"))

		return
	case PhaseEject:
		env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).
			Return(errors.New("boom"))

		return
	}

	t.Fatalf("failPhase does not support phase %q (Report and Deliver are mocked to succeed)", name)
}

// TestBackupHoldFailureAlertsDespiteFailingRelease (#387): a hold and release
// that keep failing are bounded, so the run closes and the alert fires.
func TestBackupHoldFailureAlertsDespiteFailingRelease(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)

	archives := []ResolvedArchive{{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/a@snap1"}}}}

	env.OnActivity((&ResolveControlActivities{}).ResolveK8sSources, mock.Anything, mock.Anything).
		Return([]ResolvedArchive(nil), nil)
	env.OnActivity((&ResolveDataActivities{}).ResolveAndCheck, mock.Anything, mock.Anything).
		Return(archives, nil)

	holds, releases := 0, 0

	env.OnActivity((&HoldActivities{}).HoldSnapshots, mock.Anything, mock.Anything).
		Return(func(context.Context, HoldInput) error {
			holds++

			return errors.New("hold boom")
		})
	env.OnActivity((&HoldActivities{}).ReleaseSnapshots, mock.Anything, mock.Anything).
		Return(func(context.Context, HoldInput) error {
			releases++

			return errors.New("release boom")
		})

	var alerted FailureInput

	env.OnActivity((&FailureActivities{}).NotifyFailure, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input FailureInput) error {
			alerted = input

			return nil
		})

	env.ExecuteWorkflow(Backup, validBackupConfig())

	require.True(t, env.IsWorkflowCompleted())
	require.ErrorContains(t, env.GetWorkflowError(), "phase Hold")
	assert.Equal(t, holdMaxAttempts, holds)
	assert.Equal(t, releaseMaxAttempts, releases)
	assert.Equal(t, PhaseHold, alerted.Phase)
	assert.Contains(t, alerted.ErrorSummary, "hold boom")
	assert.Contains(t, alerted.ErrorSummary, "release source-snapshot hold tape-archiver-hold-",
		"the alert names the leaked hold")
}
