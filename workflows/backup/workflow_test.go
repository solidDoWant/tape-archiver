package backup

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

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

	for _, phase := range backupPhases() {
		env.RegisterActivity(phase.activity)
	}

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
				env.OnActivity(activityFor(t, test.failAt), mock.Anything).
					Return(errors.New("boom"))
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
// it with OnActivity.
func activityFor(t *testing.T, name string) any {
	t.Helper()

	for _, phase := range backupPhases() {
		if phase.name == name {
			return phase.activity
		}
	}

	t.Fatalf("no phase named %q", name)

	return nil
}
