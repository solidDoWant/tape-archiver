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

// sseFrame is one parsed "event: NAME\ndata: JSON\n\n" Server-Sent Event
// frame, mirroring pkg/runsapi's own test helper of the same shape — kept as
// a small local duplicate rather than exported cross-package machinery,
// since this file is the only place outside pkg/runsapi that needs it.
type sseFrame struct {
	event string
	data  string
}

// readSSEFrames parses body as a stream of SSE frames on a background
// goroutine, delivering each one on the returned channel, which closes once
// body hits EOF or another read error — in particular, once the server
// closes the connection after its final "done" event.
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

// TestSSERunEventsAgainstRealTemporal proves GET /api/events/runs/{runID}
// works end to end through cmd/web's real, fully-composed stack — Temporal
// wiring, pkg/webauth's session gate, pkg/webserver, and pkg/runsapi's SSE
// handler together — not just pkg/runsapi's own unit tests, which never
// drive a real Temporal connection or the auth middleware wrapper. This is
// the specific combination the epic's tracking notes flagged as untested so
// far: a long-lived text/event-stream response passing through
// pkg/webauth.Authenticator.Wrap. It proves: an unauthenticated request is
// rejected (401, same as any other gated /api/ route — the SSE route is not
// a second, unauthenticated path), an authenticated request receives a live
// "update" event reflecting the real running workflow's status/phase
// (answered by the real backup.LastCompletedPhaseQuery handler, not a
// fixture), and once the workflow actually completes the stream delivers a
// final "update" + "done" pair and the server closes the connection on its
// own.
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

	// An unauthenticated request must be rejected the same way any other
	// gated /api/ route is — proving the SSE route is mounted through
	// pkg/webauth's real gate, not a second, unauthenticated path.
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

	// Finish the workflow; the stream must observe the resulting status
	// change and deliver a final "update" + "done" pair, then the server
	// closes the connection on its own — this is real server-side polling
	// against the production 2-second poll interval, so this step alone can
	// take a little while.
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
// criterion, verbatim: Given cmd/web is running and serving at least one
// open SSE connection, When it receives SIGTERM (its NotifyContext being
// cancelled — the identical code path), Then it exits within a small bound,
// cleanly. Before the fix this took the full 10s shutdownTimeout AND
// returned a "shutdown: context deadline exceeded" error (an open SSE
// response never goes idle, so srv.Shutdown could only ever give up), and
// with METRICS_SCRAPE_WAIT_TIMEOUT unset the no-SSE path additionally hung
// on a 60s final-scrape wait no scraper was going to satisfy. After the fix
// the drain context ends the stream immediately and cmd/web's scrape-wait
// default is 0, so both failure modes are covered by the single bound
// asserted here.
func TestShutdownWithOpenSSEConnectionIsBounded(t *testing.T) {
	requireBuiltFrontend(t)

	webListenAddr := freeAddr(t)
	setupEnv(t, webListenAddr)

	// setupEnv pins METRICS_SCRAPE_WAIT_TIMEOUT=0s so other tests never sit
	// through run()'s scrape wait; this test is specifically about cmd/web's
	// own built-in shutdown bound, so exercise the real production default
	// (env unset — cmd/web must skip the wait on its own). setupEnv's
	// t.Setenv cleanup restores the variable when this test ends.
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

	// The stream is live (initial snapshot delivered) and the run is still
	// RUNNING, so nothing but shutdown will end it server-side.
	first := waitForFrame(t, frames, 15*time.Second)
	require.Equal(t, "update", first.event)

	// Deliver the shutdown signal and time run()'s exit.
	shutdownStart := time.Now()

	cancelWeb()

	select {
	case err := <-runErrCh:
		elapsed := time.Since(shutdownStart)

		require.NoError(t, err, "shutdown with an open SSE stream must complete cleanly, not by exhausting the drain deadline")
		assert.Less(t, elapsed, 5*time.Second, "shutdown must be bounded well under shutdownTimeout, took %s", elapsed)
	case <-time.After(9 * time.Second):
		// Just under shutdownTimeout: reaching the 10s drain deadline at all
		// is the failure this test exists to catch.
		t.Fatal("run did not return within the bounded shutdown window after ctx cancellation")
	}

	// The server must have ended the SSE stream itself as part of draining.
	select {
	case _, ok := <-frames:
		assert.False(t, ok, "expected the SSE stream to be closed by the draining server without further frames")
	case <-time.After(2 * time.Second):
		t.Fatal("SSE stream was not closed by the draining server")
	}

	// Finish the stub workflow while its worker is still running (the
	// deferred backupWorker.Stop has not run yet) and wait for it to actually
	// complete. The finish-signal t.Cleanup above is not enough on its own:
	// cleanups run after the deferred worker Stop, so the signal would sit
	// unprocessed forever and this test would leave the singleton "backup"
	// workflow Running — which blocks any later test that submits a run
	// against the same dev Temporal (pkg/runsapi's dry-run submission
	// integration tests fail with "dry-run submission never started").
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil))
	require.NoError(t, workflowRun.Get(ctx, nil), "stub workflow must complete so the singleton workflow ID is free for later tests")
}
