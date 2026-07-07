package backup

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// orderedPhases is the backup pipeline in the order SPEC §4.3 prescribes. The
// tests assert the workflow drives exactly this sequence.
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

// newBackupEnv returns a test workflow environment with the Backup workflow and
// every phase activity plus the failure-alert activity registered, so a test only
// overrides the behavior it cares about with OnActivity.
func newBackupEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()

	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(Backup)

	// Every phase is run-orchestrated, so nothing is registered from the phase
	// table's activity field (all nil); each phase's activities are registered
	// explicitly below.

	env.RegisterActivity(&ResolveControlActivities{})
	env.RegisterActivity(&ResolveDataActivities{})
	// The Prepare phase is run-orchestrated like Resolve; register its activity
	// with a real staging root so a run with no sources stages nothing and the
	// activity does not reject an empty staging directory.
	env.RegisterActivity(newPrepareActivities(t.TempDir()))
	// The Pack and Generate PAR2 phases are run-orchestrated too; with no sources
	// their activities produce an empty plan and no recovery sets.
	env.RegisterActivity(newPackActivities())
	env.RegisterActivity(newGeneratePAR2Activities())
	// The Verify phase is run-orchestrated as well; with an empty plan it verifies
	// nothing and produces a VerifiedPlan.
	env.RegisterActivity(newVerifyActivities())
	// The Load and Eject phases are now run-orchestrated; register their
	// activities so the test env can dispatch them.
	env.RegisterActivity(newLoadActivities())
	env.RegisterActivity(newEjectActivities())
	// The Write phase creates a Temporal session. Enable the session worker so
	// the testsuite registers the internal session creation/completion
	// activities and the session scaffold in writePhase succeeds.
	env.SetWorkerOptions(worker.Options{EnableSessionWorker: true})
	// Register both Write and Teardown activities so the test env can dispatch
	// them; TeardownSession is deferred from writePhase even in the scaffold.
	registry := newMountRegistry()
	env.RegisterActivity(newWriteActivities(registry, t.TempDir()))
	env.RegisterActivity(newTeardownActivities(registry))
	// The Report and Deliver phases are run-orchestrated. Unlike the earlier
	// phases they cannot no-op on an empty config (Report requires an escrow
	// identity and at least one written tape), so a test that runs them to
	// completion mocks them via expectReportDeliverSuccess; failure tests either
	// target an earlier phase or fail Report explicitly.
	env.RegisterActivity(newReportActivities(t.TempDir(), t.TempDir(), t.TempDir()))
	env.RegisterActivity(newDeliverActivities())
	env.RegisterActivity(&FailureActivities{})

	return env
}

// expectReportDeliverSuccess mocks the Report and Deliver activities to succeed,
// for tests that drive the pipeline to completion. Their real bodies need staged
// files, recovery binaries, and a live webhook, which the workflow-orchestration
// tests deliberately do not set up — they assert sequencing, not artifact content.
func expectReportDeliverSuccess(env *testsuite.TestWorkflowEnvironment) {
	env.OnActivity((&ReportActivities{}).BuildReport, mock.Anything, mock.Anything).
		Return(ReportOutput{}, nil)
	env.OnActivity((&DeliverActivities{}).Deliver, mock.Anything, mock.Anything).
		Return(nil)
}

// validBackupConfig returns a run config that passes config.Validate, mirroring
// internal/config's validConfig. The entry-point validation gate rejects an
// invalid payload, so the workflow-orchestration tests — which assert phase
// sequencing, not archive content — must submit a valid config.
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
			SliceSizeBytes:   1 << 30,
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

// expectResolveEmpty mocks the Resolve phase to yield an empty work list. With a
// valid config the real Resolve data activity would shell out to zfs per source;
// resolving to nothing instead lets the downstream data phases (Prepare, Pack,
// Generate PAR2, Verify) and the tape path run as no-ops on empty input — exactly
// the behavior the pipeline had on the pre-validation empty-config path — without
// touching zfs, tar, or the tape library.
func expectResolveEmpty(env *testsuite.TestWorkflowEnvironment) {
	env.OnActivity((&ResolveControlActivities{}).ResolveK8sSources, mock.Anything, mock.Anything).
		Return([]ResolvedArchive(nil), nil)
	env.OnActivity((&ResolveDataActivities{}).ResolveAndCheck, mock.Anything, mock.Anything).
		Return([]ResolvedArchive(nil), nil)
}

// expectAllPhasesSucceed mocks every phase that cannot no-op on an empty work
// list, so a valid config drives all phases to completion without real zfs/tar/
// tape work: Resolve yields an empty plan (expectResolveEmpty) and Report/Deliver
// succeed. The intervening data phases and the tape path no-op on the empty plan.
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

func TestLastCompletedPhaseQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// failAt is the phase whose activity errors; empty runs every phase to
		// completion.
		failAt string
		// want is the value lastCompletedPhase reports once the run ends.
		want string
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
				// failPhase mocks the Resolve activity itself to fail, so no
				// empty-resolve baseline is needed (nor allowed — it would double-
				// mock the same activity).
				failPhase(t, env, test.failAt)
			default:
				// Resolve to an empty plan so the phases before the failing one
				// no-op, then fail the target phase.
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

// failIfPrepareRuns registers the Prepare activity to fail the test if it is ever
// invoked, proving no staging work happens. It is the observable proxy for "before
// any staging (Prepare) work is performed": Prepare is the first phase that reads
// or writes bulk data.
func failIfPrepareRuns(t *testing.T, env *testsuite.TestWorkflowEnvironment) {
	t.Helper()

	env.OnActivity((&PrepareActivities{}).PrepareArchives, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ PrepareInput) ([]StagedArchive, error) {
			t.Error("Prepare ran despite an invalid config")

			return nil, nil
		})
}

// TestBackupRejectsInvalidConfig covers AC1: a Backup started directly through
// Temporal with a config that client-side validation would reject (here copies =
// 0) fails with a config validation error before any staging (Prepare) work runs.
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

// TestBackupRejectsZeroSources covers AC2: a Backup started with zero sources
// fails validation rather than completing as a success that wrote nothing.
func TestBackupRejectsZeroSources(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)
	failIfPrepareRuns(t, env)

	invalid := validBackupConfig()
	invalid.Sources = nil

	env.ExecuteWorkflow(Backup, invalid)

	require.True(t, env.IsWorkflowCompleted())

	// The run errors instead of completing as a no-op "success".
	err := env.GetWorkflowError()
	require.Error(t, err)
	assert.ErrorContains(t, err, "invalid config")
	assert.ErrorContains(t, err, "sources")
}

// failPhase mocks the named phase to fail through its activity. Every phase is
// run-orchestrated, so each fails via the activity it dispatches. The failure
// tests target phases up to Eject; Report and Deliver are mocked to succeed in
// newBackupEnv, so they are not handled here.
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
