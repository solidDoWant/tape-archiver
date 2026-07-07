//go:build integration

package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"

	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
)

// requireTemporalAddress skips the test unless a Temporal server is reachable
// at TEMPORAL_ADDRESS. The make test-integration target arranges this via
// temporal-up before invoking the suite.
func requireTemporalAddress(t *testing.T) {
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

// freeAddr returns a local TCP address with an available port.
func freeAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	return addr
}

// scrapeStatus performs a GET against the given /metrics address and reports
// whether it returned HTTP 200, draining the body. A connection error yields
// (0, err) so callers can distinguish "endpoint down" from "scraped".
func scrapeStatus(t *testing.T, addr string) (int, error) {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/metrics", nil)
	require.NoError(t, err)

	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		return 0, err
	}

	defer func() { _ = response.Body.Close() }() //nolint:errcheck

	_, _ = io.Copy(io.Discard, response.Body) //nolint:errcheck

	return response.StatusCode, nil
}

// TestRunWaitsForFinalScrapeBeforeShutdown covers AC3: with the metrics endpoint
// enabled and a positive scrape-wait timeout, run() keeps /metrics scrapeable
// after the worker drains and only returns once Prometheus performs a final
// scrape — well before the (generous) timeout would elapse. This proves the
// documented final-scrape wait is actually wired into run().
func TestRunWaitsForFinalScrapeBeforeShutdown(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	metricsAddr := freeAddr(t)

	t.Setenv("ROLE", string(RoleControl))
	t.Setenv("METRICS_ADDR", metricsAddr)
	// A generous scrape-wait timeout: the test must release run() via a scrape,
	// not by hitting this bound. If the wait were not wired in, run() would
	// return immediately after drain instead of blocking for the scrape.
	t.Setenv("METRICS_SCRAPE_WAIT_TIMEOUT", "30s")
	// Disable the health server so it does not contend for a fixed port.
	t.Setenv("HEALTH_ADDR", "")

	interruptCh := make(chan interface{})
	runErr := make(chan error, 1)

	go func() { runErr <- run(t.Context(), interruptCh) }()

	// Wait until the worker is registered and /metrics is serving.
	require.Eventually(t, func() bool {
		status, err := scrapeStatus(t, metricsAddr)

		return err == nil && status == http.StatusOK
	}, 30*time.Second, 250*time.Millisecond, "metrics endpoint did not come up")

	// Trigger graceful drain. After the worker drains, run() must block in the
	// final-scrape wait with /metrics still up rather than returning.
	close(interruptCh)

	// The worker should not have returned yet: it is waiting for a final scrape.
	// (A brief settle avoids racing the drain; run() may still be mid-drain,
	// which is also "not returned".)
	select {
	case err := <-runErr:
		t.Fatalf("run returned before any post-drain scrape; final-scrape wait not wired: err=%v", err)
	case <-time.After(500 * time.Millisecond):
	}

	// Repeatedly scrape /metrics. WaitForScrape drains any tick buffered before
	// it began blocking, so a single pre-block scrape may not satisfy it; the
	// loop guarantees a scrape lands after run() enters the wait. Each scrape
	// returning 200 also proves the endpoint stays up through the shutdown
	// window. run() must then return promptly, far under the 30s timeout.
	deadline := time.Now().Add(10 * time.Second)

	for {
		status, err := scrapeStatus(t, metricsAddr)
		require.NoError(t, err, "metrics endpoint must stay scrapeable during the final-scrape wait")
		require.Equal(t, http.StatusOK, status)

		select {
		case err := <-runErr:
			require.NoError(t, err)

			return
		case <-time.After(100 * time.Millisecond):
		}

		if time.Now().After(deadline) {
			t.Fatal("run did not return after a post-drain scrape; final-scrape wait did not release")
		}
	}
}

// TestRunRegistersWorkerOnTaskQueue starts the worker for each role and asserts
// that a poller appears on the matching task queue — the observable behavior
// behind "the Temporal UI shows the worker as registered on the correct task
// queue". Disabling the metrics endpoint keeps the two role workers from
// racing for the same listen address.
func TestRunRegistersWorkerOnTaskQueue(t *testing.T) {
	tests := []struct {
		name  string
		role  Role
		queue string
	}{
		{name: "control role polls control queue", role: RoleControl, queue: controlTaskQueue},
		{name: "data role polls data queue", role: RoleData, queue: dataTaskQueue},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requireTemporalAddress(t)
			isolateTemporalConfig(t)

			t.Setenv("ROLE", string(test.role))
			t.Setenv("METRICS_ADDR", "")
			// Disable the health server too, so sequential subtests do not
			// contend for the default health port.
			t.Setenv("HEALTH_ADDR", "")

			// interruptCh stands in for worker.InterruptCh: closing it drives
			// the same run-phase drain a SIGTERM would, without signalling the
			// test process.
			interruptCh := make(chan interface{})
			runErr := make(chan error, 1)

			go func() { runErr <- run(t.Context(), interruptCh) }()

			// A separate client probes the task queue the worker should be
			// polling. New performs its own startup health check.
			probe, shutdown, err := temporalclient.New(t.Context(), nil)
			require.NoError(t, err)

			defer shutdown()

			require.Eventually(t, func() bool {
				resp, err := probe.DescribeTaskQueue(t.Context(), test.queue, enumspb.TASK_QUEUE_TYPE_WORKFLOW)
				if err != nil {
					return false
				}

				return len(resp.GetPollers()) > 0
			}, 30*time.Second, 250*time.Millisecond, "worker did not register a poller on the %q task queue", test.queue)

			// Trigger graceful drain and confirm run returns without error.
			close(interruptCh)

			select {
			case err := <-runErr:
				assert.NoError(t, err)
			case <-time.After(30 * time.Second):
				t.Fatal("run did not return after interrupt")
			}
		})
	}
}
