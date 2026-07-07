package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/webhook"
)

// capture records the most recent request a test server received.
type capture struct {
	hits        atomic.Int32
	contentType string
	body        []byte
}

// newServer returns an httptest server that records each request into a capture
// and responds with status. The capture is returned for assertions.
func newServer(t *testing.T, status int) (*httptest.Server, *capture) {
	t.Helper()

	cap := &capture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		cap.hits.Add(1)
		cap.contentType = r.Header.Get("Content-Type")
		cap.body = body

		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)

	return server, cap
}

func TestSend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		emptyURL    bool
		assertError require.ErrorAssertionFunc
		expectHit   bool
	}{
		{name: "2xx success", status: http.StatusNoContent, expectHit: true},
		{name: "200 success", status: http.StatusOK, expectHit: true},
		{name: "non-2xx error", status: http.StatusInternalServerError, assertError: require.Error, expectHit: true},
		{name: "empty URL no-op", emptyURL: true, expectHit: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assertError := test.assertError
			if assertError == nil {
				assertError = require.NoError
			}

			server, cap := newServer(t, test.status)

			url := server.URL
			if test.emptyURL {
				url = ""
			}

			client := webhook.New(url)
			err := client.Send(t.Context(), webhook.Message{Content: "hello world"})
			assertError(t, err)

			if !test.expectHit {
				assert.Equal(t, int32(0), cap.hits.Load())

				return
			}

			require.Equal(t, int32(1), cap.hits.Load())
			assert.Equal(t, "application/json", cap.contentType)
			assert.JSONEq(t, `{"content":"hello world"}`, string(cap.body))
		})
	}
}

func TestSendFile(t *testing.T) {
	t.Parallel()

	const fileContents = "recovery iso bytes"

	tests := []struct {
		name        string
		status      int
		emptyURL    bool
		missingFile bool
		assertError require.ErrorAssertionFunc
		expectHit   bool
	}{
		{name: "uploads multipart attachment", status: http.StatusOK, expectHit: true},
		{name: "non-2xx error", status: http.StatusBadRequest, assertError: require.Error, expectHit: true},
		{name: "missing file error", status: http.StatusOK, missingFile: true, assertError: require.Error, expectHit: false},
		{name: "empty URL no-op", emptyURL: true, expectHit: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assertError := test.assertError
			if assertError == nil {
				assertError = require.NoError
			}

			server, cap := newServer(t, test.status)

			url := server.URL
			if test.emptyURL {
				url = ""
			}

			path := filepath.Join(t.TempDir(), "recovery.iso")
			if !test.missingFile {
				require.NoError(t, os.WriteFile(path, []byte(fileContents), 0o600))
			}

			client := webhook.New(url)
			err := client.SendFile(t.Context(), path)
			assertError(t, err)

			if !test.expectHit {
				assert.Equal(t, int32(0), cap.hits.Load())

				return
			}

			require.Equal(t, int32(1), cap.hits.Load())

			mediaType, params, perr := mime.ParseMediaType(cap.contentType)
			require.NoError(t, perr)
			assert.Equal(t, "multipart/form-data", mediaType)

			reader := multipart.NewReader(bytes.NewReader(cap.body), params["boundary"])
			part, perr := reader.NextPart()
			require.NoError(t, perr)

			assert.Equal(t, "files[0]", part.FormName())
			assert.Equal(t, "recovery.iso", part.FileName())

			got, perr := io.ReadAll(part)
			require.NoError(t, perr)
			assert.Equal(t, fileContents, string(got))
		})
	}
}

func TestSendFailurePayload(t *testing.T) {
	t.Parallel()

	server, cap := newServer(t, http.StatusNoContent)

	client := webhook.New(server.URL)
	client.SendFailure(t.Context(), "run-123", "Write", assert.AnError)

	require.Equal(t, int32(1), cap.hits.Load())
	assert.Equal(t, "application/json", cap.contentType)

	var payload struct {
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal(cap.body, &payload))

	assert.Contains(t, payload.Content, "run-123")
	assert.Contains(t, payload.Content, "Write")
	assert.Contains(t, payload.Content, assert.AnError.Error())
}

func TestSendOperatorPausePayload(t *testing.T) {
	t.Parallel()

	server, cap := newServer(t, http.StatusNoContent)

	client := webhook.New(server.URL)
	client.SendOperatorPause(t.Context(), "run-123", []string{"TA0001L6", "TA0002L6"}, 2)

	require.Equal(t, int32(1), cap.hits.Load())
	assert.Equal(t, "application/json", cap.contentType)

	var payload struct {
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal(cap.body, &payload))

	assert.Contains(t, payload.Content, "run-123")
	assert.Contains(t, payload.Content, "TA0001L6")
	assert.Contains(t, payload.Content, "TA0002L6")
	assert.Contains(t, payload.Content, "2 more")
}

func TestSendOperatorPauseEmptyURLNoOp(t *testing.T) {
	t.Parallel()

	server, cap := newServer(t, http.StatusNoContent)
	_ = server

	client := webhook.New("")
	// Must not panic and must not contact any endpoint.
	client.SendOperatorPause(t.Context(), "run-123", []string{"TA0001L6"}, 1)

	assert.Equal(t, int32(0), cap.hits.Load())
}

func TestSendWritePathPausePayload(t *testing.T) {
	t.Parallel()

	server, cap := newServer(t, http.StatusNoContent)

	client := webhook.New(server.URL)
	client.SendWritePathPause(t.Context(), "run-123", "Write", []string{"TA0002L6"}, []int{101}, "mkltfs failed")

	require.Equal(t, int32(1), cap.hits.Load())
	assert.Equal(t, "application/json", cap.contentType)

	var payload struct {
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal(cap.body, &payload))

	assert.Contains(t, payload.Content, "run-123")
	assert.Contains(t, payload.Content, "Write")
	assert.Contains(t, payload.Content, "TA0002L6")
	assert.Contains(t, payload.Content, "101")
	assert.Contains(t, payload.Content, "mkltfs failed")
	assert.Contains(t, payload.Content, "tapectl resume")
	assert.Contains(t, payload.Content, "tapectl abort")
}

func TestSendWritePathPauseEmptyURLNoOp(t *testing.T) {
	t.Parallel()

	server, cap := newServer(t, http.StatusNoContent)
	_ = server

	client := webhook.New("")
	// Must not panic and must not contact any endpoint.
	client.SendWritePathPause(t.Context(), "run-123", "Load", nil, []int{100, 101}, "move medium failed")

	assert.Equal(t, int32(0), cap.hits.Load())
}

func TestSendBurnPausePayload(t *testing.T) {
	t.Parallel()

	server, cap := newServer(t, http.StatusNoContent)

	client := webhook.New(server.URL)
	client.SendBurnPause(t.Context(), "run-123", []string{"/dev/sr0", "/dev/sr1"}, "a burn or verify failed: drive reported a write error")

	require.Equal(t, int32(1), cap.hits.Load())
	assert.Equal(t, "application/json", cap.contentType)

	var payload struct {
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal(cap.body, &payload))

	assert.Contains(t, payload.Content, "run-123")
	assert.Contains(t, payload.Content, "/dev/sr0")
	assert.Contains(t, payload.Content, "/dev/sr1")
	assert.Contains(t, payload.Content, "a burn or verify failed")
	assert.Contains(t, payload.Content, "tapectl resume")
	assert.Contains(t, payload.Content, "tapectl abort")
}

func TestSendBurnPauseEmptyURLNoOp(t *testing.T) {
	t.Parallel()

	server, cap := newServer(t, http.StatusNoContent)
	_ = server

	client := webhook.New("")
	// Must not panic and must not contact any endpoint.
	client.SendBurnPause(t.Context(), "run-123", []string{"/dev/sr0"}, "the burn-set is complete; load fresh blank recovery discs for the next set")

	assert.Equal(t, int32(0), cap.hits.Load())
}

func TestSendFailureEmptyURLNoOp(t *testing.T) {
	t.Parallel()

	server, cap := newServer(t, http.StatusNoContent)
	_ = server

	client := webhook.New("")
	// Must not panic and must not contact any endpoint.
	client.SendFailure(t.Context(), "run-123", "Write", assert.AnError)

	assert.Equal(t, int32(0), cap.hits.Load())
}

// TestSendFailureLogsDeliveryFailure verifies that a delivery failure is logged
// via slog and that SendFailure returns without surfacing the failure (it
// cannot mask the caller's original error). This test mutates the global slog
// default, so it is not run in parallel.
func TestSendFailureLogsDeliveryFailure(t *testing.T) {
	original := slog.Default()

	t.Cleanup(func() { slog.SetDefault(original) })

	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	server, cap := newServer(t, http.StatusInternalServerError)

	client := webhook.New(server.URL)
	// SendFailure returns no value; the assertion is that it logs and does not
	// panic, leaving the caller's original error intact.
	client.SendFailure(t.Context(), "run-456", "Verify", assert.AnError)

	require.Equal(t, int32(1), cap.hits.Load())

	logged := logs.String()
	assert.Contains(t, logged, "failed to deliver webhook failure alert")
	assert.Contains(t, logged, "run-456")
	assert.Contains(t, logged, "Verify")
}

// TestSendFileBoundedByContextNotFixedTimeout proves the upload's timeout is the
// caller's request context (AC1), not a fixed client-wide cap: against a server
// that delays its response, the call succeeds when the context deadline exceeds
// the delay and fails with a deadline error when it does not. This exercises the
// removal of the former fixed 10 s http.Client.Timeout without waiting 10 s.
func TestSendFileBoundedByContextNotFixedTimeout(t *testing.T) {
	t.Parallel()

	const serverDelay = 200 * time.Millisecond

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)

		time.Sleep(serverDelay)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	path := filepath.Join(t.TempDir(), "report.pdf")
	require.NoError(t, os.WriteFile(path, []byte("report bytes"), 0o600))

	client := webhook.New(server.URL)

	t.Run("succeeds when context deadline exceeds server delay", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), serverDelay*5)
		defer cancel()

		require.NoError(t, client.SendFile(ctx, path))
	})

	t.Run("fails with deadline error when context deadline is below server delay", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), serverDelay/4)
		defer cancel()

		err := client.SendFile(ctx, path)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

// TestAlertContentTruncatedToDiscordLimit proves an alert whose error summary
// alone exceeds Discord's 2000-character content limit is still accepted (AC2):
// the server receives exactly one request, the delivered content is within the
// limit, and the actionable text (run id / resume-abort instructions) survives
// the truncation rather than being pushed out by the giant error tail.
func TestAlertContentTruncatedToDiscordLimit(t *testing.T) {
	t.Parallel()

	longSummary := strings.Repeat("x", 5000)

	tests := []struct {
		name        string
		send        func(client *webhook.Client, ctx context.Context)
		mustContain []string
	}{
		{
			name: "SendFailure",
			send: func(client *webhook.Client, ctx context.Context) {
				client.SendFailure(ctx, "run-123", "Write", errors.New(longSummary))
			},
			mustContain: []string{"run-123"},
		},
		{
			name: "SendWritePathPause",
			send: func(client *webhook.Client, ctx context.Context) {
				client.SendWritePathPause(ctx, "run-123", "Write", []string{"TA0002L6"}, []int{101}, longSummary)
			},
			mustContain: []string{"run-123", "tapectl resume", "tapectl abort"},
		},
		{
			name: "SendBurnPause",
			send: func(client *webhook.Client, ctx context.Context) {
				client.SendBurnPause(ctx, "run-123", []string{"/dev/sr0"}, longSummary)
			},
			mustContain: []string{"run-123", "tapectl resume", "tapectl abort"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server, cap := newServer(t, http.StatusNoContent)

			client := webhook.New(server.URL)
			test.send(client, t.Context())

			require.Equal(t, int32(1), cap.hits.Load())

			var payload struct {
				Content string `json:"content"`
			}
			require.NoError(t, json.Unmarshal(cap.body, &payload))

			assert.LessOrEqual(t, utf8.RuneCountInString(payload.Content), 2000,
				"content must be within Discord's 2000-character limit")

			for _, substr := range test.mustContain {
				assert.Contains(t, payload.Content, substr)
			}
		})
	}
}

func TestSendFailureNilError(t *testing.T) {
	t.Parallel()

	server, cap := newServer(t, http.StatusNoContent)

	client := webhook.New(server.URL)
	client.SendFailure(t.Context(), "run-789", "Prepare", nil)

	require.Equal(t, int32(1), cap.hits.Load())

	body := string(cap.body)
	assert.True(t, strings.Contains(body, "run-789"), "payload should name the run: %s", body)
}
