//go:build integration

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
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

// sseFrame is one parsed "event: NAME\ndata: JSON\n\n" SSE frame.
type sseFrame struct {
	event string
	data  string
}

// readSSEFrames streams parsed frames; the channel closes on EOF or error.
func readSSEFrames(body io.Reader) <-chan sseFrame {
	frames := make(chan sseFrame, 16)

	go func() {
		defer close(frames)

		reader := bufio.NewReader(body)

		for {
			eventLine, err := reader.ReadString('\n')
			if err != nil {
				return
			}

			dataLine, err := reader.ReadString('\n')
			if err != nil {
				return
			}

			if _, err := reader.ReadString('\n'); err != nil {
				return
			}

			frames <- sseFrame{
				event: strings.TrimPrefix(strings.TrimSuffix(eventLine, "\n"), "event: "),
				data:  strings.TrimPrefix(strings.TrimSuffix(dataLine, "\n"), "data: "),
			}
		}
	}()

	return frames
}

func waitForFrame(t *testing.T, frames <-chan sseFrame, timeout time.Duration) sseFrame {
	t.Helper()

	select {
	case frame, ok := <-frames:
		require.True(t, ok, "SSE stream closed while waiting for a frame")

		return frame
	case <-time.After(timeout):
		t.Fatal("timed out waiting for an SSE frame")

		return sseFrame{}
	}
}

// TestSSERunEventsAgainstRealTemporal covers SSE through the full stack,
// including a long-lived event stream passing through webauth's Wrap.
func TestSSERunEventsAgainstRealTemporal(t *testing.T) {
	requireBuiltFrontend(t)

	webListenAddr := freeAddr(t)
	setupEnv(t, webListenAddr)

	ctx := t.Context()

	temporalClient, shutdown, err := temporalclient.New(ctx, nil)
	require.NoError(t, err)

	defer shutdown()

	backupWorker := worker.New(temporalClient, backup.TaskQueue, worker.Options{})
	backupWorker.RegisterWorkflowWithOptions(stubBackupWorkflow, workflow.RegisterOptions{Name: backup.WorkflowType})
	require.NoError(t, backupWorker.Start())

	defer backupWorker.Stop()

	workflowRun, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       backup.WorkflowID,
		TaskQueue:                backup.TaskQueue,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_TERMINATE_EXISTING,
	}, backup.WorkflowType, nil)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil)
	})

	webCtx, cancelWeb := context.WithCancel(ctx)
	defer cancelWeb()

	readyCh := make(chan string, 1)
	runErrCh := make(chan error, 1)

	go func() {
		runErrCh <- run(webCtx, envGetenv(webListenAddr), func(addr string) { readyCh <- addr })
	}()

	var addr string

	select {
	case addr = <-readyCh:
	case <-time.After(15 * time.Second):
		t.Fatal("web server never became ready")
	}

	eventsURL := "http://" + addr + "/api/events/runs/" + workflowRun.GetRunID()

	unauthResp, err := http.Get(eventsURL)
	require.NoError(t, err)

	_ = unauthResp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, unauthResp.StatusCode, "an unauthenticated SSE request must be rejected, not accepted")

	authenticatedClient := newAuthenticatedClient(t, addr)

	resp, err := authenticatedClient.Get(eventsURL)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	frames := readSSEFrames(resp.Body)

	first := waitForFrame(t, frames, 15*time.Second)
	assert.Equal(t, "update", first.event)

	var firstDetail runsapi.RunDetail

	require.NoError(t, json.Unmarshal([]byte(first.data), &firstDetail))
	assert.Equal(t, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING.String(), firstDetail.Status)
	assert.Equal(t, stubPhase, firstDetail.LastCompletedPhase)

	// Real 2s server-side polling, so this step can take a while.
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil))

	second := waitForFrame(t, frames, 30*time.Second)
	assert.Equal(t, "update", second.event)

	var secondDetail runsapi.RunDetail

	require.NoError(t, json.Unmarshal([]byte(second.data), &secondDetail))
	assert.Equal(t, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED.String(), secondDetail.Status)

	third := waitForFrame(t, frames, 5*time.Second)
	assert.Equal(t, "done", third.event)

	select {
	case _, ok := <-frames:
		assert.False(t, ok, "expected no further frames after the done event")
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not close after the terminal done event")
	}

	cancelWeb()

	select {
	case err := <-runErrCh:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after ctx cancellation")
	}
}

// TestShutdownWithOpenSSEConnectionIsBounded is issue #270's acceptance
// criterion: with an open SSE connection, shutdown exits cleanly and quickly.
func TestShutdownWithOpenSSEConnectionIsBounded(t *testing.T) {
	requireBuiltFrontend(t)

	webListenAddr := freeAddr(t)
	setupEnv(t, webListenAddr)

	// Exercise the production default (unset) rather than setupEnv's 0s.
	require.NoError(t, os.Unsetenv("METRICS_SCRAPE_WAIT_TIMEOUT"))

	ctx := t.Context()

	temporalClient, shutdown, err := temporalclient.New(ctx, nil)
	require.NoError(t, err)

	defer shutdown()

	backupWorker := worker.New(temporalClient, backup.TaskQueue, worker.Options{})
	backupWorker.RegisterWorkflowWithOptions(stubBackupWorkflow, workflow.RegisterOptions{Name: backup.WorkflowType})
	require.NoError(t, backupWorker.Start())

	defer backupWorker.Stop()

	workflowRun, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       backup.WorkflowID,
		TaskQueue:                backup.TaskQueue,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_TERMINATE_EXISTING,
	}, backup.WorkflowType, nil)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil)
	})

	webCtx, cancelWeb := context.WithCancel(ctx)
	defer cancelWeb()

	readyCh := make(chan string, 1)
	runErrCh := make(chan error, 1)

	go func() {
		runErrCh <- run(webCtx, envGetenv(webListenAddr), func(addr string) { readyCh <- addr })
	}()

	var addr string

	select {
	case addr = <-readyCh:
	case <-time.After(15 * time.Second):
		t.Fatal("web server never became ready")
	}

	authenticatedClient := newAuthenticatedClient(t, addr)

	resp, err := authenticatedClient.Get("http://" + addr + "/api/events/runs/" + workflowRun.GetRunID())
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	frames := readSSEFrames(resp.Body)

	first := waitForFrame(t, frames, 15*time.Second)
	require.Equal(t, "update", first.event)

	shutdownStart := time.Now()

	cancelWeb()

	select {
	case err := <-runErrCh:
		elapsed := time.Since(shutdownStart)

		require.NoError(t, err, "shutdown with an open SSE stream must complete cleanly, not by exhausting the drain deadline")
		assert.Less(t, elapsed, 5*time.Second, "shutdown must be bounded well under shutdownTimeout, took %s", elapsed)
	case <-time.After(9 * time.Second):
		// Just under the 10s shutdownTimeout.
		t.Fatal("run did not return within the bounded shutdown window after ctx cancellation")
	}

	select {
	case _, ok := <-frames:
		assert.False(t, ok, "expected the SSE stream to be closed by the draining server without further frames")
	case <-time.After(2 * time.Second):
		t.Fatal("SSE stream was not closed by the draining server")
	}

	// Complete the run while the worker still polls: t.Cleanup runs after
	// backupWorker.Stop, and a leaked run blocks later tests on the singleton
	// workflow ID.
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil))
	require.NoError(t, workflowRun.Get(ctx, nil), "stub workflow must complete so the singleton workflow ID is free for later tests")
}
