// This file implements GET /api/runs/{runID}/logs (issue #274): a server-side
// proxy over VictoriaLogs returning one run's (or one phase's) log lines. It is a
// plain JSON endpoint polled by the frontend rather than SSE, because EventSource
// cannot expose the HTTP status needed to distinguish a 503 "unavailable" from a
// transient network error.
package runsapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Environment variables, documented in docs/configuration.md.
const (
	// victoriaLogsURLEnv unset or empty means VictoriaLogs is not configured.
	victoriaLogsURLEnv = "VICTORIALOGS_URL"
	// victoriaLogsStreamFilterEnv is an operator-controlled LogsQL fragment ANDed
	// onto every query. It is server-side config, so it is not escaped.
	victoriaLogsStreamFilterEnv = "VICTORIALOGS_STREAM_FILTER"
	// defaultVictoriaLogsStreamFilter matches every stream.
	defaultVictoriaLogsStreamFilter = "*"
	// victoriaLogsFieldPrefixEnv prefixes every worker-originated field name
	// (RunID/level/msg/Error/error), for collectors such as fluentbit with
	// Merge_Log_Key that nest parsed keys (e.g. "log_fields.RunID") and leave _msg
	// holding the raw JSON line. VictoriaLogs-owned fields are never prefixed.
	victoriaLogsFieldPrefixEnv = "VICTORIALOGS_FIELD_PREFIX"
)

// maxLogLines is the server-side cap on returned lines; there is deliberately
// no client-supplied limit (issue #274 security note). When exceeded, the
// NEWEST lines are kept so a failed run's trailing error is never truncated.
const maxLogLines = 5000

// victoriaLogsQueryPath is VictoriaLogs' LogsQL query HTTP API.
// https://docs.victoriametrics.com/victorialogs/querying/#http-api
const victoriaLogsQueryPath = "/select/logsql/query"

// vlQueryTimeout is well inside requestTimeout so a slow VictoriaLogs still
// leaves time to degrade to 503.
const vlQueryTimeout = 5 * time.Second

// runIDPattern restricts runID to a UUID before it is interpolated into a
// LogsQL string, making query injection structurally impossible rather than
// relying on escaping.
var runIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// errVictoriaLogsUnavailable covers both unconfigured and unreachable
// VictoriaLogs; issue #274 AC1/AC2 treat them identically.
var errVictoriaLogsUnavailable = errors.New("victorialogs is not configured or is unreachable")

// LogLine is one matched log line, projected to the fields the log panel renders.
type LogLine struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level,omitempty"`
	Message string    `json:"message"`
	// Error is the entry's structured error field ("Error" from the Temporal SDK
	// logger, "error" from this repo's slog calls), since many failure lines log
	// only a terse message like "Activity error.".
	Error string `json:"error,omitempty"`
}

// RunLogsResponse is the GET /api/runs/{runID}/logs response body.
type RunLogsResponse struct {
	RunID string `json:"runId"`
	// Phase is the requested phase, or "" for the whole run.
	Phase string `json:"phase,omitempty"`
	// Lines are the matched log lines, oldest first.
	Lines []LogLine `json:"lines"`
	// Live is true while more lines can still arrive for this window; the
	// frontend keeps polling with ?since= while it is set.
	Live bool `json:"live"`
}

// getRunLogs implements GET /api/runs/{runID}/logs?phase=&since=. since is an
// inclusive RFC3339 lower bound, so polling callers must deduplicate lines at
// that timestamp.
func (h *handler) getRunLogs(w http.ResponseWriter, r *http.Request) {
	// Checked before any Temporal RPC (issue #274 AC1).
	victoriaLogsURL := strings.TrimSuffix(h.getenv(victoriaLogsURLEnv), "/")
	if victoriaLogsURL == "" {
		writeError(w, http.StatusServiceUnavailable, errVictoriaLogsUnavailable)

		return
	}

	runID := r.PathValue("runID")
	if runID == "" {
		writeError(w, http.StatusBadRequest, errors.New("runID is required"))

		return
	}

	if !runIDPattern.MatchString(runID) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid run ID %q: must be a UUID", runID))

		return
	}

	phase := r.URL.Query().Get("phase")
	if phase != "" && phaseIndex(phase) < 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown phase %q", phase))

		return
	}

	var since *time.Time

	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := parseVLTime(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid since %q: must be RFC3339: %w", raw, err))

			return
		}

		since = &parsed
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	window, ok, err := h.resolveLogWindow(ctx, runID, phase)
	if err != nil {
		if phase == "" {
			writeRunDetailError(w, runID, err)
		} else {
			writeHistoryError(ctx, w, h.temporalClient, runID, err)
		}

		return
	}

	if !ok {
		// The phase has not started. Live carries the run's liveness so the
		// client keeps polling until the phase starts or the run closes.
		writeJSON(w, http.StatusOK, RunLogsResponse{RunID: runID, Phase: phase, Lines: []LogLine{}, Live: window.live})

		return
	}

	streamFilter := h.getenv(victoriaLogsStreamFilterEnv)
	if streamFilter == "" {
		streamFilter = defaultVictoriaLogsStreamFilter
	}

	fieldPrefix := h.getenv(victoriaLogsFieldPrefixEnv)

	lines, err := queryVictoriaLogs(ctx, victoriaLogsURL, streamFilter, fieldPrefix, runID, window.start, window.end, since, window.spans, maxLogLines)
	if err != nil {
		// Unreachable is reported like unconfigured (issue #274 AC2).
		slog.WarnContext(ctx, "runsapi: victorialogs query failed", "run_id", runID, "phase", phase, "error", err)
		writeError(w, http.StatusServiceUnavailable, errVictoriaLogsUnavailable)

		return
	}

	if window.spans != nil {
		lines = filterLinesToSpans(lines, window.spans)
	}

	writeJSON(w, http.StatusOK, RunLogsResponse{RunID: runID, Phase: phase, Lines: lines, Live: window.live})
}

// logWindow is the time range getRunLogs queries and whether it can still grow.
type logWindow struct {
	start time.Time
	end   *time.Time // nil means still open
	live  bool
	// spans, when non-nil, are the phase's own per-activity ranges. The tape
	// path interleaves Load/Write/Eject per drive-set (and retries after a
	// pause), so a phase's [earliest, latest] envelope would swallow other
	// phases' lines; results are post-filtered to these spans.
	spans []timeSpan
}

// timeSpan is one activity's range; a zero end means it is still open.
type timeSpan struct {
	start time.Time
	end   time.Time
}

// filterLinesToSpans keeps lines inside at least one span (inclusive both ends).
func filterLinesToSpans(lines []LogLine, spans []timeSpan) []LogLine {
	filtered := make([]LogLine, 0, len(lines))

	for _, line := range lines {
		for _, span := range spans {
			if line.Time.Before(span.start) {
				continue
			}

			if !span.end.IsZero() && line.Time.After(span.end) {
				continue
			}

			filtered = append(filtered, line)

			break
		}
	}

	return filtered
}

// resolveLogWindow returns the whole run's window (phase == "") or the phase's
// window from the phase timeline. ok is false when the phase has not started.
func (h *handler) resolveLogWindow(ctx context.Context, runID, phase string) (logWindow, bool, error) {
	if phase == "" {
		detail, err := fetchRunDetail(ctx, h.temporalClient, runID)
		if err != nil {
			return logWindow{}, false, err
		}

		return logWindow{start: detail.StartTime, end: detail.CloseTime, live: detail.CloseTime == nil}, true, nil
	}

	history, err := fetchRunHistory(ctx, h.temporalClient, runID)
	if err != nil {
		return logWindow{}, false, err
	}

	for _, info := range buildPhaseTimeline(history, nil) {
		if info.Name != phase {
			continue
		}

		if info.StartTime == nil {
			// A running run may still reach this phase, so keep the client polling.
			return logWindow{live: !history.Closed}, false, nil
		}

		return logWindow{
			start: *info.StartTime,
			end:   info.EndTime,
			live:  info.Status == PhaseActive,
			spans: phaseActivitySpans(history, phase),
		}, true, nil
	}

	// Unreachable: getRunLogs already validated phase against phaseOrder.
	return logWindow{}, false, nil
}

// phaseActivitySpans collects the per-activity time ranges of one phase's
// activities, using the same attribution as buildPhaseTimeline.
func phaseActivitySpans(history runHistory, phase string) []timeSpan {
	spans := make([]timeSpan, 0)

	for _, record := range history.Activities {
		recordPhase, ok := phaseForActivity(record.Name, record.Input)
		if !ok || recordPhase != phase {
			continue
		}

		if record.ScheduledTime.IsZero() {
			continue
		}

		spans = append(spans, timeSpan{start: record.ScheduledTime, end: record.EndTime})
	}

	return spans
}

// queryVictoriaLogs runs one LogsQL query and returns the lines oldest first.
func queryVictoriaLogs(ctx context.Context, baseURL, streamFilter, fieldPrefix, runID string, start time.Time, end *time.Time, since *time.Time, spans []timeSpan, limit int) ([]LogLine, error) {
	queryCtx, cancel := context.WithTimeout(ctx, vlQueryTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(queryCtx, http.MethodGet, baseURL+victoriaLogsQueryPath, nil)
	if err != nil {
		return nil, fmt.Errorf("build victorialogs request: %w", err)
	}

	query := request.URL.Query()
	query.Set("query", buildLogsQLQuery(streamFilter, fieldPrefix, runID, start, end, since, spans, limit))
	request.URL.RawQuery = query.Encode()

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("query victorialogs: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))

		return nil, fmt.Errorf("victorialogs returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}

	return parseLogsQLResponse(response.Body, fieldPrefix)
}

// buildLogsQLQuery composes the LogsQL query for one request.
//
// "sort by (_time) desc | limit N | sort by (_time)" keeps the newest N lines,
// returned oldest first. The lower bound is inclusive even for since: log
// shipping is asynchronous, so an exclusive bound would permanently lose
// same-timestamp lines ingested after the previous poll; the client dedups.
// spans are ANDed in server-side so the limit counts only in-phase lines;
// otherwise an interleaved high-volume phase could crowd them out entirely.
func buildLogsQLQuery(streamFilter, fieldPrefix, runID string, start time.Time, end *time.Time, since *time.Time, spans []timeSpan, limit int) string {
	lower := start
	if since != nil && since.After(start) {
		lower = *since
	}

	var timeFilter string
	if end != nil {
		timeFilter = fmt.Sprintf("_time:[%s, %s]", formatVLTime(lower), formatVLTime(*end))
	} else {
		timeFilter = fmt.Sprintf("_time:>=%s", formatVLTime(lower))
	}

	if spanFilter := spansTimeFilter(spans); spanFilter != "" {
		timeFilter = fmt.Sprintf("%s AND %s", timeFilter, spanFilter)
	}

	return fmt.Sprintf("(%s) AND %s:=%q AND %s | sort by (_time) desc | limit %d | sort by (_time)", streamFilter, prefixedFieldName(fieldPrefix, "RunID"), runID, timeFilter, limit)
}

// prefixedFieldName returns name under fieldPrefix, quoted when prefixed so the
// dot is a literal field-name character rather than LogsQL syntax.
func prefixedFieldName(fieldPrefix, name string) string {
	if fieldPrefix == "" {
		return name
	}

	return strconv.Quote(fieldPrefix + name)
}

// spansTimeFilter renders spans as a LogsQL OR of time ranges (an open span
// becomes a lower-bound clause), or "" when there are none.
func spansTimeFilter(spans []timeSpan) string {
	if len(spans) == 0 {
		return ""
	}

	clauses := make([]string, 0, len(spans))
	for _, span := range spans {
		if span.end.IsZero() {
			clauses = append(clauses, fmt.Sprintf("_time:>=%s", formatVLTime(span.start)))

			continue
		}

		clauses = append(clauses, fmt.Sprintf("_time:[%s, %s]", formatVLTime(span.start), formatVLTime(span.end)))
	}

	return "(" + strings.Join(clauses, " OR ") + ")"
}

func formatVLTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// parseVLTime accepts both RFC3339Nano (as VictoriaLogs emits) and RFC3339.
func parseVLTime(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, nil
	}

	return time.Parse(time.RFC3339, raw)
}

// maxLogLineBytes bounds one NDJSON record. Oversized records are skipped
// without aborting the scan, so one huge line cannot blank the whole response.
const maxLogLineBytes = 1 << 20

// parseLogsQLResponse decodes VictoriaLogs' newline-delimited JSON response.
// https://docs.victoriametrics.com/victorialogs/querying/#json-stream-decoding
func parseLogsQLResponse(body io.Reader, fieldPrefix string) ([]LogLine, error) {
	lines := make([]LogLine, 0)
	reader := bufio.NewReader(body)

	for {
		raw, tooLong, err := readBoundedLine(reader, maxLogLineBytes)

		if tooLong {
			slog.Warn("runsapi: skipping oversized victorialogs response line", "limit_bytes", maxLogLineBytes)
		} else if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 {
			if line, ok := parseLogsQLRecord(trimmed, fieldPrefix); ok {
				lines = append(lines, line)
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return lines, nil
			}

			return nil, fmt.Errorf("read victorialogs response: %w", err)
		}
	}
}

// parseLogsQLRecord decodes one NDJSON record. ok is false for a malformed
// record, which the caller skips so one bad record cannot hide the rest.
// With a field prefix the message is read from "<prefix>msg", falling back to
// _msg only when absent (under a merge-key pipeline _msg is the raw JSON line).
func parseLogsQLRecord(raw []byte, fieldPrefix string) (LogLine, bool) {
	var fields map[string]json.RawMessage

	if err := json.Unmarshal(raw, &fields); err != nil {
		slog.Warn("runsapi: skipping undecodable victorialogs response line", "error", err)

		return LogLine{}, false
	}

	rawTime, _ := stringField(fields, "_time")

	parsedTime, err := parseVLTime(rawTime)
	if err != nil {
		slog.Warn("runsapi: skipping victorialogs record with unparseable time", "time", rawTime, "error", err)

		return LogLine{}, false
	}

	level, _ := stringField(fields, fieldPrefix+"level")

	errText, ok := stringField(fields, fieldPrefix+"Error")
	if !ok {
		errText, _ = stringField(fields, fieldPrefix+"error")
	}

	message, _ := stringField(fields, "_msg")
	if fieldPrefix != "" {
		if prefixed, present := stringField(fields, fieldPrefix+"msg"); present {
			message = prefixed
		}
	}

	return LogLine{Time: parsedTime, Level: level, Message: message, Error: errText}, true
}

// stringField reads a string field; a missing or non-string value reports
// present == false rather than an error.
func stringField(fields map[string]json.RawMessage, name string) (value string, present bool) {
	raw, ok := fields[name]
	if !ok {
		return "", false
	}

	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}

	return value, true
}

// readBoundedLine reads one line capped at limit bytes. A longer line is fully
// consumed but returned as tooLong with a nil payload. err is io.EOF after the
// final line.
func readBoundedLine(reader *bufio.Reader, limit int) (line []byte, tooLong bool, err error) {
	for {
		// ErrBufferFull lets this stream through an arbitrarily long line.
		chunk, readErr := reader.ReadSlice('\n')

		if !tooLong {
			if len(line)+len(chunk) > limit {
				tooLong = true
				line = nil
			} else {
				line = append(line, chunk...)
			}
		}

		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}

		return line, tooLong, readErr
	}
}
