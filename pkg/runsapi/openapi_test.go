package runsapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// documentedRoutes is every /api route the spec must describe, including the
// two webauth routes. Coverage is checked in both directions.
var documentedRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/runs"},
	{http.MethodGet, "/api/runs/{runID}"},
	{http.MethodPost, "/api/runs"},
	{http.MethodPost, "/api/runs/{runID}/resume"},
	{http.MethodPost, "/api/runs/{runID}/abort"},
	{http.MethodPost, "/api/runs/{runID}/cancel"},
	{http.MethodGet, "/api/events/runs/{runID}"},
	{http.MethodGet, "/api/runs/{runID}/phases"},
	{http.MethodGet, "/api/runs/{runID}/config"},
	{http.MethodGet, "/api/runs/{runID}/tapes"},
	{http.MethodGet, "/api/runs/{runID}/delivery"},
	{http.MethodGet, "/api/tapes"},
	{http.MethodGet, "/api/runs/{runID}/metrics/drives"},
	{http.MethodGet, "/api/runs/{runID}/metrics/drives/{barcode}/history"},
	{http.MethodGet, "/api/runs/{runID}/logs"},
	{http.MethodGet, "/api/config/schema"},
	{http.MethodPost, "/api/age/keygen"},
	{http.MethodGet, "/api/config/ui"},
	{http.MethodGet, "/api/me"},
	{http.MethodGet, "/api/build-info"},
}

func fetchSpec(t *testing.T) map[string]any {
	t.Helper()

	recorder := httptest.NewRecorder()
	newDocsHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Header().Get("Content-Type"), "json")

	var spec map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &spec))

	return spec
}

func TestOpenAPISpecIsServedAndWellFormed(t *testing.T) {
	spec := fetchSpec(t)

	assert.Equal(t, "3.1.0", spec["openapi"])

	info, ok := spec["info"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Tape Archiver Web API", info["title"])
	assert.NotEmpty(t, info["version"])
}

func TestOpenAPICoversEveryRoute(t *testing.T) {
	spec := fetchSpec(t)

	paths, ok := spec["paths"].(map[string]any)
	require.True(t, ok)

	declared := make(map[string]bool)

	for path, item := range paths {
		methods, ok := item.(map[string]any)
		require.True(t, ok)

		for method := range methods {
			declared[method+" "+path] = true
		}
	}

	// OpenAPI methods are lower-case.
	for _, route := range documentedRoutes {
		key := strings.ToLower(route.method) + " " + route.path
		assert.Truef(t, declared[key], "route %s %s is missing from the OpenAPI spec", route.method, route.path)
	}

	// The spec/docs plumbing routes are not self-described, so the sets match exactly.
	assert.Lenf(t, declared, len(documentedRoutes),
		"spec declares %d operations but %d are documented — update documentedRoutes", len(declared), len(documentedRoutes))
}

func TestDocumentedRoutesAreServed(t *testing.T) {
	mux := newMux(newHandler(&fakeTemporalClient{}, func(string) string { return "" }))

	for _, route := range documentedRoutes {
		if route.path == "/api/me" || route.path == "/api/build-info" {
			continue // served by webauth, not runsapi's mux.
		}

		concrete := requestPath(route.path)

		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(route.method, concrete, nil))

		// The handler may fail against the fake client; anything but 404 proves the route exists.
		assert.NotEqualf(t, http.StatusNotFound, recorder.Code,
			"documented route %s %s does not resolve on the API mux", route.method, concrete)
	}
}

func TestMuxServesDocRoutes(t *testing.T) {
	mux := newMux(newHandler(&fakeTemporalClient{}, func(string) string { return "" }))

	for _, path := range []string{"/api/openapi.json", "/api/openapi.yaml", "/api/docs"} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

		assert.Equalf(t, http.StatusOK, recorder.Code, "%s served through the API mux", path)
	}
}

func TestDocsPageIsServed(t *testing.T) {
	recorder := httptest.NewRecorder()
	newDocsHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/docs", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Header().Get("Content-Type"), "text/html")
	// The docs page renders this API's own spec.
	assert.Contains(t, recorder.Body.String(), "/api/openapi")
}

func TestOpenAPIYAMLIsServed(t *testing.T) {
	recorder := httptest.NewRecorder()
	newDocsHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/openapi.yaml", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "openapi:")
}

func TestOpenAPIErrorEnvelopeMatchesReal(t *testing.T) {
	spec := fetchSpec(t)

	schemas, ok := spec["components"].(map[string]any)["schemas"].(map[string]any)
	require.True(t, ok)

	apiErr, ok := schemas["ApiError"].(map[string]any)
	require.True(t, ok, "spec declares an ApiError schema")

	props, ok := apiErr["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, props, "error", "the error envelope uses an `error` field, matching errorResponse")

	required, ok := apiErr["required"].([]any)
	require.True(t, ok)
	assert.Contains(t, required, "error")
}

// requestPath substitutes sample values for {wildcard} segments.
func requestPath(pattern string) string {
	switch pattern {
	case "/api/runs/{runID}":
		return "/api/runs/sample"
	case "/api/runs/{runID}/resume":
		return "/api/runs/sample/resume"
	case "/api/runs/{runID}/abort":
		return "/api/runs/sample/abort"
	case "/api/runs/{runID}/cancel":
		return "/api/runs/sample/cancel"
	case "/api/events/runs/{runID}":
		return "/api/events/runs/sample"
	case "/api/runs/{runID}/phases":
		return "/api/runs/sample/phases"
	case "/api/runs/{runID}/config":
		return "/api/runs/sample/config"
	case "/api/runs/{runID}/tapes":
		return "/api/runs/sample/tapes"
	case "/api/runs/{runID}/delivery":
		return "/api/runs/sample/delivery"
	case "/api/runs/{runID}/metrics/drives":
		return "/api/runs/sample/metrics/drives"
	case "/api/runs/{runID}/metrics/drives/{barcode}/history":
		return "/api/runs/sample/metrics/drives/TA0001L9/history"
	case "/api/runs/{runID}/logs":
		return "/api/runs/sample/logs"
	default:
		return pattern
	}
}
