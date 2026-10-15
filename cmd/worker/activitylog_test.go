package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/tape-archiver/pkg/logging"
)

// TestActivityLogInterceptorEmitsTaggedBoundaryLines checks the start/end lines
// carry version, worker, and the run tags; untagged lines would vanish from the
// web UI's run log panel (#303).
func TestActivityLogInterceptorEmitsTaggedBoundaryLines(t *testing.T) {
	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	// Same order as workerOptions.
	env.SetWorkerOptions(worker.Options{
		Interceptors: []interceptor.WorkerInterceptor{
			&logTagsInterceptor{},
			newActivityLogInterceptor(RoleData),
		},
	})
	env.RegisterWorkflow(loggingWorkflow)
	env.RegisterActivity(loggingActivity)

	output := captureStderr(t, func() {
		logging.Setup("info")
		env.ExecuteWorkflow(loggingWorkflow)
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var identity runIdentity
	require.NoError(t, env.GetWorkflowResult(&identity))
	require.NotEmpty(t, identity.RunID, "test env must give the activity a run identity")

	started := findRecordByMessage(t, output, "activity started")
	assert.Equal(t, identity.WorkflowID, started["WorkflowID"],
		"the start line must carry the run's WorkflowID so the log panel matches it")
	assert.Equal(t, identity.RunID, started["RunID"],
		"the start line must carry the run's RunID so the log panel matches it")
	assert.Equal(t, "loggingActivity", started["activity"])
	assert.NotEmpty(t, started["version"], "the start line must stamp the build version")

	workerID, ok := started["worker"].(string)
	require.True(t, ok, "the start line must include a worker identity string")
	assert.Contains(t, workerID, string(RoleData),
		"the worker identity must name the role")

	finished := findRecordByMessage(t, output, "activity finished")
	assert.Equal(t, identity.RunID, finished["RunID"],
		"the end line must carry the run's RunID so the log panel matches it")
	assert.Equal(t, "loggingActivity", finished["activity"])
	assert.Contains(t, finished, "durationMs", "the end line must report the activity duration")
}
