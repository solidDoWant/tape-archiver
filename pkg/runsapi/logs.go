// This file implements GET /api/runs/{runID}/logs (issue #274): a
// server-side proxy over VictoriaLogs (LogsQL, https://docs.victoriametrics.com/victorialogs/logsql/)
// that returns the log lines an external collector shipped for one run (the
// whole run, or one of the 11 pipeline phases — SPEC §4.3), never a raw
// LogsQL passthrough. The VictoriaLogs URL/credentials are configured
// server-side only (VICTORIALOGS_URL, VICTORIALOGS_STREAM_FILTER — see
// docs/configuration.md) and never reach the browser.
//
// This is a plain JSON endpoint, not a Server-Sent Events stream like
// events.go's GET /api/events/runs/{runID}: the browser's EventSource API
// gives calling JS no access to a failed connection's HTTP status code, so
// an SSE-based design cannot distinguish "VictoriaLogs unconfigured/
// unreachable" (503, issue #274 AC1/AC2) from an ordinary transient network
// hiccup — exactly the distinction these ACs require. A plain fetch surfaces
// the status via api.ts's ApiError cleanly instead. The frontend log panel
// (web/src/LogPanel.tsx) gets "live update without a full page reload" (AC4)
// by polling this endpoint on an interval with ?since=<last line's time>
// while the response's "live" field is true, and stopping once it is false
// — a deliberate, documented departure from events.go's server-push
// precedent (used there to avoid many browser tabs each independently
// polling Temporal; VictoriaLogs has no equivalent concern here).
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
	"strings"
	"time"
)

// Environment variables read via handler.getenv (the same injectable getenv
// New wires to os.Getenv — see runsapi.go's "MHVTL_*" precedent), documented
// in docs/configuration.md.
const (
	// victoriaLogsURLEnv, when unset or empty, means VictoriaLogs is not
	// configured at all: getRunLogs reports 503 "unavailable" (issue #274
	// AC1) before making any Temporal or VictoriaLogs call.
	victoriaLogsURLEnv = "VICTORIALOGS_URL"
	// victoriaLogsStreamFilterEnv is an operator-controlled LogsQL filter
	// fragment ANDed onto every query this file issues (e.g. to scope
	// queries to a specific VictoriaLogs tenant/stream in a shared
	// deployment). It is server-side configuration, never client input, so
	// it is not validated/escaped the way runID and phase are below.
	victoriaLogsStreamFilterEnv = "VICTORIALOGS_STREAM_FILTER"
	// defaultVictoriaLogsStreamFilter matches every stream — the sensible
	// default for the common case of one VictoriaLogs instance dedicated to
	// this deployment.
	defaultVictoriaLogsStreamFilter = "*"
)

// maxLogLines hard-caps how many lines a single request can return,
// regardless of how many actually matched. There is deliberately no
// client-facing "limit" query parameter (issue #274's security note: "a
// bounded limit") — this is the only bound, applied server-side to every
// request the same way, which is simpler and safer than trusting a
// client-supplied value. 5000 lines comfortably covers one phase's or one
// run's console output for an operator glancing at recent activity (this
// is not a log search/analytics UI — see issue #274's non-goals) while
// keeping a single response bounded in size.
const maxLogLines = 5000

// victoriaLogsQueryPath is VictoriaLogs' LogsQL query HTTP API.
// https://docs.victoriametrics.com/victorialogs/querying/#http-api
const victoriaLogsQueryPath = "/select/logsql/query"

// runIDPattern matches a Temporal run ID's UUID form. getRunLogs rejects
// anything else with 400 before runID is ever interpolated into a LogsQL
// query string (buildLogsQLQuery) or passed to Temporal — this is stricter
// than every other /api/runs/{runID}/* route (which just let a malformed ID
// surface as Temporal's own InvalidArgument), because unlike those routes
// this one builds a *query language* string from runID rather than only
// ever passing it as an opaque RPC argument. Restricting it to hex digits
// and hyphens makes LogsQL injection structurally impossible regardless of
// any nuance in LogsQL's own string-quoting rules, rather than relying
// solely on %q-style escaping being correct for VictoriaLogs' syntax.
var runIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// errVictoriaLogsUnavailable is the stable, distinguishable error
// getRunLogs reports (as a 503) whether VictoriaLogs is unconfigured
// (VICTORIALOGS_URL unset) or configured but unreachable/erroring — issue
// #274 AC1/AC2 treat both the same way ("an explicit unavailable state"),
// so collapsing them here is correct, not a shortcut: a client cannot do
// anything different for one case versus the other anyway.
var errVictoriaLogsUnavailable = errors.New("victorialogs is not configured or is unreachable")

// LogLine is one matched log line, projected down from VictoriaLogs' own
// record (which also carries stream labels, the ingest-assigned
// "_stream_id", and any other field the worker happened to log) to just
// what the log panel renders — issue #274's non-goals exclude a general log
// search/filter UI, so there is no reason to forward the rest to the
// browser.
type LogLine struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level,omitempty"`
	Message string    `json:"message"`
	// Error is the log entry's error detail, when it carries one. Many of the
	// most operator-relevant lines — a failing/retrying activity above all —
	// log a terse _msg ("Activity error.") and put the actual cause in a
	// structured field rather than the message text, so projecting _msg alone
	// (as this endpoint first did) hid it. This surfaces that field so the log
	// panel can show the cause without every call site having to inline it into
	// its message: the Temporal SDK's own logger names it "Error", and this
	// repo's slog error logs conventionally use "error" (slog.Error(msg,
	// "error", err)); parseLogsQLResponse reads whichever is present. Empty when
	// the line logged no error field.
	Error string `json:"error,omitempty"`
}

// RunLogsResponse is the GET /api/runs/{runID}/logs response body.
type RunLogsResponse struct {
	RunID string `json:"runId"`
	// Phase is the phase name the request was scoped to, or "" for the
	// whole-run window (no ?phase= given).
	Phase string `json:"phase,omitempty"`
	// Lines are the matched log lines, oldest first (LogsQL's own "sort by
	// (_time)" — VictoriaLogs, not this handler, orders them).
	Lines []LogLine `json:"lines"`
	// Live is true while more lines can still arrive for this exact window
	// (the run, or the requested phase, has not finished yet) — the signal
	// web/src/LogPanel.tsx polls on to decide whether to keep re-fetching
	// with ?since=<last line's time>, or stop.
	Live bool `json:"live"`
}

// getRunLogs implements GET /api/runs/{runID}/logs?phase=&since=.
//
// phase, if given, must be one of the 11 pipeline phase names (phaseOrder);
// omitted, the window is the whole run. since, if given, is an RFC3339
// timestamp: only lines at or after it (inclusive — see buildLogsQLQuery's
// doc comment for why not exclusive) are returned, letting a client
// (LogPanel's poll loop) fetch just the new tail rather than the whole
// window again; a polling caller must therefore deduplicate lines sharing
// its since timestamp.
func (h *handler) getRunLogs(w http.ResponseWriter, r *http.Request) {
	// Checked first, before any Temporal RPC: the cheapest possible way to
	// answer "is this even configured" (issue #274 AC1).
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
		// The requested phase has not started yet (or, for the whole-run
		// case, this branch is unreachable — a run always has a start
		// time). This is a genuinely empty result, not an availability
		// problem: LogPanel shows its own distinct "no log lines yet"
		// empty state for Live == false, Lines == [].
		writeJSON(w, http.StatusOK, RunLogsResponse{RunID: runID, Phase: phase, Lines: []LogLine{}, Live: false})

		return
	}

	streamFilter := h.getenv(victoriaLogsStreamFilterEnv)
	if streamFilter == "" {
		streamFilter = defaultVictoriaLogsStreamFilter
	}

	lines, err := queryVictoriaLogs(ctx, victoriaLogsURL, streamFilter, runID, window.start, window.end, since, maxLogLines)
	if err != nil {
		// Any failure to reach or parse VictoriaLogs' response, now that
		// VICTORIALOGS_URL is confirmed set, is reported exactly like an
		// unconfigured VictoriaLogs (issue #274 AC2: "unreachable" and
		// "unconfigured" show the same explicit state) rather than as a
		// generic 502/500 — a client cannot act on the distinction anyway.
		slog.WarnContext(ctx, "runsapi: victorialogs query failed", "run_id", runID, "phase", phase, "error", err)
		writeError(w, http.StatusServiceUnavailable, errVictoriaLogsUnavailable)

		return
	}

	if window.spans != nil {
		lines = filterLinesToSpans(lines, window.spans)
	}

	writeJSON(w, http.StatusOK, RunLogsResponse{RunID: runID, Phase: phase, Lines: lines, Live: window.live})
}

// logWindow is the [start, end] time range getRunLogs queries VictoriaLogs
// over, whether that range can still grow, and — for a phase-scoped
// request — the disjoint sub-ranges within it that actually belong to the
// phase.
type logWindow struct {
	start time.Time
	end   *time.Time // nil means "still open" (the run/phase has not closed)
	live  bool
	// spans, when non-nil, are the phase's own disjoint per-activity time
	// ranges within [start, end]. The tape path interleaves Load/Write/Eject
	// per drive-set (SPEC §4.3 phases 6-8, and a Load/Write-failure pause
	// retries onto fresh blanks), so one phase's activities are NOT
	// contiguous: on a run whose first Write failed and was retried after a
	// pause, "Load"'s single [earliest, latest] envelope would swallow the
	// failed Write's and the pause's log lines, and vice versa. getRunLogs
	// therefore issues ONE VictoriaLogs query over the whole envelope (a
	// per-span query fan-out buys nothing) and post-filters the results to
	// lines falling inside any of these spans (filterLinesToSpans). nil (the
	// whole-run mode) means no filtering — every line in the window belongs.
	spans []timeSpan
}

// timeSpan is one activity's [start, end] range. A zero end means the
// activity has not reached a terminal state yet — the span is still open.
type timeSpan struct {
	start time.Time
	end   time.Time
}

// filterLinesToSpans keeps only the lines whose timestamp falls inside at
// least one span (inclusive on both ends — a line logged at the exact
// scheduled/terminal instant belongs to the activity).
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

// resolveLogWindow determines the time window to query for runID/phase,
// reusing the same run/phase-status machinery the rest of this package
// already exposes rather than deriving it a third way:
//   - phase == "": the whole run's window, via fetchRunDetail (the same
//     call GET /api/runs/{runID} and its SSE stream make) — StartTime to
//     CloseTime, open (live) while CloseTime is nil.
//   - phase != "": that phase's window, via fetchRunHistory + the phases.go
//     timeline builder — StartTime to EndTime, live exactly while the
//     phase's computed status is PhaseActive.
//
// ok is false only when the requested phase has not started yet (no window
// to query at all yet); err carries a fetchRunDetail/fetchRunHistory
// failure (unknown run ID, aged-out history, ...), which the caller maps to
// an HTTP status using the same classification the corresponding
// non-logs endpoint already uses.
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

	// outcomes (nil here) only feed PhaseInfo.Facts, which this endpoint
	// never reads — buildPhaseTimeline ranges over a nil slice harmlessly.
	for _, info := range buildPhaseTimeline(history, nil) {
		if info.Name != phase {
			continue
		}

		if info.StartTime == nil {
			return logWindow{}, false, nil
		}

		return logWindow{
			start: *info.StartTime,
			end:   info.EndTime,
			live:  info.Status == PhaseActive,
			spans: phaseActivitySpans(history, phase),
		}, true, nil
	}

	// Unreachable: getRunLogs already rejected any phase not in phaseOrder
	// (phaseIndex(phase) < 0) before calling this, and buildPhaseTimeline
	// always emits exactly one PhaseInfo per phaseOrder entry.
	return logWindow{}, false, nil
}

// phaseActivitySpans collects the disjoint per-activity time ranges of one
// phase's own activities — the same attribution buildPhaseTimeline uses
// (phaseForActivity, including the NotifyWritePathPause input-based
// sub-phase routing) — so a phase-scoped log query never swallows the lines
// another phase's activity emitted inside this phase's overall envelope
// (see logWindow.spans' doc comment for the interleaved-tape-path case
// that makes this necessary).
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

// queryVictoriaLogs issues one LogsQL query (buildLogsQLQuery) against
// VictoriaLogs' HTTP query API and decodes its newline-delimited JSON
// response into LogLines, oldest first.
func queryVictoriaLogs(ctx context.Context, baseURL, streamFilter, runID string, start time.Time, end *time.Time, since *time.Time, limit int) ([]LogLine, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+victoriaLogsQueryPath, nil)
	if err != nil {
		return nil, fmt.Errorf("build victorialogs request: %w", err)
	}

	query := request.URL.Query()
	query.Set("query", buildLogsQLQuery(streamFilter, runID, start, end, since, limit))
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

	return parseLogsQLResponse(response.Body)
}

// buildLogsQLQuery composes the LogsQL query string for one request:
// streamFilter (operator config) ANDed with an exact match on RunID (never
// client-supplied LogsQL — runID is validated as a UUID by the caller
// before this is ever invoked) and a time-range clause, sorted oldest first
// and capped at limit. since, when set and later than start, replaces the
// lower bound.
//
// The lower bound is always INCLUSIVE (">="/"["), even for since — a
// deliberate choice, not an off-by-one: a caller polling with "since = the
// last line's own timestamp" would, with an exclusive bound, permanently
// lose any same-timestamp lines that had not been ingested yet when the
// previous poll ran (log shipping is asynchronous and batched). Re-sending
// the boundary lines instead and letting the client deduplicate (LogPanel
// dedups by time+message identity) means a split same-timestamp batch is
// eventually complete rather than silently truncated.
func buildLogsQLQuery(streamFilter, runID string, start time.Time, end *time.Time, since *time.Time, limit int) string {
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

	return fmt.Sprintf("(%s) AND RunID:=%q AND %s | sort by (_time) | limit %d", streamFilter, runID, timeFilter, limit)
}

// formatVLTime renders t the way LogsQL time-range literals expect
// (RFC3339, UTC) — verified against a real VictoriaLogs instance (issue
// #274's task notes).
func formatVLTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// parseVLTime parses a timestamp VictoriaLogs itself produced (a query
// response's "_time" field) or a client-supplied ?since= value, accepting
// both the fractional-second form VictoriaLogs emits and plain RFC3339.
func parseVLTime(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, nil
	}

	return time.Parse(time.RFC3339, raw)
}

// vlRecord is one line of VictoriaLogs' LogsQL query response, decoded down
// to the fields LogLine needs. VictoriaLogs' own JSON stream
// ingestion (docker-compose.web-dev.yml's vector container,
// docs/web-ui.md) is configured with _msg_field=msg and _time_field=time
// (see this package's doc comment), so a worker's slog JSON "msg"/"time"
// keys surface here as "_msg"/"_time"; "level" passes through unchanged,
// slog's own field name — as do "Error"/"error" (see LogLine.Error).
type vlRecord struct {
	Time  string `json:"_time"`
	Msg   string `json:"_msg"`
	Level string `json:"level"`
	// Error / ErrorLower are the two conventions a log line's error detail
	// arrives under: the Temporal SDK's own logger uses "Error", this repo's
	// slog error logs use "error". At most one is set on any given line;
	// parseLogsQLResponse prefers "Error" and falls back to "error".
	Error      string `json:"Error"`
	ErrorLower string `json:"error"`
}

// parseLogsQLResponse decodes VictoriaLogs' LogsQL query response: one JSON
// object per line (https://docs.victoriametrics.com/victorialogs/querying/#json-stream-decoding),
// not a single JSON array/document.
func parseLogsQLResponse(body io.Reader) ([]LogLine, error) {
	lines := make([]LogLine, 0)

	scanner := bufio.NewScanner(body)
	// A generous max line length: the default 64KiB bufio.Scanner token
	// limit is fine for ordinary log lines but a single pathological line
	// (e.g. a stack trace embedded in one structured field) must not abort
	// the whole response.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for scanner.Scan() {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}

		var record vlRecord

		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, fmt.Errorf("decode victorialogs response line: %w", err)
		}

		parsedTime, err := parseVLTime(record.Time)
		if err != nil {
			return nil, fmt.Errorf("parse victorialogs record time %q: %w", record.Time, err)
		}

		errText := record.Error
		if errText == "" {
			errText = record.ErrorLower
		}

		lines = append(lines, LogLine{Time: parsedTime, Level: record.Level, Message: record.Msg, Error: errText})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read victorialogs response: %w", err)
	}

	return lines, nil
}
