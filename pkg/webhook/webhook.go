// Package webhook provides a Discord webhook client used for both run-success
// delivery (report + recovery ISO) and operational run-failure alerts
// (SPEC §11). A Client wraps a webhook URL; when that URL is empty every method
// is a no-op, so callers never nil-check the client. The failure-alert path
// (SendFailure) never masks the caller's original error: a delivery failure is
// logged via slog, not returned.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const defaultTimeout = 10 * time.Second

// Message is a Discord webhook message payload. Only the content field is used;
// it is rendered as the message body.
type Message struct {
	Content string `json:"content"`
}

// Client posts messages and file attachments to a Discord webhook. The zero
// value is not usable; construct one with New. A Client whose URL is empty is a
// disabled no-op: every method returns nil (or, for SendFailure, returns
// silently) without performing any I/O.
type Client struct {
	url        string
	httpClient *http.Client
}

// New returns a Client targeting the given webhook URL. An empty URL yields a
// disabled client whose methods are no-ops, so callers can construct
// unconditionally and never nil-check.
func New(url string) *Client {
	return &Client{
		url:        url,
		httpClient: &http.Client{Timeout: defaultTimeout},
	}
}

// Send posts msg to the webhook as a JSON payload. It returns nil on an HTTP
// 2xx response and a non-nil error on any non-2xx status or transport failure.
// When the client's URL is empty, Send is a no-op and returns nil.
func (c *Client) Send(ctx context.Context, msg Message) error {
	if c.url == "" {
		return nil
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("webhook: marshalling message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook: creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	return c.do(req)
}

// SendFile uploads the file at path to the webhook as a multipart attachment
// (Discord's files[0] form field). It returns nil on an HTTP 2xx response and a
// non-nil error if the file cannot be read or the endpoint returns a non-2xx
// status. When the client's URL is empty, SendFile is a no-op and returns nil.
func (c *Client) SendFile(ctx context.Context, path string) error {
	if c.url == "" {
		return nil
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("webhook: opening %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	var body bytes.Buffer

	writer := multipart.NewWriter(&body)

	part, err := writer.CreateFormFile("files[0]", filepath.Base(path))
	if err != nil {
		return fmt.Errorf("webhook: creating form file: %w", err)
	}

	if _, err := io.Copy(part, file); err != nil {
		return fmt.Errorf("webhook: copying %q into request: %w", path, err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("webhook: finalizing multipart body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, &body)
	if err != nil {
		return fmt.Errorf("webhook: creating request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	return c.do(req)
}

// SendFailure posts a concise failure alert naming the run, the failing phase,
// and the error summary. Per SPEC §11 it must never mask the caller's original
// error, so it returns nothing: a delivery failure (transport error or non-2xx
// status) is logged via slog and otherwise ignored, leaving runErr to propagate
// in the caller. When the client's URL is empty, SendFailure is a silent no-op.
func (c *Client) SendFailure(ctx context.Context, runID, phase string, runErr error) {
	if c.url == "" {
		return
	}

	errSummary := "unknown error"
	if runErr != nil {
		errSummary = runErr.Error()
	}

	msg := Message{
		Content: fmt.Sprintf("Backup run %s failed in phase %q: %s", runID, phase, errSummary),
	}

	if err := c.Send(ctx, msg); err != nil {
		slog.Error("failed to deliver webhook failure alert",
			"run_id", runID,
			"phase", phase,
			"delivery_error", err,
		)
	}
}

// do sends req with the client's HTTP client and maps the response to an error:
// nil for 2xx, a non-nil error otherwise. The response body is always drained
// and closed.
func (c *Client) do(req *http.Request) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook: unexpected status %d", resp.StatusCode)
	}

	return nil
}
