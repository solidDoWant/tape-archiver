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

func requireTemporalAddress(t *testing.T) {
	t.Helper()

	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; run via `make test-integration`")
	}
}

// isolateTemporalConfig keeps envconfig from reading a stray host
// ~/.config/temporalio/temporal.toml.
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

// TestRunWaitsForFinalScrapeBeforeShutdown: after draining, run() keeps /metrics
// up and returns only once a final scrape lands, well before the timeout.
func TestRunWaitsForFinalScrapeBeforeShutdown(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	metricsAddr := freeAddr(t)

	t.Setenv("ROLE", string(RoleControl))
	t.Setenv("METRICS_ADDR", metricsAddr)
	// The test must release run() via a scrape, not by hitting this bound.
	t.Setenv("METRICS_SCRAPE_WAIT_TIMEOUT", "30s")
	t.Setenv("HEALTH_ADDR", "")

	interruptCh := make(chan interface{})
	runErr := make(chan error, 1)

	go func() { runErr <- run(t.Context(), interruptCh) }()

	require.Eventually(t, func() bool {
		status, err := scrapeStatus(t, metricsAddr)

		return err == nil && status == http.StatusOK
	}, 30*time.Second, 250*time.Millisecond, "metrics endpoint did not come up")

	close(interruptCh)

	// run() must now block in the final-scrape wait.
	select {
	case err := <-runErr:
		t.Fatalf("run returned before any post-drain scrape; final-scrape wait not wired: err=%v", err)
	case <-time.After(500 * time.Millisecond):
	}

	// Scrape repeatedly: WaitForScrape discards ticks buffered before it began
	// blocking, so one pre-block scrape may not satisfy it.
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

// TestRunRegistersWorkerOnTaskQueue asserts each role's worker polls its queue.
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
			t.Setenv("HEALTH_ADDR", "")

			// Closing interruptCh drives the same drain a SIGTERM would.
			interruptCh := make(chan interface{})
			runErr := make(chan error, 1)

			go func() { runErr <- run(t.Context(), interruptCh) }()

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
