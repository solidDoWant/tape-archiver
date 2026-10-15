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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/pkg/runsapi"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

const validSubmitConfigJSON = `{
  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
  "copies": 2,
  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
  "redundancy": {"targetPercentage": 10},
  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
  "delivery": {"webhookUrl": "https://discord.com/api/webhooks/123/abc"}
}`

const (
	// stubPhase is what the stub reports for LastCompletedPhaseQuery.
	stubPhase = "Verify"
	// stubFinishSignal unblocks the stub workflow so it completes.
	stubFinishSignal = "finish"
)

// stubBackupWorkflow answers LastCompletedPhaseQuery and runs until signalled.
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

func requireTemporalAddress(t *testing.T) {
	t.Helper()

	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; run via `make test-integration`")
	}
}

// isolateTemporalConfig keeps envconfig from reading a stray host temporal.toml.
func isolateTemporalConfig(t *testing.T) {
	t.Helper()

	emptyConfig := filepath.Join(t.TempDir(), "empty.toml")
	require.NoError(t, os.WriteFile(emptyConfig, nil, 0o600))
	t.Setenv("TEMPORAL_CONFIG_FILE", emptyConfig)
	t.Setenv("TEMPORAL_PROFILE", "")
}

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

	// TERMINATE_EXISTING: a pre-signal to a leftover run whose worker exited
	// is never processed, so the ID would never free up.
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

	// Visibility is eventually consistent.
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

	resp, err := http.Get(server.URL + "/api/runs/00000000-0000-0000-0000-000000000000")
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// The real server reports a malformed run ID as InvalidArgument.
	badResp, err := http.Get(server.URL + "/api/runs/not-a-uuid")
	require.NoError(t, err)

	defer func() { _ = badResp.Body.Close() }()

	assert.Equal(t, http.StatusBadRequest, badResp.StatusCode)

	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil))
}

// setPauseSignal is a test-only signal setting the stub's reported pause.
const setPauseSignal = "setPause"

// lastSignalQuery is a test-only query proving which signal was actually delivered.
const lastSignalQuery = "lastSignal"

// stubPausableBackupWorkflow adds a settable pause and the real resume/abort
// signals; it tests only the HTTP layer, not the real workflow's pause logic.
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

func getRunDetail(t *testing.T, serverURL, runID string) (int, runsapi.RunDetail) {
	t.Helper()

	resp, err := http.Get(serverURL + "/api/runs/" + runID)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	var detail runsapi.RunDetail

	_ = json.NewDecoder(resp.Body).Decode(&detail)

	return resp.StatusCode, detail
}

func postAction(t *testing.T, serverURL, runID, action string) int {
	t.Helper()

	resp, err := http.Post(serverURL+"/api/runs/"+runID+"/"+action, "application/json", nil)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode
}

// queryLastSignal bypasses HTTP so a test can prove a rejected request never
// reached the workflow.
func queryLastSignal(t *testing.T, temporalClient client.Client, ctx context.Context, runID string) string {
	t.Helper()

	value, err := temporalClient.QueryWorkflow(ctx, backup.WorkflowID, runID, lastSignalQuery)
	require.NoError(t, err)

	var signal string
	require.NoError(t, value.Get(&signal))

	return signal
}

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

	// GET can return 200 before the query handlers exist (it degrades on query
	// failure); the real phase proves all three handlers are registered.
	require.Eventually(t, func() bool {
		status, detail := getRunDetail(t, server.URL, runID)

		return status == http.StatusOK && detail.LastCompletedPhase == stubPhase
	}, 30*time.Second, 250*time.Millisecond, "GET /api/runs/{runID} never reported the stub workflow's query handlers as ready")

	assert.Equal(t, http.StatusConflict, postAction(t, server.URL, runID, "resume"),
		"resume against an unpaused run must be rejected")
	assert.Equal(t, http.StatusConflict, postAction(t, server.URL, runID, "abort"),
		"abort against an unpaused run must be rejected")
	assert.Empty(t, queryLastSignal(t, temporalClient, ctx, runID), "no signal must have reached the workflow yet")

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

	assert.Equal(t, http.StatusAccepted, postAction(t, server.URL, runID, "resume"),
		"resume against an eject pause must succeed")

	require.Eventually(t, func() bool {
		_, detail := getRunDetail(t, server.URL, runID)

		return detail.CurrentPause.Kind == ""
	}, 30*time.Second, 250*time.Millisecond, "the eject pause never cleared after resume")

	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil))
}

// postRun also returns the raw body so callers can put the server's refusal
// reason in their failure message (issue #338).
func postRun(t *testing.T, serverURL string, body []byte) (int, runsapi.SubmitRunResponse, string) {
	t.Helper()

	resp, err := http.Post(serverURL+"/api/runs", "application/json", bytes.NewReader(body))
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var submitBody runsapi.SubmitRunResponse

	_ = json.Unmarshal(raw, &submitBody)

	return resp.StatusCode, submitBody, strings.TrimSpace(string(raw))
}

// freeBackupSingleton frees the singleton ID for a FAIL-policy API submit,
// waiting out a closing run and terminating (with a log) a stranded one: an
// orphaned run can be abandoned RUNNING with no worker forever (issue #338).
func freeBackupSingleton(t *testing.T, ctx context.Context, temporalClient client.Client) {
	t.Helper()

	const grace = 15 * time.Second

	deadline := time.Now().Add(grace)

	for {
		description, err := temporalClient.DescribeWorkflowExecution(ctx, backup.WorkflowID, "")
		if err != nil {
			// Never used or aged out: free.
			return
		}

		info := description.GetWorkflowExecutionInfo()
		if info.GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
			return
		}

		if time.Now().After(deadline) {
			t.Logf("issue #338: singleton %q still RUNNING as run %s (started %s) after %s;"+
				" terminating it so this test can submit through the real API",
				backup.WorkflowID, info.GetExecution().GetRunId(), info.GetStartTime().AsTime(), grace)

			require.NoError(t, temporalClient.TerminateWorkflow(ctx, backup.WorkflowID, "",
				"tape-archiver integration tests: reclaiming the singleton workflow ID from a stranded run"),
				"terminating the stranded singleton run must succeed")

			return
		}

		time.Sleep(250 * time.Millisecond)
	}
}

// TestSubmitRunAgainstRealTemporal checks the singleton guard (SPEC §4.2)
// matches `tapectl run`'s TestConcurrentRunGuard.
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

	t.Cleanup(func() {
		_ = temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil)
	})

	// Production submits require deploy-owned devices and webhook (issue #304).
	handler := runsapi.New(temporalClient, runsapi.WithDeployConfig("/dev/sch0", []string{"/dev/nst0", "/dev/nst1"}, "https://discord.com/api/webhooks/deploy/deploy"))
	server := httptest.NewServer(handler)

	defer server.Close()

	submitBody := []byte(`{"config": ` + validSubmitConfigJSON + `}`)

	// Retrying a FAIL-policy submit against an incumbent cannot progress (issue #338).
	freeBackupSingleton(t, ctx, temporalClient)

	firstStatus, firstBody, firstRaw := postRun(t, server.URL, submitBody)
	require.Equal(t, http.StatusCreated, firstStatus, "first submission failed: %s", firstRaw)

	firstRunID := firstBody.RunID
	require.NotEmpty(t, firstRunID, "first submission returned no run ID: %s", firstRaw)

	status, _, raw := postRun(t, server.URL, submitBody)
	assert.Equal(t, http.StatusConflict, status, "second concurrent submission must be refused with 409: %s", raw)

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

	// Await closure so a refusal below is meaningful, not a drain race.
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil))
	require.NoError(t, temporalClient.GetWorkflow(ctx, backup.WorkflowID, firstRunID).Get(ctx, nil),
		"the first run must close so the singleton workflow ID frees up")

	thirdStatus, thirdBody, thirdRaw := postRun(t, server.URL, submitBody)
	require.Equal(t, http.StatusCreated, thirdStatus,
		"a new submission after the first run closed failed: %s", thirdRaw)

	thirdRunID := thirdBody.RunID
	require.NotEmpty(t, thirdRunID, "the new submission returned no run ID: %s", thirdRaw)
	assert.NotEqual(t, firstRunID, thirdRunID, "a fresh submission must start a new execution")

	// Complete it while the worker still polls: cleanups run after the
	// deferred Stop, so a leaked run would survive into the next package.
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil))
	require.NoError(t, temporalClient.GetWorkflow(ctx, backup.WorkflowID, thirdRunID).Get(ctx, nil),
		"stub workflow must complete so the singleton workflow ID is free for later tests")
}

func stubLoadWorkflow(ctx workflow.Context, dataQueue string) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           dataQueue,
		StartToCloseTimeout: 5 * time.Minute,
	})

	return workflow.ExecuteActivity(ctx, "Load").Get(ctx, nil)
}

func getLoadPhase(t *testing.T, serverURL, runID string) (runsapi.PhaseInfo, bool) {
	t.Helper()

	status, body := getJSON[runsapi.RunPhasesResponse](t, serverURL+"/api/runs/"+runID+"/phases")
	if status != http.StatusOK {
		return runsapi.PhaseInfo{}, false
	}

	for _, phase := range body.Phases {
		if phase.Name == backup.PhaseLoad {
			return phase, true
		}
	}

	return runsapi.PhaseInfo{}, false
}

// TestPhasesWaitingForWorkerAgainstRealTemporal covers issue #344.
func TestPhasesWaitingForWorkerAgainstRealTemporal(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	ctx := t.Context()

	temporalClient, shutdown, err := temporalclient.New(ctx, nil)
	require.NoError(t, err)

	defer shutdown()

	// Unique queues keep this run away from any other worker on the host.
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	controlQueue := "runsapi-344-control-" + suffix
	dataQueue := "runsapi-344-data-" + suffix

	controlWorker := worker.New(temporalClient, controlQueue, worker.Options{})
	controlWorker.RegisterWorkflowWithOptions(stubLoadWorkflow, workflow.RegisterOptions{Name: backup.WorkflowType})
	require.NoError(t, controlWorker.Start())

	defer controlWorker.Stop()

	run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       backup.WorkflowID,
		TaskQueue:                controlQueue,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_TERMINATE_EXISTING,
	}, backup.WorkflowType, dataQueue)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = temporalClient.TerminateWorkflow(context.WithoutCancel(ctx), backup.WorkflowID, run.GetRunID(), "test cleanup")
	})

	server := httptest.NewServer(runsapi.New(temporalClient))
	defer server.Close()

	require.Eventually(t, func() bool {
		load, ok := getLoadPhase(t, server.URL, run.GetRunID())

		return ok && load.Waiting != nil
	}, 30*time.Second, 250*time.Millisecond, "the unstarted Load never read as waiting")

	load, ok := getLoadPhase(t, server.URL, run.GetRunID())
	require.True(t, ok)
	require.NotNil(t, load.Waiting)
	assert.Equal(t, runsapi.PhaseActive, load.Status)
	assert.Equal(t, "Load", load.Waiting.Activity)
	assert.Equal(t, dataQueue, load.Waiting.TaskQueue)
	assert.False(t, load.Waiting.Stalled, "a wait inside the cold-start grace must not read as stalled")
	require.NotNil(t, load.Waiting.Pollers, "DescribeTaskQueue must report the poller count")
	assert.Equal(t, 0, *load.Waiting.Pollers)

	release := make(chan struct{})

	dataWorker := worker.New(temporalClient, dataQueue, worker.Options{})
	dataWorker.RegisterActivityWithOptions(func(ctx context.Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, activity.RegisterOptions{Name: "Load"})
	require.NoError(t, dataWorker.Start())

	defer dataWorker.Stop()
	defer close(release)

	require.Eventually(t, func() bool {
		load, ok := getLoadPhase(t, server.URL, run.GetRunID())

		return ok && load.Status == runsapi.PhaseActive && load.Waiting == nil
	}, 30*time.Second, 250*time.Millisecond, "the Load a worker picked up never read as plainly active")
}
