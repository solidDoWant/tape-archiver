package metrics_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/metrics"
)

// freeAddr returns a local TCP address with an available port.
func freeAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	return addr
}

// scrape performs a GET against the provider's /metrics endpoint and returns
// the response body. It uses t.Context so the request is cancelled if the test
// ends early.
func scrape(t *testing.T, addr string) string {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/metrics", nil)
	require.NoError(t, err)

	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	require.NoError(t, err)

	defer func() { require.NoError(t, response.Body.Close()) }()

	assert.Equal(t, http.StatusOK, response.StatusCode)

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	return string(body)
}

// shutdownCleanup registers a cleanup that shuts the provider down. It uses
// context.Background rather than t.Context because t.Context is already
// cancelled by the time cleanup functions run, which would abort the graceful
// shutdown.
func shutdownCleanup(t *testing.T, provider *metrics.Provider) {
	t.Helper()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = provider.Shutdown(ctx) //nolint:errcheck
	})
}

func TestPrometheusEndpoint_Enabled_ExposesRuntimeMetrics(t *testing.T) {
	addr := freeAddr(t)

	provider, err := metrics.New(metrics.WithMetricsAddr(addr))
	require.NoError(t, err)
	shutdownCleanup(t, provider)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/metrics", nil)
	require.NoError(t, err)

	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	require.NoError(t, err)

	defer func() { require.NoError(t, response.Body.Close()) }()

	assert.Equal(t, http.StatusOK, response.StatusCode)

	// The response must be in the Prometheus text exposition format.
	contentType := response.Header.Get("Content-Type")
	assert.True(t, strings.HasPrefix(contentType, "text/plain"),
		"metrics endpoint should return Prometheus text exposition format, got: %s", contentType)

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	// The standard Go runtime metrics must be present because New registers the
	// Go runtime collector on the registry.
	assert.Contains(t, string(body), "go_goroutines",
		"scrape should expose Go runtime metrics")
}

func TestPrometheusEndpoint_Disabled(t *testing.T) {
	provider, err := metrics.New()
	require.NoError(t, err)

	// PrometheusRegisterer must be nil so downstream consumers can detect
	// "metrics off" and short-circuit to a noop.
	require.Nil(t, provider.PrometheusRegisterer())

	// Shutdown and WaitForScrape must be safe no-ops.
	require.NoError(t, provider.Shutdown(t.Context()))
	require.NoError(t, provider.WaitForScrape(t.Context()))
}

func TestNewFromEnv_FallsBackToDefaultAddr(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("METRICS_ADDR", "")

	// NewFromEnv binds the supplied default when METRICS_ADDR is unset.
	provider, shutdown, err := metrics.NewFromEnv(addr)
	require.NoError(t, err)
	t.Cleanup(shutdown)

	require.NotNil(t, provider.PrometheusRegisterer())
	assert.Contains(t, scrape(t, addr), "go_goroutines")
}

func TestNewFromEnv_EmptyDefault_KeepsMetricsDisabled(t *testing.T) {
	t.Setenv("METRICS_ADDR", "")

	// Empty defaultAddr leaves the endpoint disabled when METRICS_ADDR is also
	// unset.
	provider, shutdown, err := metrics.NewFromEnv("")
	require.NoError(t, err)
	t.Cleanup(shutdown)

	require.Nil(t, provider.PrometheusRegisterer())
}

func TestNewFromEnv_MetricsAddr_StartsPrometheusEndpoint(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("METRICS_ADDR", addr)

	provider, shutdown, err := metrics.NewFromEnv("")
	require.NoError(t, err)
	t.Cleanup(shutdown)
	require.NotNil(t, provider)

	assert.Contains(t, scrape(t, addr), "go_goroutines")
}

func TestNewFromEnv_InvalidScrapeWaitTimeout_ReturnsError(t *testing.T) {
	t.Setenv("METRICS_ADDR", "")
	t.Setenv("METRICS_SCRAPE_WAIT_TIMEOUT", "not-a-duration")

	_, _, err := metrics.NewFromEnv("")
	require.Error(t, err)
}

func TestWaitForScrape_ReturnsWhenScrapeArrives(t *testing.T) {
	addr := freeAddr(t)

	// Generous configured timeout — the test must not hit it on the happy path.
	provider, err := metrics.New(
		metrics.WithMetricsAddr(addr),
		metrics.WithScrapeWaitTimeout(5*time.Second),
	)
	require.NoError(t, err)
	shutdownCleanup(t, provider)

	// Issue an initial synchronous scrape before starting WaitForScrape so the
	// notify-buffer is in a known pre-filled state. WaitForScrape's drain step
	// then deterministically clears that buffered tick, leaving the second
	// scrape (issued below) as the only event that can satisfy the wait.
	scrape(t, addr)

	waitErr := make(chan error, 1)

	go func() {
		waitErr <- provider.WaitForScrape(t.Context())
	}()

	scrape(t, addr)

	select {
	case err := <-waitErr:
		require.NoError(t, err, "WaitForScrape should return nil when a scrape arrives")
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForScrape did not return after a scrape was served")
	}
}

func TestWaitForScrape_HonorsTimeout(t *testing.T) {
	addr := freeAddr(t)

	provider, err := metrics.New(
		metrics.WithMetricsAddr(addr),
		metrics.WithScrapeWaitTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)
	shutdownCleanup(t, provider)

	start := time.Now()
	err = provider.WaitForScrape(t.Context())
	elapsed := time.Since(start)

	require.Error(t, err, "WaitForScrape should return an error when no scrape arrives before the timeout")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	// Sanity-bound the elapsed time so we know the timeout actually fired.
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond)
	assert.Less(t, elapsed, 2*time.Second, "WaitForScrape should return shortly after the configured timeout")
}

func TestWaitForScrape_NoOpWhenNoHTTPServer(t *testing.T) {
	// No metrics address configured → no Prometheus HTTP server, so
	// WaitForScrape must return nil immediately rather than blocking.
	provider, err := metrics.New(metrics.WithScrapeWaitTimeout(10 * time.Second))
	require.NoError(t, err)
	shutdownCleanup(t, provider)

	start := time.Now()
	err = provider.WaitForScrape(t.Context())
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Less(t, elapsed, 1*time.Second, "WaitForScrape should return immediately when no HTTP server is configured")
}

func TestWaitForScrape_DisabledByExplicitZero(t *testing.T) {
	addr := freeAddr(t)

	// Explicit zero must short-circuit the wait rather than fall back to the
	// default. This distinguishes "not provided" from "explicitly disabled".
	provider, err := metrics.New(
		metrics.WithMetricsAddr(addr),
		metrics.WithScrapeWaitTimeout(0),
	)
	require.NoError(t, err)
	shutdownCleanup(t, provider)

	start := time.Now()
	err = provider.WaitForScrape(t.Context())
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Less(t, elapsed, 500*time.Millisecond, "WaitForScrape should return immediately when timeout is explicitly zero")
}

func TestNewFromEnv_ScrapeWaitTimeout_DisabledViaZeroEnvVar(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("METRICS_ADDR", addr)
	t.Setenv("METRICS_SCRAPE_WAIT_TIMEOUT", "0s")

	provider, shutdown, err := metrics.NewFromEnv("")
	require.NoError(t, err)
	t.Cleanup(shutdown)

	start := time.Now()
	err = provider.WaitForScrape(t.Context())
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Less(t, elapsed, 500*time.Millisecond, "explicit 0s env value should disable the scrape-wait gate")
}
