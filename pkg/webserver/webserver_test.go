package webserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAssets is a minimal in-memory SPA build: an index.html shell plus one
// hashed asset, standing in for a real `vite build` output so these tests
// never depend on web/ actually having been built.
func fakeAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           &fstest.MapFile{Data: []byte("<html><body>tape-archiver shell</body></html>")},
		"assets/app-abc123.js": &fstest.MapFile{Data: []byte("console.log('app')")},
	}
}

func TestNewHandler(t *testing.T) {
	t.Run("rejects assets with no index.html", func(t *testing.T) {
		_, err := NewHandler(fstest.MapFS{"assets/app.js": &fstest.MapFile{Data: []byte("x")}}, nil)
		require.Error(t, err)
	})

	t.Run("builds a handler for valid assets", func(t *testing.T) {
		handler, err := NewHandler(fakeAssets(), nil)
		require.NoError(t, err)
		assert.NotNil(t, handler)
	})
}

func TestHandlerRoutes(t *testing.T) {
	handler, err := NewHandler(fakeAssets(), nil)
	require.NoError(t, err)

	tests := []struct {
		name           string
		method         string
		path           string
		wantStatus     int
		wantBodyHas    string
		wantContentHas string
	}{
		{
			name:        "root serves the SPA shell",
			method:      http.MethodGet,
			path:        "/",
			wantStatus:  http.StatusOK,
			wantBodyHas: "tape-archiver shell",
		},
		{
			name:           "a real asset is served as itself",
			method:         http.MethodGet,
			path:           "/assets/app-abc123.js",
			wantStatus:     http.StatusOK,
			wantBodyHas:    "console.log",
			wantContentHas: "javascript",
		},
		{
			name:        "an unknown client-side route falls back to the SPA shell",
			method:      http.MethodGet,
			path:        "/runs/some-run-id",
			wantStatus:  http.StatusOK,
			wantBodyHas: "tape-archiver shell",
		},
		{
			name:        "a client-side route matching a real asset directory name falls back to the SPA shell",
			method:      http.MethodGet,
			path:        "/assets",
			wantStatus:  http.StatusOK,
			wantBodyHas: "tape-archiver shell",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			assert.Equal(t, test.wantStatus, recorder.Code)
			assert.Contains(t, recorder.Body.String(), test.wantBodyHas)

			if test.wantContentHas != "" {
				assert.Contains(t, recorder.Header().Get("Content-Type"), test.wantContentHas)
			}
		})
	}
}

// TestAPIDelegation covers the /api/* routing contract: requests under
// /api/ reach the injected api handler rather than falling back to the SPA,
// and the SPA catch-all still serves everything else — proving the ordering
// promised by NewHandler's doc comment without depending on any real API
// package (pkg/webserver must not import pkg/runsapi or Temporal).
func TestAPIDelegation(t *testing.T) {
	var apiHit string

	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHit = r.URL.Path

		w.WriteHeader(http.StatusTeapot)
	})

	handler, err := NewHandler(fakeAssets(), api)
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusTeapot, recorder.Code, "an /api/ request must reach the injected api handler")
	assert.Equal(t, "/api/runs", apiHit)

	// A non-API path still falls through to the SPA even with an api handler
	// configured.
	request = httptest.NewRequest(http.MethodGet, "/runs/some-run-id", nil)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "tape-archiver shell")
}
