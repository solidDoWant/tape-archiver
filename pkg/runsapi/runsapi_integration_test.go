//go:build integration

package runsapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

// validSubmitConfigJSON is a minimal valid run-config document (same shape
// cmd/tapectl's integration tests submit) for exercising POST /api/runs
// against a real Temporal.
const validSubmitConfigJSON = `{
  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
  "copies": 2,
  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
  "redundancy": {"targetPercentage": 10, "sliceSizeBytes": 1073741824},
  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
  "delivery": {"webhookUrl": "https://discord.com/api/webhooks/123/abc"}
}`

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

// setPauseSignal is a test-only signal (not part of workflows/backup's real
// contract) that stubPausableBackupWorkflow uses to let a test set the pause
// state CurrentPauseQuery should report next.
const setPauseSignal = "setPause"

// lastSignalQuery is a test-only query (not part of workflows/backup's real
// contract) that reports which of OperatorResumeSignal/OperatorAbortSignal
// stubPausableBackupWorkflow most recently received, so a test can prove the
// real signal was actually delivered and processed — not just that the pause
// state happened to change for some other reason.
const lastSignalQuery = "lastSignal"

// stubPausableBackupWorkflow extends stubBackupWorkflow with a settable pause
// state and the real OperatorResumeSignal/OperatorAbortSignal names, so
// TestResumeAndAbortAgainstRealTemporal can drive POST
// /api/runs/{runID}/resume and /abort against a real Temporal server and
// observe the real signal being delivered and acted on — behavior no fake
// client can prove. It is a test-only stand-in, not the real workflow: the
// pre-existing resume/abort signal-handling behavior (drainStalePauseSignals
// etc.) is covered by workflows/backup's own tests, and the real workflow's
// three pause sites setting/clearing CurrentPause around their existing wait
// calls are covered by workflows/backup's own unit tests plus, for the Eject
// pause specifically, eject_pause_integration_test.go's real-mhvtl
// TestEjectAutoResumeOnAccess. This stub proves only the HTTP layer's own
// logic: that it queries backup.CurrentPauseQuery and sends the correct
// signal to the correct run — the same "stub the workflow, drive the real
// Temporal + HTTP layer" pattern stubBackupWorkflow above already uses for
// GET/POST /api/runs.
func stubPausableBackupWorkflow(ctx workflow.Context, _ interface{}) error {
	phase := stubPhase
	pause := backup.CurrentPause{}
	lastSignal := ""

	if err := workflow.SetQueryHandler(ctx, backup.LastCompletedPhaseQuery, func() (string, error) {
		return phase, nil
	}); err != nil {
		return err
	}

	if err := workflow.SetQueryHandler(ctx, backup.CurrentPauseQuery, func() (backup.CurrentPause, error) {
		return pause, nil
	}); err != nil {
		return err
	}

	if err := workflow.SetQueryHandler(ctx, lastSignalQuery, func() (string, error) {
		return lastSignal, nil
	}); err != nil {
		return err
	}

	setPauseCh := workflow.GetSignalChannel(ctx, setPauseSignal)
	resumeCh := workflow.GetSignalChannel(ctx, backup.OperatorResumeSignal)
	abortCh := workflow.GetSignalChannel(ctx, backup.OperatorAbortSignal)
	finishCh := workflow.GetSignalChannel(ctx, stubFinishSignal)

	finished := false

	for !finished {
		selector := workflow.NewSelector(ctx)
		selector.AddReceive(setPauseCh, func(c workflow.ReceiveChannel, _ bool) {
			c.Receive(ctx, &pause)
		})
		selector.AddReceive(resumeCh, func(c workflow.ReceiveChannel, _ bool) {
			c.Receive(ctx, nil)

			lastSignal = "resume"
			pause = backup.CurrentPause{}
		})
		selector.AddReceive(abortCh, func(c workflow.ReceiveChannel, _ bool) {
			c.Receive(ctx, nil)

			lastSignal = "abort"
			pause = backup.CurrentPause{}
		})
		selector.AddReceive(finishCh, func(c workflow.ReceiveChannel, _ bool) {
			c.Receive(ctx, nil)

			finished = true
		})
		selector.Select(ctx)
	}

	return nil
}

// getRunDetail fetches and decodes GET /api/runs/{runID}, failing the test on
// a transport error but leaving status/body assertions to the caller.
func getRunDetail(t *testing.T, serverURL, runID string) (int, runsapi.RunDetail) {
	t.Helper()

	resp, err := http.Get(serverURL + "/api/runs/" + runID)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	var detail runsapi.RunDetail

	_ = json.NewDecoder(resp.Body).Decode(&detail)

	return resp.StatusCode, detail
}

// postAction POSTs to serverURL+"/api/runs/"+runID+"/"+action (resume or
// abort) and returns the status code.
func postAction(t *testing.T, serverURL, runID, action string) int {
	t.Helper()

	resp, err := http.Post(serverURL+"/api/runs/"+runID+"/"+action, "application/json", nil)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode
}

// queryLastSignal asks the stub workflow directly (bypassing the HTTP layer)
// which of resume/abort it most recently received, so a test can prove a
// rejected request never reached the workflow at all — a 409 alone does not
// distinguish "correctly rejected" from "sent, then coincidentally had no
// effect".
func queryLastSignal(t *testing.T, temporalClient client.Client, ctx context.Context, runID string) string {
	t.Helper()

	value, err := temporalClient.QueryWorkflow(ctx, backup.WorkflowID, runID, lastSignalQuery)
	require.NoError(t, err)

	var signal string
	require.NoError(t, value.Get(&signal))

	return signal
}

// TestResumeAndAbortAgainstRealTemporal exercises POST
// /api/runs/{runID}/resume and /abort against a real dev Temporal (make
// temporal-up): a request against an unpaused run is rejected (409) and never
// reaches the workflow; once paused, resume/abort actually deliver the real
// OperatorResumeSignal/OperatorAbortSignal and the workflow observes them
// (CurrentPauseQuery clears, lastSignalQuery records which arrived); and an
// abort against an Eject pause is rejected client-side and never reaches the
// workflow at all — proving the "web API is stricter than tapectl"
// pause-state check (signalPausedRun's doc comment) actually holds against a
// real server, not just the fakeTemporalClient unit tests.
func TestResumeAndAbortAgainstRealTemporal(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	ctx := t.Context()

	temporalClient, shutdown, err := temporalclient.New(ctx, nil)
	require.NoError(t, err)

	defer shutdown()

	backupWorker := worker.New(temporalClient, backup.TaskQueue, worker.Options{})
	backupWorker.RegisterWorkflowWithOptions(stubPausableBackupWorkflow, workflow.RegisterOptions{Name: backup.WorkflowType})
	require.NoError(t, backupWorker.Start())

	defer backupWorker.Stop()

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

	runID := run.GetRunID()

	// Wait for the run to be visible AND for stubPausableBackupWorkflow's
	// first workflow task to actually execute and register its query
	// handlers, not just for DescribeWorkflowExecution to succeed:
	// fetchRunDetail degrades gracefully when a query fails (a deliberate
	// property this same PR added, see CurrentPauseInfo.Unknown), so GET
	// /api/runs/{runID} can return 200 with a placeholder LastCompletedPhase
	// before CurrentPauseQuery/lastSignalQuery exist yet — driving resume/abort
	// that early hits "unknown queryType" against a real server instead of the
	// behavior under test. All three query handlers are registered
	// synchronously in the same first workflow task, so waiting for
	// LastCompletedPhase to report the real stub value proves the others are
	// registered too.
	require.Eventually(t, func() bool {
		status, detail := getRunDetail(t, server.URL, runID)

		return status == http.StatusOK && detail.LastCompletedPhase == stubPhase
	}, 30*time.Second, 250*time.Millisecond, "GET /api/runs/{runID} never reported the stub workflow's query handlers as ready")

	// Not yet paused: both actions are rejected with 409, not sent into the
	// void.
	assert.Equal(t, http.StatusConflict, postAction(t, server.URL, runID, "resume"),
		"resume against an unpaused run must be rejected")
	assert.Equal(t, http.StatusConflict, postAction(t, server.URL, runID, "abort"),
		"abort against an unpaused run must be rejected")
	assert.Empty(t, queryLastSignal(t, temporalClient, ctx, runID), "no signal must have reached the workflow yet")

	// Simulate a write-failure pause and resume it through the API.
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", setPauseSignal,
		backup.CurrentPause{Kind: backup.PauseWriteFailure, Phase: backup.PhaseWrite, AffectedTapes: []string{"TA0001L6"}}))

	require.Eventually(t, func() bool {
		status, detail := getRunDetail(t, server.URL, runID)

		return status == http.StatusOK && detail.CurrentPause.Kind == string(backup.PauseWriteFailure)
	}, 30*time.Second, 250*time.Millisecond, "GET /api/runs/{runID} never reported the simulated write-failure pause")

	assert.Equal(t, http.StatusAccepted, postAction(t, server.URL, runID, "resume"),
		"resume against a paused run must succeed")

	require.Eventually(t, func() bool {
		return queryLastSignal(t, temporalClient, ctx, runID) == "resume"
	}, 30*time.Second, 250*time.Millisecond, "the workflow never observed the real resume signal")

	_, detail := getRunDetail(t, server.URL, runID)
	assert.Equal(t, "", detail.CurrentPause.Kind, "the pause clears once the workflow processes the resume")

	// Simulate a burn pause and abort it through the API.
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", setPauseSignal,
		backup.CurrentPause{Kind: backup.PauseBurn, Devices: []string{"/dev/sr0"}}))

	require.Eventually(t, func() bool {
		status, detail := getRunDetail(t, server.URL, runID)

		return status == http.StatusOK && detail.CurrentPause.Kind == string(backup.PauseBurn)
	}, 30*time.Second, 250*time.Millisecond, "GET /api/runs/{runID} never reported the simulated burn pause")

	assert.Equal(t, http.StatusAccepted, postAction(t, server.URL, runID, "abort"),
		"abort against a burn pause must succeed")

	require.Eventually(t, func() bool {
		return queryLastSignal(t, temporalClient, ctx, runID) == "abort"
	}, 30*time.Second, 250*time.Millisecond, "the workflow never observed the real abort signal")

	// Simulate an Eject pause and confirm abort is rejected client-side,
	// never reaching the workflow (lastSignalQuery stays "abort" from the
	// burn pause above, not overwritten).
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", setPauseSignal,
		backup.CurrentPause{Kind: backup.PauseEject, AffectedTapes: []string{"TA0002L6"}, AwaitingExport: 1}))

	require.Eventually(t, func() bool {
		status, detail := getRunDetail(t, server.URL, runID)

		return status == http.StatusOK && detail.CurrentPause.Kind == string(backup.PauseEject)
	}, 30*time.Second, 250*time.Millisecond, "GET /api/runs/{runID} never reported the simulated eject pause")

	assert.Equal(t, http.StatusConflict, postAction(t, server.URL, runID, "abort"),
		"abort against an eject pause must be rejected: the Eject wait never listens for it")
	assert.Equal(t, "abort", queryLastSignal(t, temporalClient, ctx, runID),
		"the rejected eject-pause abort must never reach the workflow (lastSignal is unchanged from the burn-pause abort)")

	// Resume still applies to an eject pause.
	assert.Equal(t, http.StatusAccepted, postAction(t, server.URL, runID, "resume"),
		"resume against an eject pause must succeed")

	require.Eventually(t, func() bool {
		_, detail := getRunDetail(t, server.URL, runID)

		return detail.CurrentPause.Kind == ""
	}, 30*time.Second, 250*time.Millisecond, "the eject pause never cleared after resume")

	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil))
}

// postRun POSTs body to serverURL+"/api/runs" and returns the status code and
// decoded body.
func postRun(t *testing.T, serverURL string, body []byte) (int, runsapi.SubmitRunResponse) {
	t.Helper()

	resp, err := http.Post(serverURL+"/api/runs", "application/json", bytes.NewReader(body))
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var submitBody runsapi.SubmitRunResponse

	_ = json.Unmarshal(raw, &submitBody)

	return resp.StatusCode, submitBody
}

// TestSubmitRunAgainstRealTemporal exercises POST /api/runs against a real
// dev Temporal (make temporal-up): a valid submission actually starts the
// backup workflow (observable via GET /api/runs), a second submission while
// the first is still running is refused with 409 rather than silently
// queued or replacing the in-flight run (the singleton guard, SPEC §4.2),
// and once the first run closes a fresh submission succeeds again — the
// same submit/conflict semantics `tapectl run` has via
// TestConcurrentRunGuard (cmd/tapectl/main_integration_test.go), proving the
// pkg/runsubmit extraction did not let the two front doors drift.
func TestSubmitRunAgainstRealTemporal(t *testing.T) {
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

	// Best-effort safety net: unblock whatever backup run is current so a
	// failed assertion does not leave the singleton ID occupied for later
	// tests/runs.
	t.Cleanup(func() {
		_ = temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil)
	})

	// A production (non-dry-run) submit requires the deployment to own the
	// library devices (runsapi.requireDeviceOwnership); declare them so this
	// test's production submissions are accepted.
	handler := runsapi.New(temporalClient, runsapi.WithDeployConfig("/dev/sch0", []string{"/dev/nst0", "/dev/nst1"}, ""))
	server := httptest.NewServer(handler)

	defer server.Close()

	submitBody := []byte(`{"config": ` + validSubmitConfigJSON + `}`)

	// Start the first run. Tolerate a still-closing run from a previous test
	// by retrying until the singleton ID is free.
	var firstRunID string

	require.Eventually(t, func() bool {
		status, body := postRun(t, server.URL, submitBody)
		if status != http.StatusCreated {
			return false
		}

		firstRunID = body.RunID

		return firstRunID != ""
	}, 30*time.Second, 250*time.Millisecond, "first submission never started")

	// A second concurrent submission is refused with 409, not a 500 and not
	// a silent replace of the in-progress run.
	status, _ := postRun(t, server.URL, submitBody)
	assert.Equal(t, http.StatusConflict, status, "second concurrent submission must be refused with 409")

	// GET /api/runs/{runID} still reports the first run as RUNNING — the
	// refused second submission left it untouched.
	require.Eventually(t, func() bool {
		resp, err := http.Get(server.URL + "/api/runs/" + firstRunID)
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

		return detail.Status == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING.String()
	}, 30*time.Second, 250*time.Millisecond, "the first run must still be running after the refused second submission")

	// Finish the first run so the singleton ID frees up.
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil))

	// Once the first run has closed, a fresh submission succeeds again.
	var thirdRunID string

	require.Eventually(t, func() bool {
		status, body := postRun(t, server.URL, submitBody)
		thirdRunID = body.RunID

		return status == http.StatusCreated && body.RunID != "" && body.RunID != firstRunID
	}, 30*time.Second, 250*time.Millisecond, "a new submission did not succeed after the first run closed")

	// Unblock the run started by the post-close check and wait for it to
	// actually complete while the stub worker is still polling. This is the
	// last test in this file, so a leaked run here is never drained by a later
	// test in this package and would survive into the next package's binary.
	// The t.Cleanup/defer backupWorker.Stop safety nets above are not enough on
	// their own: cleanups run after the deferred Stop, so a signal sent there
	// would sit unprocessed and this test would leave the singleton "backup"
	// workflow Running for a later package's test to trip over.
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil))
	require.NoError(t, temporalClient.GetWorkflow(ctx, backup.WorkflowID, thirdRunID).Get(ctx, nil),
		"stub workflow must complete so the singleton workflow ID is free for later tests")
}
