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
	PhaseDeliver,
}

// newBackupEnv returns a test workflow environment with the Backup workflow and
// every phase stub plus the failure-alert activity registered, so a test only
// overrides the behavior it cares about with OnActivity.
func newBackupEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()

	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(Backup)

	// Stub phases register their single activity. All tape-path phases (Load,
	// Write, Eject) are now run-orchestrated, so no stub activity is registered
	// from the phase table. Only Report and Deliver still use single stubs.
	for _, phase := range backupPhases() {
		if phase.activity != nil {
			env.RegisterActivity(phase.activity)
		}
	}

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
	env.RegisterActivity(&FailureActivities{})

	return env
}

func TestBackupRunsAllPhasesInOrder(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)

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

// activityFor returns the stub activity for the named phase so a test can target
// it with OnActivity. It is only valid for stub phases that expose a single
// activity function; run-orchestrated phases (Resolve, Prepare, Pack, Generate
// PAR2, Verify, Load, Write, Eject) have phase.activity == nil and must be
// handled in failPhase's switch instead.
func activityFor(t *testing.T, name string) any {
	t.Helper()

	for _, phase := range backupPhases() {
		if phase.name == name {
			if phase.activity == nil {
				t.Fatalf("phase %q has no single stub activity; add it to failPhase's switch", name)
			}

			return phase.activity
		}
	}

	t.Fatalf("no phase named %q", name)

	return nil
}

// failPhase mocks the named phase to fail. The run-orchestrated phases (Resolve,
// Prepare, Pack, Generate PAR2, Verify, Load, Write, Eject) fail through their
// activities; every other phase is a single stub activity returning just an
// error.
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

	env.OnActivity(activityFor(t, name), mock.Anything).
		Return(errors.New("boom"))
}
