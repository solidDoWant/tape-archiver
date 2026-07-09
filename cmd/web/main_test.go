package main

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/webserver"
)

func TestListenAddr(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "unset falls back to the default", env: map[string]string{}, want: defaultListenAddr},
		{name: "empty falls back to the default", env: map[string]string{"WEB_LISTEN_ADDRESS": ""}, want: defaultListenAddr},
		{name: "explicit override is honored", env: map[string]string{"WEB_LISTEN_ADDRESS": "127.0.0.1:9999"}, want: "127.0.0.1:9999"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := listenAddr(func(name string) string { return test.env[name] })
			assert.Equal(t, test.want, got)
		})
	}
}

// requireBuiltFrontend skips the calling test with an actionable reason when
// the embedded SPA has not actually been built (dist/ still only holds the
// committed dist/.gitkeep placeholder — see assets.go). `make test` builds
// the frontend before running Go tests, so this only fires when `go test` is
// invoked directly.
func requireBuiltFrontend(t *testing.T) fs.FS {
	t.Helper()

	assets, err := distFS()
	require.NoError(t, err)

	if _, err := fs.Stat(assets, "index.html"); err != nil {
		t.Skip("embedded SPA has no index.html: run `npm run build` in web/ (or `make build`) before this test")
	}

	return assets
}

// TestServesHealthzAndSPA exercises cmd/web's actual wiring (distFS +
// webserver.NewHandler) against the public HTTP surface: AC "make build
// produces a binary that, when run, serves the SPA shell at / and 200 at
// /healthz".
func TestServesHealthzAndSPA(t *testing.T) {
	assets := requireBuiltFrontend(t)

	handler, err := webserver.NewHandler(assets)
	require.NoError(t, err)

	server := httptest.NewServer(handler)
	defer server.Close()

	healthzResp, err := http.Get(server.URL + "/healthz")
	require.NoError(t, err)

	defer func() { _ = healthzResp.Body.Close() }()

	assert.Equal(t, http.StatusOK, healthzResp.StatusCode)

	rootResp, err := http.Get(server.URL + "/")
	require.NoError(t, err)

	defer func() { _ = rootResp.Body.Close() }()

	assert.Equal(t, http.StatusOK, rootResp.StatusCode)

	body, err := io.ReadAll(rootResp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "<html")
}

// TestRunServesAndShutsDownGracefully drives the full run() entrypoint
// (listen, serve, ctx-cancel-triggered shutdown) end to end over a real
// loopback listener, proving the same behavior main() relies on: the server
// answers /healthz while running and run() returns cleanly once ctx is
// cancelled.
func TestRunServesAndShutsDownGracefully(t *testing.T) {
	requireBuiltFrontend(t)

	ctx, cancel := context.WithCancel(t.Context())

	getenv := func(name string) string {
		if name == "WEB_LISTEN_ADDRESS" {
			return "127.0.0.1:0"
		}

		return ""
	}

	readyCh := make(chan string, 1)
	runErrCh := make(chan error, 1)

	go func() {
		runErrCh <- run(ctx, getenv, func(addr string) { readyCh <- addr })
	}()

	var addr string

	select {
	case addr = <-readyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server never became ready")
	}

	resp, err := http.Get("http://" + addr + "/healthz")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	cancel()

	select {
	case err := <-runErrCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after ctx cancellation")
	}
}

// TestRunListenError covers a bad listen address (a non-numeric port fails at
// net.Listen) returning an error rather than hanging or panicking.
func TestRunListenError(t *testing.T) {
	requireBuiltFrontend(t)

	getenv := func(name string) string {
		if name == "WEB_LISTEN_ADDRESS" {
			return "127.0.0.1:not-a-port"
		}

		return ""
	}

	err := run(t.Context(), getenv, nil)
	require.Error(t, err)
}
