// Package webhook is a Discord webhook client for run-success delivery and
// failure alerts (SPEC §11). A Client with an empty URL is a no-op.
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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// maxContentLength is Discord's content limit, in code points.
	maxContentLength = 2000

	// maxSummaryLength caps embedded error text so the fixed resume/abort
	// instructions always fit under maxContentLength.
	maxSummaryLength = 1500

	truncationMarker = " […truncated; see logs]"
)

// truncate caps s at max runes (Discord counts code points, not bytes),
// appending truncationMarker when it cuts.
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

// codeBlock fences a multi-line error summary so Discord preserves its
// newlines. Pass the already-truncated summary so the fence is never cut.
func codeBlock(s string) string {
	return "\n```\n" + s + "\n```"
}

// Message is a Discord webhook message payload.
type Message struct {
	Content string `json:"content"`
}

// Client posts messages and file attachments to a Discord webhook.
type Client struct {
	url        string
	httpClient *http.Client
}

// New returns a Client for url; an empty URL yields a disabled no-op client.
func New(url string) *Client {
	return &Client{
		url: url,
		// No client-wide Timeout: it would also cap the body upload and undercut
		// the caller's context deadline, which is the sole timeout.
		httpClient: &http.Client{},
	}
}

// Send posts msg as JSON, returning an error on a non-2xx status or transport failure.
func (c *Client) Send(ctx context.Context, msg Message) error {
	if c.url == "" {
		return nil
	}

	// Backstop: Discord rejects content over its limit with HTTP 400.
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

// PostedMessage identifies a message created by a ?wait=true webhook execution,
// for building a jump-to-message link (the guild comes from FetchWebhookGuild).
type PostedMessage struct {
	// ID is the message snowflake ID.
	ID string
	// ChannelID is the channel the message landed in.
	ChannelID string
}

// SendFile uploads the file at path as an attachment with content as the message
// text, returning the posted message. A nil error always means delivered; the
// returned *PostedMessage is best-effort and may be nil.
func (c *Client) SendFile(ctx context.Context, path, content string) (*PostedMessage, error) {
	if c.url == "" {
		return nil, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("webhook: opening %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	var body bytes.Buffer

	writer := multipart.NewWriter(&body)

	// Discord takes multipart message fields as JSON in the payload_json part.
	if content != "" {
		payload, err := json.Marshal(Message{Content: truncate(content, maxContentLength)})
		if err != nil {
			return nil, fmt.Errorf("webhook: encoding message: %w", err)
		}

		if err := writer.WriteField("payload_json", string(payload)); err != nil {
			return nil, fmt.Errorf("webhook: writing message field: %w", err)
		}
	}

	part, err := writer.CreateFormFile("files[0]", filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("webhook: creating form file: %w", err)
	}

	if _, err := io.Copy(part, file); err != nil {
		return nil, fmt.Errorf("webhook: copying %q into request: %w", path, err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("webhook: finalizing multipart body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, withWait(c.url), &body)
	if err != nil {
		return nil, fmt.Errorf("webhook: creating request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	respBody, err := c.doReturningBody(req)
	if err != nil {
		return nil, err
	}

	var posted struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
	}
	if err := json.Unmarshal(respBody, &posted); err != nil {
		// Delivered; only the deep-link identity is lost, so don't fail the run.
		slog.WarnContext(ctx, "webhook: could not decode posted-message response; deep-link unavailable", "error", err)
		return nil, nil
	}

	return &PostedMessage{ID: posted.ID, ChannelID: posted.ChannelID}, nil
}

// FetchWebhookGuild returns the guild ID of the webhook's channel (which the
// ?wait=true response omits). Best-effort: the result may be "" and errors are
// safe to ignore.
func (c *Client) FetchWebhookGuild(ctx context.Context) (string, error) {
	if c.url == "" {
		return "", nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return "", fmt.Errorf("webhook: creating request: %w", err)
	}

	body, err := c.doReturningBody(req)
	if err != nil {
		return "", err
	}

	var hook struct {
		GuildID string `json:"guild_id"`
	}
	if err := json.Unmarshal(body, &hook); err != nil {
		return "", fmt.Errorf("webhook: decoding webhook object: %w", err)
	}

	return hook.GuildID, nil
}

// withWait sets wait=true so Discord returns the created message instead of a 204.
func withWait(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	query := parsed.Query()
	query.Set("wait", "true")
	parsed.RawQuery = query.Encode()

	return parsed.String()
}

// SendFailure posts a failure alert. It returns nothing so it can never mask the
// caller's error (SPEC §11); delivery failures are only logged.
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
		Content: fmt.Sprintf("Backup run %s failed in phase %q:%s", runID, phase, codeBlock(errSummary)),
	}

	if err := c.Send(ctx, msg); err != nil {
		slog.ErrorContext(ctx, "failed to deliver webhook failure alert",
			"run_id", runID,
			"phase", phase,
			"delivery_error", err,
		)
	}
}

// SendOperatorPause alerts that the Eject phase filled the import/export station
// and is waiting for the operator to remove tapes (SPEC §4.3, §11). Best-effort.
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
		slog.ErrorContext(ctx, "failed to deliver webhook operator-pause alert",
			"run_id", runID,
			"awaiting", awaiting,
			"delivery_error", err,
		)
	}
}

// SendWritePathPause alerts that a Load or Write failed for one drive-set and the
// run is waiting for the operator to swap tapes and resume or abort (SPEC §4.3,
// §11). Best-effort.
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
				"or `tapectl abort` to end the run. Error:%s",
			runID, phase, tapes, formatSlots(reloadSlots), codeBlock(errSummary)),
	}

	if err := c.Send(ctx, msg); err != nil {
		slog.ErrorContext(ctx, "failed to deliver webhook write-path pause alert",
			"run_id", runID,
			"phase", phase,
			"delivery_error", err,
		)
	}
}

// SendBurnPause alerts that the optical Burn phase is waiting for blank discs,
// after a failure or between disc-sets (no autoloader). Best-effort.
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
		slog.ErrorContext(ctx, "failed to deliver webhook burn pause alert",
			"run_id", runID,
			"delivery_error", err,
		)
	}
}

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

// StatusError is returned for a non-2xx webhook response.
type StatusError struct {
	Code int
}

// Error implements error.
func (e *StatusError) Error() string {
	return fmt.Sprintf("webhook: unexpected status %d", e.Code)
}

// Retryable reports whether the status is transient (429 or 5xx); other 4xx
// statuses fail identically on every attempt.
func (e *StatusError) Retryable() bool {
	return e.Code == http.StatusTooManyRequests || (e.Code >= 500 && e.Code <= 599)
}

// do sends req, returning *StatusError for a non-2xx status.
func (c *Client) do(req *http.Request) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &StatusError{Code: resp.StatusCode}
	}

	return nil
}

// doReturningBody is do but returns the 2xx response body.
func (c *Client) doReturningBody(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webhook: sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("webhook: reading response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &StatusError{Code: resp.StatusCode}
	}

	return body, nil
}
