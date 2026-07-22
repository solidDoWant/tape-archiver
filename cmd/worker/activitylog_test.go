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

// TestActivityLogInterceptorEmitsTaggedBoundaryLines is the end-to-end proof that
// activityLogInterceptor emits an "activity started" and an "activity finished"
// line for every activity, stamped with the build version and worker identity —
// and, because it is registered inside logTagsInterceptor, that those lines carry
// the run's WorkflowID/RunID. That last part is the real risk: an untagged line
// would be silently dropped from the web UI's run-detail log panel (the RunID
// filter would not match it), the same failure #303 fixed for domain logs. This
// asserts the boundary lines land tagged, through the exact same slog →
// contextHandler → stderr path as the activity's own logs.
func TestActivityLogInterceptorEmitsTaggedBoundaryLines(t *testing.T) {
	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	// Both interceptors, in the same order workerOptions installs them:
	// logTagsInterceptor seeds the run tags, then activityLogInterceptor runs
	// inside it and its lines inherit those tags.
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

	// The start line carries the run tags (so the panel shows it) plus the
	// version/worker/activity stamps the operator asked for.
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

	// The end line is also tagged and reports the duration.
	finished := findRecordByMessage(t, output, "activity finished")
	assert.Equal(t, identity.RunID, finished["RunID"],
		"the end line must carry the run's RunID so the log panel matches it")
	assert.Equal(t, "loggingActivity", finished["activity"])
	assert.Contains(t, finished, "durationMs", "the end line must report the activity duration")
}
