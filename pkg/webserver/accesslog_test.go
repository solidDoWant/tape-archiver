package webserver

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureLogger returns a logger writing JSON to buf at debug level (so no
// record is filtered out by level), for tests that inspect the emitted record.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// decodeRecord unmarshals the single JSON log line in buf.
func decodeRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	line := strings.TrimSpace(buf.String())
	require.NotEmpty(t, line, "expected exactly one log record")
	require.NotContains(t, line, "\n", "expected exactly one log record")

	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &record))

	return record
}

// handlerWriting returns a handler that sets status (via WriteHeader unless it
// is 0) and writes body.
func handlerWriting(status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
		}

		_, _ = w.Write([]byte(body))
	})
}

func TestAccessLogRecord(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := AccessLog(captureLogger(buf), nil, handlerWriting(http.StatusTeapot, "hello"))

	request := httptest.NewRequest(http.MethodPost, "/api/runs", nil)
	request.RemoteAddr = "203.0.113.7:54321"
	handler.ServeHTTP(httptest.NewRecorder(), request)

	record := decodeRecord(t, buf)
	assert.Equal(t, "web: request", record["msg"])
	assert.Equal(t, http.MethodPost, record["method"])
	assert.Equal(t, "/api/runs", record["path"])
	assert.Equal(t, float64(http.StatusTeapot), record["status"])
	assert.Equal(t, float64(len("hello")), record["bytes"])
	assert.Equal(t, "203.0.113.7:54321", record["remote"])
	assert.Contains(t, record, "duration_ms")
	assert.NotContains(t, record, "user", "no userFor was supplied")
}

func TestAccessLogDefaultStatusOK(t *testing.T) {
	buf := &bytes.Buffer{}
	// A handler that writes a body without an explicit WriteHeader implicitly
	// sends 200; the recorder must report that, not a zero status.
	handler := AccessLog(captureLogger(buf), nil, handlerWriting(0, "body"))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	record := decodeRecord(t, buf)
	assert.Equal(t, float64(http.StatusOK), record["status"])
	assert.Equal(t, float64(len("body")), record["bytes"])
}

// TestAccessLogNeverLogsQuery is the security-critical case: the OIDC callback
// URL carries the authorization code and state in its query string, and those
// single-use secrets must never reach the log — only the path is recorded.
func TestAccessLogNeverLogsQuery(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := AccessLog(captureLogger(buf), nil, handlerWriting(http.StatusFound, ""))

	request := httptest.NewRequest(http.MethodGet, "/auth/callback?code=SUPERSECRETCODE&state=xyz", nil)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	assert.NotContains(t, buf.String(), "SUPERSECRETCODE", "the authorization code must never be logged")
	assert.NotContains(t, buf.String(), "state=xyz")

	record := decodeRecord(t, buf)
	assert.Equal(t, "/auth/callback", record["path"], "only the path is logged, never the query")
}

func TestAccessLogUserAttribution(t *testing.T) {
	tests := []struct {
		name     string
		userFor  func(*http.Request) string
		wantUser any // nil = the "user" field must be absent
	}{
		{
			name:     "nil userFor omits the field",
			userFor:  nil,
			wantUser: nil,
		},
		{
			name:     "empty user omits the field",
			userFor:  func(*http.Request) string { return "" },
			wantUser: nil,
		},
		{
			name:     "non-empty user is logged",
			userFor:  func(*http.Request) string { return "oidc-subject-123" },
			wantUser: "oidc-subject-123",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			handler := AccessLog(captureLogger(buf), test.userFor, handlerWriting(http.StatusOK, ""))

			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

			record := decodeRecord(t, buf)
			if test.wantUser == nil {
				assert.NotContains(t, record, "user")
			} else {
				assert.Equal(t, test.wantUser, record["user"])
			}
		})
	}
}

func TestAccessLogLevelTracksStatus(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantLevel string
	}{
		{name: "2xx logs at info", status: http.StatusOK, wantLevel: "INFO"},
		{name: "3xx logs at info", status: http.StatusFound, wantLevel: "INFO"},
		{name: "4xx logs at warn", status: http.StatusNotFound, wantLevel: "WARN"},
		{name: "5xx logs at error", status: http.StatusInternalServerError, wantLevel: "ERROR"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			handler := AccessLog(captureLogger(buf), nil, handlerWriting(test.status, ""))

			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

			assert.Equal(t, test.wantLevel, decodeRecord(t, buf)["level"])
		})
	}
}

func TestAccessLogRemoteAddr(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		forwarded  string
		want       string
	}{
		{
			name:       "no forwarded header uses RemoteAddr",
			remoteAddr: "10.0.0.1:1234",
			want:       "10.0.0.1:1234",
		},
		{
			name:       "single forwarded entry is the client",
			remoteAddr: "10.0.0.1:1234",
			forwarded:  "198.51.100.9",
			want:       "198.51.100.9",
		},
		{
			name:       "first forwarded entry wins over proxies",
			remoteAddr: "10.0.0.1:1234",
			forwarded:  "198.51.100.9, 10.0.0.5, 10.0.0.6",
			want:       "198.51.100.9",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			handler := AccessLog(captureLogger(buf), nil, handlerWriting(http.StatusOK, ""))

			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.RemoteAddr = test.remoteAddr

			if test.forwarded != "" {
				request.Header.Set("X-Forwarded-For", test.forwarded)
			}

			handler.ServeHTTP(httptest.NewRecorder(), request)

			assert.Equal(t, test.want, decodeRecord(t, buf)["remote"])
		})
	}
}

func TestAccessLogAnnotate(t *testing.T) {
	t.Run("annotated fields are merged and the record is raised to warn", func(t *testing.T) {
		buf := &bytes.Buffer{}
		// A handler that annotates and then responds with a 302 — the shape of a
		// failed OIDC callback: its status alone would keep it at info, but the
		// annotation must surface it at warn so it is not hidden at LOG_LEVEL=warn.
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Annotate(r.Context(),
				slog.String("auth_error", "identity provider returned an error"),
				slog.String("idp_error", "access_denied"),
			)
			w.WriteHeader(http.StatusFound)
		})

		handler := AccessLog(captureLogger(buf), nil, inner)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/auth/callback", nil))

		record := decodeRecord(t, buf)
		assert.Equal(t, "WARN", record["level"], "an annotated request is surfaced at warn even though 302 alone is info")
		assert.Equal(t, "identity provider returned an error", record["auth_error"])
		assert.Equal(t, "access_denied", record["idp_error"])
	})

	t.Run("a 5xx annotation stays at error, not downgraded to warn", func(t *testing.T) {
		buf := &bytes.Buffer{}
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Annotate(r.Context(), slog.String("reason", "temporal unreachable"))
			w.WriteHeader(http.StatusInternalServerError)
		})

		handler := AccessLog(captureLogger(buf), nil, inner)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/runs", nil))

		record := decodeRecord(t, buf)
		assert.Equal(t, "ERROR", record["level"], "the level is the higher of status and annotation, never downgraded")
		assert.Equal(t, "temporal unreachable", record["reason"])
	})
}

// TestAnnotateWithoutMiddleware verifies Annotate is a safe no-op when the
// context did not come through AccessLog, so a handler can call it
// unconditionally (e.g. in a unit test that exercises it directly).
func TestAnnotateWithoutMiddleware(t *testing.T) {
	assert.NotPanics(t, func() {
		Annotate(context.Background(), slog.String("reason", "nowhere to go"))
	})
}

// TestAccessLogPreservesFlush guards the SSE path: pkg/runsapi drives its
// live-monitoring stream through http.NewResponseController(w).Flush, which
// finds the underlying Flusher only if the wrapper exposes it via Unwrap. If
// this breaks, run monitoring silently buffers instead of streaming.
func TestAccessLogPreservesFlush(t *testing.T) {
	var flushReached bool

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The recorder handed to the inner handler is the *statusRecorder;
		// the controller must be able to reach the real Flusher through it.
		err := http.NewResponseController(w).Flush()
		require.NoError(t, err, "Flush must reach the underlying ResponseWriter through the wrapper")

		flushReached = true
	})

	// httptest.ResponseRecorder implements http.Flusher, so a successful Flush
	// proves the controller unwrapped past statusRecorder to it.
	handler := AccessLog(captureLogger(&bytes.Buffer{}), nil, inner)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/runs/x/events", nil))

	assert.True(t, flushReached)
}
