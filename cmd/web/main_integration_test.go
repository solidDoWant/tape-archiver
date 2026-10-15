//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
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

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/runsapi"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

const validRunConfigJSON = `{
  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
  "copies": 2,
  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
  "redundancy": {"targetPercentage": 10},
  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
  "delivery": {"webhookUrl": "https://discord.com/api/webhooks/123/abc"}
}`

const (
	// stubPhase stands in for a real phase name.
	stubPhase = "Verify"
	// stubFinishSignal unblocks the stub workflow so it completes.
	stubFinishSignal = "finish"
)

// stubBackupWorkflow answers the phase query and runs until signalled.
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

// isolateTemporalConfig keeps envconfig from reading the host's temporal.toml.
func isolateTemporalConfig(t *testing.T) {
	t.Helper()

	emptyConfig := filepath.Join(t.TempDir(), "empty.toml")
	require.NoError(t, os.WriteFile(emptyConfig, nil, 0o600))
	t.Setenv("TEMPORAL_CONFIG_FILE", emptyConfig)
	t.Setenv("TEMPORAL_PROFILE", "")
}

func freeAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	return addr
}

// setupEnv configures run() against a dev Temporal and a fake OIDC provider.
// webListenAddr must be reserved up front because OIDC_REDIRECT_URL embeds it.
func setupEnv(t *testing.T, webListenAddr string) (healthAddr, metricsAddr string) {
	t.Helper()

	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	healthAddr = freeAddr(t)
	metricsAddr = freeAddr(t)

	t.Setenv("HEALTH_ADDR", healthAddr)
	t.Setenv("METRICS_ADDR", metricsAddr)
	t.Setenv("METRICS_SCRAPE_WAIT_TIMEOUT", "0s")

	idp := testutil.NewFakeOIDCProvider(t, "test-client", "test-secret")

	sessionKey := make([]byte, 32)
	_, err := rand.Read(sessionKey)
	require.NoError(t, err)

	t.Setenv("OIDC_ISSUER_URL", idp.Server.URL)
	t.Setenv("OIDC_CLIENT_ID", idp.ClientID)
	t.Setenv("OIDC_CLIENT_SECRET", idp.ClientSecret)
	t.Setenv("OIDC_REDIRECT_URL", "http://"+webListenAddr+"/auth/callback")
	t.Setenv("WEB_SESSION_KEY", base64.StdEncoding.EncodeToString(sessionKey))

	return healthAddr, metricsAddr
}

func envGetenv(webListenAddr string) func(string) string {
	return func(name string) string {
		if name == "WEB_LISTEN_ADDRESS" {
			return webListenAddr
		}

		return os.Getenv(name)
	}
}

// newAuthenticatedClient logs in via the fake provider and returns a client
// holding a valid session cookie.
func newAuthenticatedClient(t *testing.T, addr string) *http.Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	client := &http.Client{Jar: jar}

	resp, err := client.Get("http://" + addr + "/auth/login")
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode, "OIDC login against the fake provider did not complete")

	return client
}

func TestRunServesAndShutsDownGracefully(t *testing.T) {
	requireBuiltFrontend(t)

	webListenAddr := freeAddr(t)
	healthAddr, metricsAddr := setupEnv(t, webListenAddr)

	ctx, cancel := context.WithCancel(t.Context())

	readyCh := make(chan string, 1)
	runErrCh := make(chan error, 1)

	go func() {
		runErrCh <- run(ctx, envGetenv(webListenAddr), func(addr string) { readyCh <- addr })
	}()

	var addr string

	select {
	case addr = <-readyCh:
	case <-time.After(15 * time.Second):
		t.Fatal("server never became ready")
	}

	// Since #284, "/" serves the SPA (which renders its own login); the real
	// gate is /api/*.
	unauthPage, err := http.Get("http://" + addr + "/")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, unauthPage.StatusCode, "an unauthenticated page request must be served the SPA, which renders its own login")
	require.NoError(t, unauthPage.Body.Close())

	unauthMe, err := http.Get("http://" + addr + "/api/me")
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, unauthMe.StatusCode, "the real gate is /api/*: an unauthenticated /api/me must be rejected")
	require.NoError(t, unauthMe.Body.Close())

	authenticatedClient := newAuthenticatedClient(t, addr)

	resp, err := authenticatedClient.Get("http://" + addr + "/api/me")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "once a session is established /api/me must report it, not 401")
	require.NoError(t, resp.Body.Close())

	healthzResp, err := http.Get("http://" + healthAddr + "/healthz")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, healthzResp.StatusCode)
	require.NoError(t, healthzResp.Body.Close())

	readyzResp, err := http.Get("http://" + healthAddr + "/readyz")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, readyzResp.StatusCode, "readyz must be 200 while Temporal is reachable")
	require.NoError(t, readyzResp.Body.Close())

	metricsResp, err := http.Get("http://" + metricsAddr + "/metrics")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, metricsResp.StatusCode, "Prometheus cannot perform an OIDC login, so /metrics must stay reachable unauthenticated")
	require.NoError(t, metricsResp.Body.Close())

	cancel()

	select {
	case err := <-runErrCh:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after ctx cancellation")
	}
}

func TestRunListenError(t *testing.T) {
	requireBuiltFrontend(t)

	const badAddr = "127.0.0.1:not-a-port"

	setupEnv(t, badAddr)

	err := run(t.Context(), envGetenv(badAddr), nil)
	require.Error(t, err)
}

func TestRunAPIRoutesAgainstRealTemporal(t *testing.T) {
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

	// TERMINATE_EXISTING atomically replaces a leftover run; signalling it to
	// finish is racy if its worker has already exited.
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

	require.Eventually(t, func() bool {
		resp, err := authenticatedClient.Get("http://" + addr + "/api/runs")
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
			if summary.RunID == workflowRun.GetRunID() {
				return true
			}
		}

		return false
	}, 30*time.Second, 250*time.Millisecond, "GET /api/runs never listed the submitted run via cmd/web's own server")

	require.Eventually(t, func() bool {
		resp, err := authenticatedClient.Get("http://" + addr + "/api/runs/" + workflowRun.GetRunID())
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
	}, 30*time.Second, 250*time.Millisecond, "GET /api/runs/{runID} never reported the stub's phase via cmd/web's own server")

	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil))

	cancelWeb()

	select {
	case err := <-runErrCh:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after ctx cancellation")
	}
}

// TestSubmitRunAgainstRealTemporal covers POST /api/runs through the
// authenticated stack, which pkg/runsapi's own tests bypass.
func TestSubmitRunAgainstRealTemporal(t *testing.T) {
	requireBuiltFrontend(t)

	webListenAddr := freeAddr(t)
	setupEnv(t, webListenAddr)

	// A production submit requires deploy-owned devices and webhook (#304).
	t.Setenv("LIBRARY_CHANGER", "/dev/sch0")
	t.Setenv("LIBRARY_DRIVES", "/dev/nst0,/dev/nst1")
	t.Setenv("DELIVERY_WEBHOOK_URL", "https://discord.com/api/webhooks/deploy/deploy")

	ctx := t.Context()

	temporalClient, shutdown, err := temporalclient.New(ctx, nil)
	require.NoError(t, err)

	defer shutdown()

	backupWorker := worker.New(temporalClient, backup.TaskQueue, worker.Options{})
	backupWorker.RegisterWorkflowWithOptions(stubBackupWorkflow, workflow.RegisterOptions{Name: backup.WorkflowType})
	require.NoError(t, backupWorker.Start())

	defer backupWorker.Stop()

	// Reset, then wait for close: POST /api/runs uses a FAIL conflict policy.
	resetRun, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       backup.WorkflowID,
		TaskQueue:                backup.TaskQueue,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_TERMINATE_EXISTING,
	}, backup.WorkflowType, nil)
	require.NoError(t, err)
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil))
	require.NoError(t, resetRun.Get(ctx, nil))

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

	requestBody, err := json.Marshal(runsapi.SubmitRunRequest{Config: json.RawMessage(validRunConfigJSON)})
	require.NoError(t, err)

	resp, err := authenticatedClient.Post("http://"+addr+"/api/runs", "application/json", bytes.NewReader(requestBody))
	require.NoError(t, err)

	responseBody, _ := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusCreated, resp.StatusCode, "POST /api/runs through cmd/web's authenticated layer failed: %s", responseBody)

	var submitted runsapi.SubmitRunResponse
	require.NoError(t, json.Unmarshal(responseBody, &submitted))
	assert.Equal(t, backup.WorkflowID, submitted.WorkflowID)
	assert.NotEmpty(t, submitted.RunID)

	t.Cleanup(func() {
		_ = temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil)
	})

	require.Eventually(t, func() bool {
		resp, err := authenticatedClient.Get("http://" + addr + "/api/runs/" + submitted.RunID)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()

		return resp.StatusCode == http.StatusOK
	}, 30*time.Second, 250*time.Millisecond, "the run submitted via POST /api/runs never became visible via GET /api/runs/{runID}")

	unauthResp, err := http.Post("http://"+addr+"/api/runs", "application/json", bytes.NewReader(requestBody))
	require.NoError(t, err)

	_ = unauthResp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, unauthResp.StatusCode, "an unauthenticated POST /api/runs must be rejected, not accepted")

	// Complete the run while the stub worker still polls: t.Cleanup runs after
	// backupWorker.Stop, too late, and a leaked run blocks later packages'
	// tests on the singleton workflow ID (PR #300).
	require.NoError(t, temporalClient.SignalWorkflow(ctx, backup.WorkflowID, "", stubFinishSignal, nil))
	require.NoError(t, temporalClient.GetWorkflow(ctx, backup.WorkflowID, submitted.RunID).Get(ctx, nil),
		"stub workflow must complete so the singleton workflow ID is free for later tests")

	cancelWeb()

	select {
	case err := <-runErrCh:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after ctx cancellation")
	}
}
