package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func dryRunProbeWorkflow(ctx workflow.Context) (bool, error) { return isDryRun(ctx), nil }

func TestIsDryRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		memo map[string]interface{}
		want bool
	}{
		{name: "no memo"},
		{name: "real run", memo: map[string]interface{}{MemoKeyDryRun: false}},
		{name: "dry run", memo: map[string]interface{}{MemoKeyDryRun: true}, want: true},
		{name: "malformed", memo: map[string]interface{}{MemoKeyDryRun: "yes"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var suite testsuite.WorkflowTestSuite

			env := suite.NewTestWorkflowEnvironment()
			if test.memo != nil {
				require.NoError(t, env.SetMemoOnStart(test.memo))
			}

			env.ExecuteWorkflow(dryRunProbeWorkflow)
			require.NoError(t, env.GetWorkflowError())

			var got bool
			require.NoError(t, env.GetWorkflowResult(&got))
			assert.Equal(t, test.want, got)
		})
	}
}
