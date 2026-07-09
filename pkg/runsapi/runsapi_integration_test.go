//go:build integration

package runsapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/pkg/runsapi"
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

// stubBackupWorkflow stands in for the real backup workflow: it registers
// under backup.WorkflowType on backup.TaskQueue and answers
// backup.LastCompletedPhaseQuery, then stays running until signalled — the
// same shape cmd/tapectl's integration tests use.
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

// requireTemporalAddress skips the test unless a Temporal server is reachable
// at TEMPORAL_ADDRESS. make test-integration arranges this via temporal-up.
func requireTemporalAddress(t *testing.T) {
	t.Helper()

	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; run via `make test-integration`")
	}
}

// isolateTemporalConfig points TEMPORAL_CONFIG_FILE at a fresh empty TOML
// file so envconfig does not pick up a stray
// ~/.config/temporalio/temporal.toml on the host, mirroring
// pkg/temporalclient's own integration tests.
func isolateTemporalConfig(t *testing.T) {
	t.Helper()

	emptyConfig := filepath.Join(t.TempDir(), "empty.toml")
	require.NoError(t, os.WriteFile(emptyConfig, nil, 0o600))
	t.Setenv("TEMPORAL_CONFIG_FILE", emptyConfig)
	t.Setenv("TEMPORAL_PROFILE", "")
}

// TestListAndGetRunAgainstRealTemporal exercises runsapi.New's handlers
// against a real dev Temporal (make temporal-up): it submits a stub backup
// workflow, then proves GET /api/runs lists it (newest-first, via real
// visibility) and GET /api/runs/{runID} describes it including the phase
// answered by the real query handler — the behavior no fake client can
// prove.
func TestListAndGetRunAgainstRealTemporal(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	ctx := t.Context()

	temporalClient, shutdown, err := temporalclient.New(ctx, nil)
	require.NoError(t, err)

	defer shutdown()

	backupWorker := worker.New(temporalClient, backup.TaskQueue, worker.Options{})
	backupWorker.RegisterWorkflowWithOptions(stubBackupWorkflow, workflow.RegisterOptions{Name: backup.WorkflowType})
	require.NoError(t, backupWorker.Start())

	defer backupWorker.Stop()

	// TERMINATE_EXISTING guarantees a clean fresh execution starts in one
	// call, atomically replacing any leftover run from a previous failed
	// test — a best-effort pre-signal is racy when the leftover run's own
	// worker process has already exited: the signal is durably recorded but
	// never processed (no poller left to receive it), so the ID never
	// actually frees up on its own.
	run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       backup.WorkflowID,
		TaskQueue:                backup.TaskQueue,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_TERMINATE_EXISTING,
	}, backup.WorkflowType, nil)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil)
	})

	handler := runsapi.New(temporalClient)
	server := httptest.NewServer(handler)

	defer server.Close()

	// Visibility is eventually consistent, so both the list and the phase
	// query are retried until they observe the running workflow.
	require.Eventually(t, func() bool {
		resp, err := http.Get(server.URL + "/api/runs")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			return false
		}

		var body runsapi.RunsResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return false
		}

		for _, summary := range body.Runs {
			if summary.RunID == run.GetRunID() {
				assert.Equal(t, backup.WorkflowID, summary.WorkflowID)
				assert.Equal(t, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING.String(), summary.Status)
				assert.False(t, summary.StartTime.IsZero())
				assert.Nil(t, summary.CloseTime)

				return true
			}
		}

		return false
	}, 30*time.Second, 250*time.Millisecond, "GET /api/runs never listed the submitted run")

	require.Eventually(t, func() bool {
		resp, err := http.Get(server.URL + "/api/runs/" + run.GetRunID())
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			return false
		}

		var detail runsapi.RunDetail
		if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
			return false
		}

		return detail.LastCompletedPhase == stubPhase
	}, 30*time.Second, 250*time.Millisecond, "GET /api/runs/{runID} never reported the stub's phase")

	// A well-formed (UUID-shaped) run ID that was never submitted is a 404.
	resp, err := http.Get(server.URL + "/api/runs/00000000-0000-0000-0000-000000000000")
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// A malformed run ID (Temporal run IDs are UUIDs) is a 400, not a 404 —
	// the real server reports this as InvalidArgument, distinct from
	// NotFound, and the API maps the distinction through rather than
	// collapsing both into one status.
	badResp, err := http.Get(server.URL + "/api/runs/not-a-uuid")
	require.NoError(t, err)

	defer func() { _ = badResp.Body.Close() }()

	assert.Equal(t, http.StatusBadRequest, badResp.StatusCode)

	// Unblock the stub so it completes rather than lingering past the test.
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil))
}
