//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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
// same shape cmd/tapectl's and pkg/runsapi's integration tests use.
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

// freeAddr returns a local TCP address with an available port, mirroring
// cmd/worker's own integration test helper.
func freeAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	return addr
}

// setupEnv points cmd/web at a real dev Temporal, isolates its config
// profile, pins HEALTH_ADDR/METRICS_ADDR to fresh free ports so concurrent
// test binaries never collide on the package defaults, and configures a
// complete, working OIDC setup (pkg/webauth is now mandatory — see
// cmd/web/main.go's package doc comment) against an in-process fake OIDC
// provider (no real IdP is available in this sandbox). webListenAddr is the
// address the caller is about to pass run() as WEB_LISTEN_ADDRESS (reserved
// up front via freeAddr, since OIDC_REDIRECT_URL must be known before run()
// starts listening); pass any placeholder string for tests that never
// actually establish a session (e.g. TestRunListenError). Callers that DO
// need an authenticated request should use newAuthenticatedClient below,
// which logs in against the fake provider this function wires up.
// METRICS_SCRAPE_WAIT_TIMEOUT is disabled (0s): these tests never scrape
// /metrics, and run()'s default 60s post-drain scrape wait would otherwise
// make every test here block that long after cancellation before returning.
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

// envGetenv builds a run() getenv function that overrides WEB_LISTEN_ADDRESS
// to webListenAddr (the fixed address setupEnv already baked into
// OIDC_REDIRECT_URL) and otherwise falls through to the real process
// environment — the same delegation main() itself does in production
// (run(ctx, os.Getenv, nil)), so every t.Setenv call in setupEnv (TEMPORAL_*
// via isolateTemporalConfig, OIDC_*, WEB_SESSION_KEY, HEALTH_ADDR,
// METRICS_ADDR, ...) actually reaches run().
func envGetenv(webListenAddr string) func(string) string {
	return func(name string) string {
		if name == "WEB_LISTEN_ADDRESS" {
			return webListenAddr
		}

		return os.Getenv(name)
	}
}

// newAuthenticatedClient drives a full OIDC login against addr's own
// /auth/login route — which redirects through the fake OIDC provider
// setupEnv configured and back to /auth/callback — and returns an
// http.Client whose cookie jar now holds a valid session. Every subsequent
// request through this client reaches gated routes the same way a logged-in
// browser tab would.
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

// TestRunServesAndShutsDownGracefully drives the full run() entrypoint
// (Temporal client, health, metrics, OIDC, listen, serve,
// ctx-cancel-triggered shutdown) end to end against a real dev Temporal and
// a fake OIDC provider: an unauthenticated request to "/" is redirected to
// login (pkg/webauth gates it), a request authenticated via a real
// login -> callback round trip against the fake provider succeeds, the
// health server answers /healthz and /readyz (never gated — a separate,
// always-on port), and run() returns cleanly once ctx is cancelled.
func TestRunServesAndShutsDownGracefully(t *testing.T) {
	requireBuiltFrontend(t)

	webListenAddr := freeAddr(t)
	healthAddr, _ := setupEnv(t, webListenAddr)

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

	// A plain http.Get would transparently follow the whole redirect chain
	// (our own /auth/login -> the fake IdP auto-approving -> our
	// /auth/callback) and land back on "/" with 200, silently defeating this
	// check — the fake provider has no interactive login prompt to stop at,
	// unlike a real one. Disabling redirects isolates just the first hop.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	unauthResp, err := noRedirect.Get("http://" + addr + "/")
	require.NoError(t, err)
	assert.Equal(t, http.StatusFound, unauthResp.StatusCode, "an unauthenticated page request must be gated, not served directly")
	require.NoError(t, unauthResp.Body.Close())

	authenticatedClient := newAuthenticatedClient(t, addr)

	resp, err := authenticatedClient.Get("http://" + addr + "/")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "an authenticated request must be served once a session is established")
	require.NoError(t, resp.Body.Close())

	healthzResp, err := http.Get("http://" + healthAddr + "/healthz")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, healthzResp.StatusCode)
	require.NoError(t, healthzResp.Body.Close())

	readyzResp, err := http.Get("http://" + healthAddr + "/readyz")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, readyzResp.StatusCode, "readyz must be 200 while Temporal is reachable")
	require.NoError(t, readyzResp.Body.Close())

	cancel()

	select {
	case err := <-runErrCh:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after ctx cancellation")
	}
}

// TestRunListenError covers a bad listen address (a non-numeric port fails at
// net.Listen) returning an error rather than hanging or panicking, even once
// run() also depends on a real Temporal connection.
func TestRunListenError(t *testing.T) {
	requireBuiltFrontend(t)

	const badAddr = "127.0.0.1:not-a-port"

	// OIDC discovery against the fake provider still succeeds here (it runs
	// before net.Listen in run()) — only the listen address itself is bad.
	// OIDC_REDIRECT_URL is never dialed, so badAddr baked into it is fine.
	setupEnv(t, badAddr)

	err := run(t.Context(), envGetenv(badAddr), nil)
	require.Error(t, err)
}

// TestRunAPIRoutesAgainstRealTemporal proves the full wiring end to end: a
// backup run submitted directly via the Temporal client is visible through
// cmd/web's actual GET /api/runs and GET /api/runs/{runID}, served by run()
// itself (not pkg/runsapi in isolation — pkg/runsapi's own integration test
// already covers that layer). This is the automated equivalent of manually
// curling bin/web against `make temporal-up`.
func TestRunAPIRoutesAgainstRealTemporal(t *testing.T) {
	requireBuiltFrontend(t)

	webListenAddr := freeAddr(t)
	setupEnv(t, webListenAddr)

	ctx := t.Context()

	// A second, independent Temporal client submits/signals the stub
	// workflow directly — run() builds its own client internally.
	temporalClient, shutdown, err := temporalclient.New(ctx, nil)
	require.NoError(t, err)

	defer shutdown()

	backupWorker := worker.New(temporalClient, backup.TaskQueue, worker.Options{})
	backupWorker.RegisterWorkflowWithOptions(stubBackupWorkflow, workflow.RegisterOptions{Name: backup.WorkflowType})
	require.NoError(t, backupWorker.Start())

	defer backupWorker.Stop()

	// TERMINATE_EXISTING guarantees a clean fresh execution starts in one
	// call, atomically replacing any leftover run from a previous failed test
	// — a best-effort pre-signal (send "finish", then retry ExecuteWorkflow
	// until the ID frees up) is racy when the leftover run's own worker
	// process has already exited: the signal is durably recorded but never
	// processed (no poller left to receive it), so the ID never actually
	// frees up on its own.
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

	// /api/runs is gated (pkg/webauth) same as every other route, so these
	// calls need an authenticated client, not a bare http.Get.
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
