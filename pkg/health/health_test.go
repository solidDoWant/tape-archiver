package health_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/health"
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

// get issues a GET against the given health path and returns the status code.
// It uses t.Context so the request is cancelled if the test ends early.
func get(t *testing.T, addr, path string) int {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+path, nil)
	require.NoError(t, err)

	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	require.NoError(t, err)

	defer func() { require.NoError(t, response.Body.Close()) }()

	return response.StatusCode
}

// shutdownCleanup registers a cleanup that shuts the server down. It uses
// context.Background rather than t.Context because t.Context is already
// cancelled by the time cleanup functions run, which would abort the graceful
// shutdown.
func shutdownCleanup(t *testing.T, server *health.Server) {
	t.Helper()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = server.Shutdown(ctx) //nolint:errcheck
	})
}

func TestLiveness_AlwaysOK(t *testing.T) {
	tests := []struct {
		name      string
		readiness health.ReadinessFunc
	}{
		{name: "no readiness check", readiness: nil},
		{
			name:      "readiness failing",
			readiness: func(context.Context) error { return errors.New("temporal down") },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			addr := freeAddr(t)

			server, err := health.New(
				health.WithHealthAddr(addr),
				health.WithReadinessCheck(test.readiness),
			)
			require.NoError(t, err)
			shutdownCleanup(t, server)

			// Liveness must be 200 regardless of the readiness check's result:
			// a failing dependency must never make the process look dead.
			assert.Equal(t, http.StatusOK, get(t, addr, "/healthz"))
		})
	}
}

func TestReadiness_OKWhenCheckPasses(t *testing.T) {
	addr := freeAddr(t)

	server, err := health.New(
		health.WithHealthAddr(addr),
		health.WithReadinessCheck(func(context.Context) error { return nil }),
	)
	require.NoError(t, err)
	shutdownCleanup(t, server)

	assert.Equal(t, http.StatusOK, get(t, addr, "/readyz"))
}

func TestReadiness_ServiceUnavailableWhenCheckFails(t *testing.T) {
	addr := freeAddr(t)

	server, err := health.New(
		health.WithHealthAddr(addr),
		health.WithReadinessCheck(func(context.Context) error { return errors.New("temporal unreachable") }),
	)
	require.NoError(t, err)
	shutdownCleanup(t, server)

	// Not-ready surfaces as 503; liveness on the same server stays 200.
	assert.Equal(t, http.StatusServiceUnavailable, get(t, addr, "/readyz"))
	assert.Equal(t, http.StatusOK, get(t, addr, "/healthz"))
}

func TestReadiness_OKWhenNoCheckConfigured(t *testing.T) {
	addr := freeAddr(t)

	// With no readiness check, readiness defaults to always-ready.
	server, err := health.New(health.WithHealthAddr(addr))
	require.NoError(t, err)
	shutdownCleanup(t, server)

	assert.Equal(t, http.StatusOK, get(t, addr, "/readyz"))
}

func TestDisabled_NoServerStarted(t *testing.T) {
	// No address configured → no HTTP server, and Shutdown is a safe no-op.
	server, err := health.New()
	require.NoError(t, err)
	require.NoError(t, server.Shutdown(t.Context()))
}

func TestNewFromEnv_FallsBackToDefaultAddr(t *testing.T) {
	addr := freeAddr(t)
	// HEALTH_ADDR is unset here (no t.Setenv), so NewFromEnv uses the default.

	server, shutdown, err := health.NewFromEnv(addr, nil)
	require.NoError(t, err)
	t.Cleanup(shutdown)
	require.NotNil(t, server)

	assert.Equal(t, http.StatusOK, get(t, addr, "/healthz"))
}

func TestNewFromEnv_EmptyHealthAddr_DisablesServer(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("HEALTH_ADDR", "")

	// An explicit empty HEALTH_ADDR disables the server even when a non-empty
	// default is supplied — no port is opened.
	server, shutdown, err := health.NewFromEnv(addr, nil)
	require.NoError(t, err)
	t.Cleanup(shutdown)

	_, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	assert.Error(t, dialErr, "no health server should be listening on %s when HEALTH_ADDR is empty", addr)

	// Shutdown remains a safe no-op.
	require.NoError(t, server.Shutdown(t.Context()))
}

func TestNewFromEnv_HealthAddr_StartsServerOnThatAddress(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("HEALTH_ADDR", addr)

	// A custom HEALTH_ADDR wins over the default.
	server, shutdown, err := health.NewFromEnv(freeAddr(t), func(context.Context) error { return nil })
	require.NoError(t, err)
	t.Cleanup(shutdown)
	require.NotNil(t, server)

	assert.Equal(t, http.StatusOK, get(t, addr, "/healthz"))
	assert.Equal(t, http.StatusOK, get(t, addr, "/readyz"))
}

func TestNewFromEnv_EmptyDefault_KeepsServerDisabled(t *testing.T) {
	// HEALTH_ADDR is unset here, so NewFromEnv falls back to the (empty) default.

	// Empty default with HEALTH_ADDR unset leaves the server disabled.
	server, shutdown, err := health.NewFromEnv("", nil)
	require.NoError(t, err)
	t.Cleanup(shutdown)

	require.NoError(t, server.Shutdown(t.Context()))
}
