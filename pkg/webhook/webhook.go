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
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// maxContentLength is Discord's hard limit on a webhook message's content
	// field, in Unicode code points. Content over this is rejected with HTTP 400.
	maxContentLength = 2000

	// maxSummaryLength budgets the variable error/reason text embedded in an
	// operational alert, leaving ample headroom under maxContentLength for the
	// fixed boilerplate — the resume/abort instructions in particular — plus the
	// run id and any tape/slot lists, so a giant error tail cannot push the
	// actionable text past Discord's limit.
	maxSummaryLength = 1500

	// truncationMarker is appended to any text truncate shortens, pointing the
	// operator at the authoritative full error (Temporal history and logs).
	truncationMarker = " […truncated; see logs]"
)

// truncate returns s unchanged when its rune count is at most max, otherwise it
// keeps a leading prefix and appends truncationMarker so the result's rune count
// is at most max. It counts Unicode code points, not bytes, because Discord's
// content limit is measured in code points.
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}

	markerLen := utf8.RuneCountInString(truncationMarker)
	if max <= markerLen {
		return string([]rune(s)[:max])
	}

	return string([]rune(s)[:max-markerLen]) + truncationMarker
}

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
		url: url,
		// No client-wide Timeout: Go's http.Client.Timeout caps the entire
		// request lifecycle, including the body upload, so a fixed value would
		// always undercut the caller's context deadline (e.g. the Deliver
		// activity's 30-minute bound for a multi-megabyte report). The request
		// context passed to every send is the sole timeout.
		httpClient: &http.Client{},
	}
}

// Send posts msg to the webhook as a JSON payload. It returns nil on an HTTP
// 2xx response and a non-nil error on any non-2xx status or transport failure.
// When the client's URL is empty, Send is a no-op and returns nil.
func (c *Client) Send(ctx context.Context, msg Message) error {
	if c.url == "" {
		return nil
	}

	// Backstop: no content-bearing message may exceed Discord's 2000-code-point
	// limit regardless of how long the interpolated fields are, or Discord
	// rejects it with HTTP 400 and the alert is lost. SendFile uses a multipart
	// body, not content, so it is unaffected.
	msg.Content = truncate(msg.Content, maxContentLength)

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

	errSummary = truncate(errSummary, maxSummaryLength)

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

// SendOperatorPause posts an operator-in-the-loop pause alert: the Eject phase
// filled the import/export station and is waiting for the operator to remove the
// exported tapes so the remaining ones can be exported (SPEC §4.3 phase 8, §11).
// It names the run, the tapes ready for removal, and how many still await a free
// slot. Like SendFailure it is best-effort — a delivery failure is logged, not
// returned — so a webhook outage never aborts a run that is otherwise healthy.
// When the client's URL is empty, it is a silent no-op.
func (c *Client) SendOperatorPause(ctx context.Context, runID string, readyForRemoval []string, awaiting int) {
	if c.url == "" {
		return
	}

	ready := "none"
	if len(readyForRemoval) > 0 {
		ready = strings.Join(readyForRemoval, ", ")
	}

	msg := Message{
		Content: fmt.Sprintf(
			"Backup run %s paused during Eject: the import/export station is full. "+
				"Remove the exported tape(s) [%s] from the station; %d more tape(s) "+
				"will be exported once slots free up.",
			runID, ready, awaiting),
	}

	if err := c.Send(ctx, msg); err != nil {
		slog.Error("failed to deliver webhook operator-pause alert",
			"run_id", runID,
			"awaiting", awaiting,
			"delivery_error", err,
		)
	}
}

// SendWritePathPause posts an operator-in-the-loop pause alert: a Load or Write
// failed for one drive-set, so the tape path paused and is waiting for the
// operator to swap the affected tapes for fresh blanks and resume, or to abort
// the run (SPEC §4.3, §11). It names the run, the failing phase, the affected
// tapes, the storage slots to restock with fresh blanks, and the error summary,
// and tells the operator the exact `tapectl resume`/`tapectl abort` commands.
// Like SendFailure it is best-effort — a delivery failure is logged, not returned
// — so a webhook outage never aborts a run that is only waiting. When the
// client's URL is empty, it is a silent no-op.
func (c *Client) SendWritePathPause(ctx context.Context, runID, phase string, affectedTapes []string, reloadSlots []int, errSummary string) {
	if c.url == "" {
		return
	}

	tapes := "none"
	if len(affectedTapes) > 0 {
		tapes = strings.Join(affectedTapes, ", ")
	}

	if errSummary == "" {
		errSummary = "unknown error"
	}

	errSummary = truncate(errSummary, maxSummaryLength)

	msg := Message{
		Content: fmt.Sprintf(
			"Backup run %s paused: %s failed for one drive-set. Remove the affected tape(s) [%s], "+
				"load fresh blank tape(s) into slot(s) %s, then run `tapectl resume` to continue "+
				"or `tapectl abort` to end the run. Error: %s",
			runID, phase, tapes, formatSlots(reloadSlots), errSummary),
	}

	if err := c.Send(ctx, msg); err != nil {
		slog.Error("failed to deliver webhook write-path pause alert",
			"run_id", runID,
			"phase", phase,
			"delivery_error", err,
		)
	}
}

// SendBurnPause posts an operator-in-the-loop pause alert for the optical Burn
// phase: either a burn/verify failed for one disc-set, or a disc-set completed
// and the next set needs fresh blanks loaded (there is no optical autoloader, so
// every set after the first requires a manual disc swap). It names the run, the
// burner drive(s) involved, and the reason, and tells the operator the exact
// `tapectl resume`/`tapectl abort` commands (SPEC §10, §11). Like SendFailure it
// is best-effort — a delivery failure is logged, not returned — so a webhook
// outage never aborts a run that is only waiting. When the client's URL is empty,
// it is a silent no-op.
func (c *Client) SendBurnPause(ctx context.Context, runID string, devices []string, reason string) {
	if c.url == "" {
		return
	}

	drives := "the burner(s)"
	if len(devices) > 0 {
		drives = strings.Join(devices, ", ")
	}

	if reason == "" {
		reason = "unknown reason"
	}

	reason = truncate(reason, maxSummaryLength)

	msg := Message{
		Content: fmt.Sprintf(
			"Backup run %s paused during Burn on drive(s) %s: %s. Load a blank recovery disc into each "+
				"named drive, then run `tapectl resume` to continue or `tapectl abort` to end the run.",
			runID, drives, reason),
	}

	if err := c.Send(ctx, msg); err != nil {
		slog.Error("failed to deliver webhook burn pause alert",
			"run_id", runID,
			"delivery_error", err,
		)
	}
}

// formatSlots renders a list of storage-slot addresses for an operator message.
// An empty list falls back to a generic phrase (a Load failure has no per-tape
// slots to name beyond the set's own).
func formatSlots(slots []int) string {
	if len(slots) == 0 {
		return "the drive-set's blank slots"
	}

	parts := make([]string, len(slots))
	for i, slot := range slots {
		parts[i] = strconv.Itoa(slot)
	}

	return strings.Join(parts, ", ")
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
