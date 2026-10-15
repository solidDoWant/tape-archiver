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

// TestMirrorGate is the safety contract: only a dry run carrying a mirror source
// dispatches the mirror, so it can never be aimed at a production library
// (issue #382).
func TestMirrorGate(t *testing.T) {
	tests := []struct {
		name         string
		dryRun       *bool
		mirrorSource string
		want         *MirrorLibraryInput
	}{
		{name: "real run", dryRun: new(false)},
		{name: "real run with a mirror source", dryRun: new(false), mirrorSource: "/dev/sch0"},
		{name: "no dry-run memo with a mirror source", mirrorSource: "/dev/sch0"},
		{name: "dry run without a mirror source", dryRun: new(true)},
		{
			name:         "dry run with a mirror source",
			dryRun:       new(true),
			mirrorSource: "/dev/sch0",
			// The real library is the source and only ever read; the virtual one
			// the run targets is the target.
			want: &MirrorLibraryInput{SourceChanger: "/dev/sch0", TargetChanger: "/dev/sch2"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite

			env := suite.NewTestWorkflowEnvironment()
			env.RegisterWorkflow(mirrorTestWorkflow)
			env.RegisterActivity(newMirrorActivities())

			if test.dryRun != nil {
				require.NoError(t, env.SetMemoOnStart(map[string]interface{}{MemoKeyDryRun: *test.dryRun}))
			}

			var got *MirrorLibraryInput

			env.OnActivity((&MirrorActivities{}).MirrorLibrary, mock.Anything, mock.Anything).Return(
				func(_ context.Context, input MirrorLibraryInput) (MirrorLibraryResult, error) {
					got = &input

					return MirrorLibraryResult{OccupiedSlots: 2, TotalSlots: 45}, nil
				})

			env.ExecuteWorkflow(mirrorTestWorkflow, config.Config{
				Library: config.Library{Changer: "/dev/sch2", MirrorSourceChanger: test.mirrorSource},
			})

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())
			assert.Equal(t, test.want, got)
		})
	}
}
