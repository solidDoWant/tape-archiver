package backup

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// mirrorTestWorkflow exercises runMirrorLibrary alone, so the gate can be
// asserted without driving a whole backup.
func mirrorTestWorkflow(ctx workflow.Context, cfg config.Config) error {
	return runMirrorLibrary(ctx, cfg)
}

// TestMirrorSkippedOnRealRun is the safety contract. A real run leaves
// Library.MirrorSourceChanger empty, and the mirror must then never be dispatched
// at all — not dispatched-and-skipped. Only runsubmit.ApplyDryRun sets that field,
// so this is what keeps a reprovision from ever being aimed at a production
// library's configuration.
func TestMirrorSkippedOnRealRun(t *testing.T) {
	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(mirrorTestWorkflow)
	env.RegisterActivity(newMirrorActivities())

	dispatched := false

	env.OnActivity((&MirrorActivities{}).MirrorLibrary, mock.Anything, mock.Anything).Return(
		func(context.Context, MirrorLibraryInput) (MirrorLibraryResult, error) {
			dispatched = true

			return MirrorLibraryResult{}, nil
		})

	// A real run: the library is configured normally, with no mirror source.
	env.ExecuteWorkflow(mirrorTestWorkflow, config.Config{
		Library: config.Library{Changer: "/dev/sch0"},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.False(t, dispatched,
		"a real run must never dispatch the library mirror")
}

// TestMirrorDispatchedOnDryRunWithBothChangers checks the dry-run path passes the
// real changer as the source and the virtual one as the target — the wrong way
// round would rewrite nothing useful and read the library being reprovisioned.
func TestMirrorDispatchedOnDryRunWithBothChangers(t *testing.T) {
	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(mirrorTestWorkflow)
	env.RegisterActivity(newMirrorActivities())

	var got MirrorLibraryInput

	env.OnActivity((&MirrorActivities{}).MirrorLibrary, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input MirrorLibraryInput) (MirrorLibraryResult, error) {
			got = input

			return MirrorLibraryResult{OccupiedSlots: 2, TotalSlots: 45}, nil
		})

	env.ExecuteWorkflow(mirrorTestWorkflow, config.Config{
		Library: config.Library{
			Changer:             "/dev/sch2", // the virtual library the run targets
			MirrorSourceChanger: "/dev/sch0", // the real library to mirror from
		},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, "/dev/sch0", got.SourceChanger,
		"the real library must be the mirror source, and is only ever read")
	assert.Equal(t, "/dev/sch2", got.TargetChanger,
		"the virtual library the run targets must be the mirror target")
}
