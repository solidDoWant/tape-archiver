package backup

import (
	"errors"
	"testing"

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
	env.RegisterActivity(newReportActivities(t.TempDir(), t.TempDir()))
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

func TestBackupRunsAllPhasesInOrder(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)
	expectReportDeliverSuccess(env)

	env.ExecuteWorkflow(Backup, config.Config{})

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

			if test.failAt != "" {
				failPhase(t, env, test.failAt)
			} else {
				expectReportDeliverSuccess(env)
			}

			env.ExecuteWorkflow(Backup, config.Config{})

			require.True(t, env.IsWorkflowCompleted())

			value, err := env.QueryWorkflow(LastCompletedPhaseQuery)
			require.NoError(t, err)

			var got string
			require.NoError(t, value.Get(&got))
			require.Equal(t, test.want, got)
		})
	}
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
