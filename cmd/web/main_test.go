package main

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

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

// TestServesSPA exercises cmd/web's actual wiring (distFS + webserver.
// NewHandler) against the public HTTP surface: AC "make build produces a
// binary that, when run, serves the SPA shell at /". No api handler is
// injected here (nil) since this test only cares about SPA serving in
// isolation, and this package must not need a Temporal connection to prove
// it — webserver_test.go (pkg/webserver) separately proves /api/*
// delegation, and cmd/web/main_integration_test.go proves the full run()
// wiring (Temporal client + health + metrics + this handler) against a real
// Temporal. Liveness/readiness now live on pkg/health's own server
// (HEALTH_ADDR), not on this handler — see pkg/webserver's package doc for
// why the old hand-rolled /healthz here was dropped.
func TestServesSPA(t *testing.T) {
	assets := requireBuiltFrontend(t)

	handler, err := webserver.NewHandler(assets, nil)
	require.NoError(t, err)

	server := httptest.NewServer(handler)
	defer server.Close()

	rootResp, err := http.Get(server.URL + "/")
	require.NoError(t, err)

	defer func() { _ = rootResp.Body.Close() }()

	assert.Equal(t, http.StatusOK, rootResp.StatusCode)

	body, err := io.ReadAll(rootResp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "<html")
}
