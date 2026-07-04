//go:build integration

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

const (
	// stubPhase is the value the stub workflow reports for the last completed
	// phase query, standing in for a real phase name (SPEC §4.3).
	stubPhase = "Verify"
	// stubFinishSignal unblocks the stub workflow so it completes.
	stubFinishSignal = "finish"
)

// stubBackupWorkflow stands in for the real backup workflow (owned by #17). It
// honors the contract tapectl depends on: it registers under backup.WorkflowType
// on backup.TaskQueue and answers backup.LastCompletedPhaseQuery, then stays
// running until signalled so `status` observes a RUNNING workflow.
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

// skipWithoutTemporal skips the test unless a Temporal server is reachable at
// TEMPORAL_ADDRESS. The make test-integration target arranges this.
func skipWithoutTemporal(t *testing.T) {
	t.Helper()

	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; run via `make test-integration`")
	}
}

// isolateTemporalConfig points TEMPORAL_CONFIG_FILE at a fresh empty TOML file
// so envconfig does not pick up a stray ~/.config/temporalio/temporal.toml on
// the host, mirroring pkg/temporalclient's integration tests.
func isolateTemporalConfig(t *testing.T) {
	t.Helper()

	emptyConfig := filepath.Join(t.TempDir(), "empty.toml")
	require.NoError(t, os.WriteFile(emptyConfig, nil, 0o600))
	t.Setenv("TEMPORAL_CONFIG_FILE", emptyConfig)
	t.Setenv("TEMPORAL_PROFILE", "")
}

// TestRunThenStatus exercises the observable behavior of both subcommands
// against a live Temporal: `run` submits a workflow and prints its ID, and
// `status` reports the running state and last completed phase via the query.
func TestRunThenStatus(t *testing.T) {
	skipWithoutTemporal(t)
	isolateTemporalConfig(t)

	ctx := t.Context()

	temporalClient, shutdown, err := temporalclient.New(ctx, nil)
	require.NoError(t, err)

	defer shutdown()

	// Run the stub backup workflow on the control task queue under the agreed
	// type name so the submitted workflow has somewhere to execute.
	backupWorker := worker.New(temporalClient, backup.TaskQueue, worker.Options{})
	backupWorker.RegisterWorkflowWithOptions(stubBackupWorkflow, workflow.RegisterOptions{Name: backup.WorkflowType})
	require.NoError(t, backupWorker.Start())

	defer backupWorker.Stop()

	// Submit via `tapectl run` and capture the printed workflow ID.
	var runOut bytes.Buffer
	require.NoError(t, submitRun(ctx, []string{"--config", writeConfig(t, validConfigJSON)}, &runOut))

	workflowID := strings.TrimSpace(runOut.String())
	require.NotEmpty(t, workflowID, "run must print the workflow ID")
	assert.Equal(t, backupWorkflowID, workflowID, "run submits under the singleton workflow ID")

	// `tapectl status` must eventually report the running workflow and the phase
	// the stub reports via the query.
	require.Eventually(t, func() bool {
		var statusOut bytes.Buffer
		if err := showStatus(ctx, []string{workflowID}, &statusOut); err != nil {
			return false
		}

		output := statusOut.String()

		return strings.Contains(output, "Running") && strings.Contains(output, stubPhase)
	}, 30*time.Second, 250*time.Millisecond, "status did not report a running workflow and its phase")

	// Unblock the stub so it completes rather than lingering past the test.
	require.NoError(t, temporalClient.SignalWorkflow(ctx, workflowID, "", stubFinishSignal, nil))
}

// TestConcurrentRunGuard exercises the singleton concurrency guard against a
// live Temporal: while one backup run is in progress a second submission is
// refused with an actionable error, and once the first run finishes a fresh run
// starts normally.
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

	// Best-effort safety net: unblock whatever backup run is current so a failed
	// assertion does not leave the singleton ID occupied for later runs.
	t.Cleanup(func() {
		_ = temporalClient.SignalWorkflow(ctx, backupWorkflowID, "", stubFinishSignal, nil)
	})

	// Start the first run. Tolerate a still-closing run from a previous test by
	// retrying until the singleton ID is free.
	require.Eventually(t, func() bool {
		return submitRun(ctx, []string{"--config", configPath}, &bytes.Buffer{}) == nil
	}, 30*time.Second, 250*time.Millisecond, "first run never started")

	// AC1 + AC3: a second concurrent submission is refused with an actionable
	// error naming the in-progress run, not an opaque Temporal error.
	err = submitRun(ctx, []string{"--config", configPath}, &bytes.Buffer{})
	require.Error(t, err, "second concurrent run must be refused")

	message := err.Error()
	assert.Contains(t, message, "already in progress")
	assert.Contains(t, message, backupWorkflowID)
	assert.Contains(t, message, "tapectl status")

	// Finish the first run so the singleton ID frees up.
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backupWorkflowID, "", stubFinishSignal, nil))

	// AC2: once the first run has closed, a new run starts normally.
	require.Eventually(t, func() bool {
		return submitRun(ctx, []string{"--config", configPath}, &bytes.Buffer{}) == nil
	}, 30*time.Second, 250*time.Millisecond, "new run did not start after the first completed")

	// Unblock the run started by the AC2 check so it does not linger.
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backupWorkflowID, "", stubFinishSignal, nil))
}
