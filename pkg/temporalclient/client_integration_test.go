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

func TestNewHappyPath(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	c, shutdown, err := temporalclient.New(t.Context(), nil)
	require.NoError(t, err)

	defer shutdown()

	_, err = c.CheckHealth(t.Context(), &client.CheckHealthRequest{})
	assert.NoError(t, err)
}

func TestNewFailsWhenServerUnreachable(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	// Port 1 (tcpmux) is never bound. The error may come from Dial or the
	// lazy-dial CheckHealth, so assert on neither prefix.
	t.Setenv("TEMPORAL_ADDRESS", "127.0.0.1:1")

	_, shutdown, err := temporalclient.New(t.Context(), nil)
	defer shutdown()

	require.Error(t, err)
	// Matches both "dial Temporal:" and "temporal health check failed:".
	assert.Contains(t, strings.ToLower(err.Error()), "temporal")
}

func TestNewWithFileBackedAPIKey(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	keyFile := filepath.Join(t.TempDir(), "api-key")
	require.NoError(t, os.WriteFile(keyFile, []byte("test-key-value\n"), 0o600))

	t.Setenv("TEMPORAL_API_KEY", "file://"+keyFile)
	// An API key auto-enables TLS; the dev frontend is plaintext.
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

	// The credentials callback's read failure surfaces via New's CheckHealth.
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

	_, err = c.CheckHealth(t.Context(), &client.CheckHealthRequest{})
	require.NoError(t, err)

	// Flush synchronously; the deferred shutdown is idempotent.
	shutdown()

	body := scrapeMetrics(t, addr)

	// Narrower than `temporal_` so a stray HELP/TYPE line cannot match.
	assert.Contains(t, body, "temporal_request", "Temporal SDK metrics should be present on /metrics; got:\n"+body)
}

func freeAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := l.Addr().String()
	require.NoError(t, l.Close())

	return addr
}

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
