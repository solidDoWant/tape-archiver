package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/pkg/logging"
)

// runIdentity is what loggingActivity reports back so the test can compare the
// run identity the activity actually saw against the tags that landed on its log
// line.
type runIdentity struct {
	WorkflowID string
	RunID      string
}

// loggingActivity emits one bulk log line the way real activities and pkg/*
// helpers do — a plain slog.*Context call — and returns the run identity Temporal
// gave it, so the test can assert the emitted line carries exactly those tags.
func loggingActivity(ctx context.Context) (runIdentity, error) {
	slog.InfoContext(ctx, activityLogMessage)

	info := activity.GetInfo(ctx)

	return runIdentity{
		WorkflowID: info.WorkflowExecution.ID,
		RunID:      info.WorkflowExecution.RunID,
	}, nil
}

// loggingWorkflow runs loggingActivity once. It exists only to give the activity
// a real workflow execution (WorkflowID/RunID), which the standalone activity
// test environment does not populate.
func loggingWorkflow(ctx workflow.Context) (runIdentity, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
	})

	var identity runIdentity

	err := workflow.ExecuteActivity(ctx, loggingActivity).Get(ctx, &identity)

	return identity, err
}

const activityLogMessage = "logtags test: activity emitted a bulk log line"

// TestLogTagsInterceptorTagsActivityLogs is the end-to-end proof of #303: with the
// interceptor installed, a plain slog line emitted inside an activity carries the
// run's WorkflowID/RunID — the fields the web UI's log panel filters on — without
// the activity routing through a Temporal context logger.
func TestLogTagsInterceptorTagsActivityLogs(t *testing.T) {
	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{
		Interceptors: []interceptor.WorkerInterceptor{&logTagsInterceptor{}},
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
	require.NotEmpty(t, identity.WorkflowID, "test env must give the activity a workflow identity")
	require.NotEmpty(t, identity.RunID)

	record := findRecordByMessage(t, output, activityLogMessage)
	assert.Equal(t, identity.WorkflowID, record["WorkflowID"],
		"activity log line must carry the run's WorkflowID")
	assert.Equal(t, identity.RunID, record["RunID"],
		"activity log line must carry the run's RunID")
}

// captureStderr redirects os.Stderr to a pipe for the duration of fn and returns
// everything written to it. The pipe is drained concurrently so a large volume of
// SDK/test output cannot deadlock fn on a full pipe buffer.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	original := os.Stderr
	read, write, err := os.Pipe()
	require.NoError(t, err)

	os.Stderr = write

	done := make(chan string, 1)

	go func() {
		data, _ := io.ReadAll(read)
		done <- string(data)
	}()

	fn()

	os.Stderr = original

	require.NoError(t, write.Close())

	output := <-done

	require.NoError(t, read.Close())

	return output
}

// findRecordByMessage decodes the captured JSON log lines and returns the one
// whose message is msg, failing the test if no such line was emitted.
func findRecordByMessage(t *testing.T, output, msg string) map[string]any {
	t.Helper()

	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}

		if record[slog.MessageKey] == msg {
			return record
		}
	}

	t.Fatalf("no log record with message %q in captured output:\n%s", msg, output)

	return nil
}
