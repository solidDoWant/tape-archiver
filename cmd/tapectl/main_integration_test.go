//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

const (
	stubPhase = "Verify" // stands in for a real phase name (SPEC §4.3)
	// stubFinishSignal unblocks the stub workflow.
	stubFinishSignal = "finish"
)

// stubBackupWorkflow answers the phase query and stays running until signalled.
func stubBackupWorkflow(ctx workflow.Context, _ interface{}) error {
	phase := stubPhase

	if err := workflow.SetQueryHandler(ctx, backup.LastCompletedPhaseQuery, func() (string, error) {
		return phase, nil
	}); err != nil {
		return err
	}

	workflow.GetSignalChannel(ctx, stubFinishSignal).Receive(ctx, nil)

	return nil
}

func skipWithoutTemporal(t *testing.T) {
	t.Helper()

	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; run via `make test-integration`")
	}
}

// isolateTemporalConfig keeps envconfig from picking up the host's
// ~/.config/temporalio/temporal.toml.
func isolateTemporalConfig(t *testing.T) {
	t.Helper()

	emptyConfig := filepath.Join(t.TempDir(), "empty.toml")
	require.NoError(t, os.WriteFile(emptyConfig, nil, 0o600))
	t.Setenv("TEMPORAL_CONFIG_FILE", emptyConfig)
	t.Setenv("TEMPORAL_PROFILE", "")
}

func TestRunThenStatus(t *testing.T) {
	skipWithoutTemporal(t)
	isolateTemporalConfig(t)

	ctx := t.Context()

	temporalClient, shutdown, err := temporalclient.New(ctx, nil)
	require.NoError(t, err)

	defer shutdown()

	backupWorker := worker.New(temporalClient, backup.TaskQueue, worker.Options{})
	backupWorker.RegisterWorkflowWithOptions(stubBackupWorkflow, workflow.RegisterOptions{Name: backup.WorkflowType})
	require.NoError(t, backupWorker.Start())

	defer backupWorker.Stop()

	var runOut bytes.Buffer
	require.NoError(t, submitRun(ctx, []string{"--config", writeConfig(t, validConfigJSON)}, &runOut))

	workflowID := strings.TrimSpace(runOut.String())
	require.NotEmpty(t, workflowID, "run must print the workflow ID")
	assert.Equal(t, backupWorkflowID, workflowID, "run submits under the singleton workflow ID")

	require.Eventually(t, func() bool {
		var statusOut bytes.Buffer
		if err := showStatus(ctx, nil, &statusOut); err != nil {
			return false
		}

		output := statusOut.String()

		return strings.Contains(output, "Running") && strings.Contains(output, stubPhase)
	}, 30*time.Second, 250*time.Millisecond, "status did not report a running workflow and its phase")

	// Wait for completion while this worker still polls (see
	// awaitBackupSingletonClosed).
	require.NoError(t, temporalClient.SignalWorkflow(ctx, workflowID, "", stubFinishSignal, nil))
	awaitBackupSingletonClosed(t, ctx, temporalClient)
}

func TestConcurrentRunGuard(t *testing.T) {
	skipWithoutTemporal(t)
	isolateTemporalConfig(t)

	ctx := t.Context()

	temporalClient, shutdown, err := temporalclient.New(ctx, nil)
	require.NoError(t, err)

	defer shutdown()

	backupWorker := worker.New(temporalClient, backup.TaskQueue, worker.Options{})
	backupWorker.RegisterWorkflowWithOptions(stubBackupWorkflow, workflow.RegisterOptions{Name: backup.WorkflowType})
	require.NoError(t, backupWorker.Start())

	defer backupWorker.Stop()

	configPath := writeConfig(t, validConfigJSON)

	// Best effort: don't leave the singleton occupied on a failed assertion.
	t.Cleanup(func() {
		_ = temporalClient.SignalWorkflow(ctx, backupWorkflowID, "", stubFinishSignal, nil)
	})

	// Tolerate a still-closing run from a previous test.
	require.Eventually(t, func() bool {
		return submitRun(ctx, []string{"--config", configPath}, &bytes.Buffer{}) == nil
	}, 30*time.Second, 250*time.Millisecond, "first run never started")

	err = submitRun(ctx, []string{"--config", configPath}, &bytes.Buffer{})
	require.Error(t, err, "second concurrent run must be refused")

	message := err.Error()
	assert.Contains(t, message, "already in progress")
	assert.Contains(t, message, backupWorkflowID)
	assert.Contains(t, message, "tapectl status")

	require.NoError(t, temporalClient.SignalWorkflow(ctx, backupWorkflowID, "", stubFinishSignal, nil))

	require.Eventually(t, func() bool {
		return submitRun(ctx, []string{"--config", configPath}, &bytes.Buffer{}) == nil
	}, 30*time.Second, 250*time.Millisecond, "new run did not start after the first completed")

	// A run leaked here survives into the rest of the suite (issue #338).
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backupWorkflowID, "", stubFinishSignal, nil))
	awaitBackupSingletonClosed(t, ctx, temporalClient)
}

// awaitBackupSingletonClosed blocks until the current `backup` execution closes.
//
// A signal only frees the singleton if a worker is still polling, and the
// deferred backupWorker.Stop runs before t.Cleanup. An orphaned run gets
// adopted by the next process polling the `control` queue (cmd/worker's test
// binary, which runs it as a real backup and then exits), stranding it RUNNING
// and making every later submission 409 (issue #338).
func awaitBackupSingletonClosed(t *testing.T, ctx context.Context, temporalClient client.Client) {
	t.Helper()

	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	require.NoError(t, temporalClient.GetWorkflow(waitCtx, backupWorkflowID, "").Get(waitCtx, nil),
		"the stub run %q must complete so the singleton workflow ID is free for later tests and packages",
		backupWorkflowID)
}
