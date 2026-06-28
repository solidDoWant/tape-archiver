//go:build integration

package temporalclient_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/pkg/metrics"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
)

// requireTemporalAddress ensures the test runner has a Temporal server reachable
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
// the host. Each test calling this gets its own deterministic baseline.
func isolateTemporalConfig(t *testing.T) {
	t.Helper()

	emptyConfig := filepath.Join(t.TempDir(), "empty.toml")

	require.NoError(t, os.WriteFile(emptyConfig, nil, 0o600))
	t.Setenv("TEMPORAL_CONFIG_FILE", emptyConfig)
	t.Setenv("TEMPORAL_PROFILE", "")
}

func TestNewHappyPath(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	c, shutdown, err := temporalclient.New(t.Context(), nil)
	require.NoError(t, err)

	defer shutdown()

	// Sanity-check the client beyond New's internal CheckHealth: a fresh
	// CheckHealth round-trip from the test confirms the returned client is
	// usable for outbound RPCs, not just the one New issued.
	_, err = c.CheckHealth(t.Context(), &client.CheckHealthRequest{})
	assert.NoError(t, err)
}

func TestNewFailsWhenServerUnreachable(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	// Port 1 is reserved (tcpmux) and not bound on dev workstations or in CI.
	// Depending on the underlying gRPC behaviour, the failure surfaces either
	// at client.Dial (immediate connection refused) or at the startup
	// CheckHealth (lazy dial that the call exercises) — both are legitimate
	// outcomes for the user-visible contract that New returns an error when
	// the frontend is unreachable, so we assert on neither prefix specifically.
	t.Setenv("TEMPORAL_ADDRESS", "127.0.0.1:1")

	_, shutdown, err := temporalclient.New(t.Context(), nil)
	defer shutdown()

	require.Error(t, err)
	// Accept either error path: "dial Temporal:" wraps an immediate connection
	// failure, "temporal health check failed:" wraps a CheckHealth timeout
	// after a lazy gRPC dial. A case-insensitive substring matches both.
	assert.Contains(t, strings.ToLower(err.Error()), "temporal")
}

func TestNewWithFileBackedAPIKey(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	keyFile := filepath.Join(t.TempDir(), "api-key")
	require.NoError(t, os.WriteFile(keyFile, []byte("test-key-value\n"), 0o600))

	t.Setenv("TEMPORAL_API_KEY", "file://"+keyFile)
	// Setting an API key auto-enables TLS in the SDK; force it back off so we
	// can dial the plaintext dev frontend. The header still rides on the gRPC
	// call — the dev server just doesn't enforce it.
	t.Setenv("TEMPORAL_TLS", "false")

	c, shutdown, err := temporalclient.New(t.Context(), nil)
	require.NoError(t, err)

	defer shutdown()

	_, err = c.CheckHealth(t.Context(), &client.CheckHealthRequest{})
	assert.NoError(t, err)
}

func TestNewFailsWhenAPIKeyFileMissing(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	missing := filepath.Join(t.TempDir(), "does-not-exist")

	t.Setenv("TEMPORAL_API_KEY", "file://"+missing)
	t.Setenv("TEMPORAL_TLS", "false")

	// The misconfiguration surfaces during New's CheckHealth: the dynamic-
	// credentials callback is invoked by the gRPC interceptor, fails its
	// os.ReadFile, and that error propagates back through CheckHealth.
	_, shutdown, err := temporalclient.New(t.Context(), nil)
	defer shutdown()

	require.Error(t, err)
	assert.ErrorContains(t, err, "read api key file")
}

func TestNewFailsWhenAPIKeyFilePathRelative(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	t.Setenv("TEMPORAL_API_KEY", "file://relative/path")
	t.Setenv("TEMPORAL_TLS", "false")

	_, shutdown, err := temporalclient.New(t.Context(), nil)
	defer shutdown()

	require.Error(t, err)
	assert.ErrorContains(t, err, "must be absolute")
}

// TestNewEmitsTemporalSDKMetrics verifies the end-to-end path: when New is
// wired with a Prometheus registerer that backs the /metrics endpoint,
// SDK-internal instruments (e.g. temporal_request_total) appear on that
// endpoint after exercising the client.
func TestNewEmitsTemporalSDKMetrics(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	addr := freeAddr(t)

	provider, err := metrics.New(metrics.WithMetricsAddr(addr))
	require.NoError(t, err)

	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = provider.Shutdown(shutdownCtx)
	}()

	c, shutdown, err := temporalclient.New(t.Context(), provider.PrometheusRegisterer())
	require.NoError(t, err)

	defer shutdown()

	// Issue an extra RPC beyond New's startup CheckHealth so the SDK has
	// definitely populated its request metrics at least once.
	_, err = c.CheckHealth(t.Context(), &client.CheckHealthRequest{})
	require.NoError(t, err)

	// Force a synchronous flush of the tally scope so the SDK metrics are
	// guaranteed to be on the registry before we scrape (instead of waiting
	// up to one report interval). The deferred shutdown is idempotent.
	shutdown()

	body := scrapeMetrics(t, addr)

	// The SDK emits `temporal_request_*` on every outbound gRPC call. Match
	// on this prefix specifically (rather than the broader `temporal_`) so
	// the assertion can't be satisfied by a stray HELP/TYPE comment line.
	assert.Contains(t, body, "temporal_request", "Temporal SDK metrics should be present on /metrics; got:\n"+body)
}

// freeAddr returns a local TCP address with an available port.
func freeAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := l.Addr().String()
	require.NoError(t, l.Close())

	return addr
}

// scrapeMetrics fetches the /metrics endpoint and returns the raw body. Any
// non-200 response or read error fails the test.
func scrapeMetrics(t *testing.T, addr string) string {
	t.Helper()

	httpClient := &http.Client{Timeout: 5 * time.Second}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/metrics", nil)
	require.NoError(t, err)

	resp, err := httpClient.Do(req)
	require.NoError(t, err)

	defer func() {
		require.NoError(t, resp.Body.Close())
	}()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return string(body)
}
