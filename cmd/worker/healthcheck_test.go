package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stripScheme turns an httptest server URL (http://host:port) into the bare
// host:port the healthcheck resolves against.
func stripScheme(t *testing.T, serverURL string) string {
	t.Helper()

	const prefix = "http://"
	require.True(t, len(serverURL) > len(prefix) && serverURL[:len(prefix)] == prefix,
		"unexpected test server URL %q", serverURL)

	return serverURL[len(prefix):]
}

func TestRunHealthcheck(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    require.ErrorAssertionFunc
	}{
		{name: "ready returns 200", statusCode: http.StatusOK, wantErr: require.NoError},
		{name: "not ready returns 503", statusCode: http.StatusServiceUnavailable, wantErr: require.Error},
		{name: "unexpected 500", statusCode: http.StatusInternalServerError, wantErr: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var probedPath string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probedPath = r.URL.Path

				w.WriteHeader(test.statusCode)
			}))
			defer server.Close()

			// Pass the address as the positional override so the probe targets
			// the test server rather than the default HEALTH_ADDR.
			err := runHealthcheck(t.Context(), []string{stripScheme(t, server.URL)})
			test.wantErr(t, err)

			// The self-probe must consult readiness, not liveness.
			assert.Equal(t, "/readyz", probedPath)
		})
	}
}

func TestRunHealthcheck_Unreachable(t *testing.T) {
	// A closed listener yields a free address that nothing is serving, so the
	// probe must fail with a dial error (non-zero exit).
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	require.Error(t, runHealthcheck(t.Context(), []string{addr}))
}

func TestResolveHealthAddr(t *testing.T) {
	tests := []struct {
		name     string
		envAddr  *string
		args     []string
		expected string
	}{
		{
			name:     "unset falls back to default, wildcard host to loopback",
			envAddr:  nil,
			expected: "127.0.0.1:8080",
		},
		{
			name:     "empty HEALTH_ADDR disables (no target)",
			envAddr:  ptr(""),
			expected: "",
		},
		{
			name:     "HEALTH_ADDR host preserved",
			envAddr:  ptr("127.0.0.1:9000"),
			expected: "127.0.0.1:9000",
		},
		{
			name:     "explicit wildcard host rewritten to loopback",
			envAddr:  ptr("0.0.0.0:9000"),
			expected: "127.0.0.1:9000",
		},
		{
			name:     "positional arg overrides env",
			envAddr:  ptr(":8080"),
			args:     []string{"127.0.0.1:7000"},
			expected: "127.0.0.1:7000",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.envAddr != nil {
				t.Setenv("HEALTH_ADDR", *test.envAddr)
			}

			assert.Equal(t, test.expected, resolveHealthAddr(test.args))
		})
	}
}

func ptr[T any](v T) *T { return &v }
