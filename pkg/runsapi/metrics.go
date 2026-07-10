// This file implements GET /api/runs/{runID}/metrics/drives and GET
// /api/runs/{runID}/metrics/drives/{barcode}/history (issue #275): a thin,
// read-only proxy over VictoriaMetrics for the live write-health gauges the
// data worker already exports (workflows/backup/writehealth.go —
// tape_archiver_write_throughput_mbps/repositions/tapealert_flags/
// below_floor, all labeled by barcode). No new metric instrumentation is
// added anywhere; this only queries what already exists.
//
// cmd/web must never become an open PromQL proxy: every query issued here is
// built server-side from a fixed metric-name allowlist and a barcode this
// run's own Temporal history actually loaded (deriveTapeOutcomes, shared with
// tapes.go) — a client can never supply raw PromQL, an arbitrary metric name,
// or a barcode outside this run. The sparkline range/step are also fixed
// server-side constants, never client-controlled, keeping this a narrow
// "current run's drive view" (issue #275's non-goal: no historical/long-range
// metrics explorer).
//
// VictoriaMetrics is optional observability, not a dependency this API can
// fail without: an unset VICTORIAMETRICS_URL always yields the same stable
// 503 response (errVMUnconfigured), and any failure actually reaching or
// parsing a VictoriaMetrics response also degrades to 503, never 500 — a
// misbehaving or unreachable metrics backend must never make the run detail
// API itself look broken.
package runsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// victoriaMetricsEnvVar names the environment variable carrying
// VictoriaMetrics's base URL (e.g. "http://127.0.0.1:8428"), read via the
// same injectable h.getenv every other env-gated handler in this package
// uses (runsapi.go's dry-run mhvtl gate). Unset means metrics are not
// configured for this deployment.
const victoriaMetricsEnvVar = "VICTORIAMETRICS_URL"

// errVMUnconfigured is returned verbatim (same message, same 503 status)
// whenever VICTORIAMETRICS_URL is unset, so "metrics unavailable because
// unconfigured" is a single stable response shape a client/test can assert
// on directly, per issue #275 AC1.
var errVMUnconfigured = errors.New("VictoriaMetrics is not configured (VICTORIAMETRICS_URL is unset)")

// The four write-health gauges workflows/backup/writehealth.go registers,
// all labeled "barcode". These names must stay in sync with that file's
// prometheus.GaugeOpts (Namespace "tape_archiver", Subsystem "write") — the
// fixed allowlist queries built below never accept a client-supplied metric
// name.
const (
	metricThroughput  = "tape_archiver_write_throughput_mbps"
	metricRepositions = "tape_archiver_write_repositions"
	metricTapeAlerts  = "tape_archiver_write_tapealert_flags"
	metricBelowFloor  = "tape_archiver_write_below_floor"
)

// sparklineMetricAllowlist maps the history endpoint's public `metric` query
// parameter to the underlying Prometheus metric name, so a client picks one
// of a fixed, named set rather than ever supplying (or probing for) a raw
// metric name. defaultSparklineMetric is used when the parameter is absent —
// the design's write-rate sparkline (DESIGN_ANALYSIS.md §3) only ever needs
// throughput, but the other three are exposed the same safe way for reuse.
var sparklineMetricAllowlist = map[string]string{
	"throughput":  metricThroughput,
	"repositions": metricRepositions,
	"tapealerts":  metricTapeAlerts,
	"belowfloor":  metricBelowFloor,
}

const defaultSparklineMetric = "throughput"

// sparklinePoints/sparklineStep fix the write-rate sparkline's shape to the
// design's 8-bar chart (DESIGN_ANALYSIS.md §3: "8-bar sparkline"): a
// hard-coded, server-side-only range/step, never client-controlled, so this
// endpoint cannot be used to page back through arbitrary history (issue
// #275's explicit non-goal).
const (
	sparklinePoints = 8
	sparklineStep   = 90 * time.Second
)

// vmQueryTimeout bounds a single VictoriaMetrics HTTP request, well inside
// requestTimeout so a slow/unreachable VictoriaMetrics cannot itself exhaust
// the whole request's budget before this handler gets a chance to degrade to
// 503.
const vmQueryTimeout = 5 * time.Second

// DriveMetric is one physical tape's live write-health reading, merging this
// run's own history (barcode/drive/tape identity and the speed-matching
// floor, which VictoriaMetrics does not carry) with whatever VictoriaMetrics
// currently reports for that barcode's gauges.
type DriveMetric struct {
	Barcode    string `json:"barcode"`
	TapeIndex  int    `json:"tapeIndex"`
	CopyIndex  int    `json:"copyIndex"`
	DriveIndex int    `json:"driveIndex"`
	// Result mirrors TapeOutcome.Result ("loaded", "written", or "failed")
	// for this barcode, so a client can tell a still-in-flight tape apart
	// from one already finalized without a second request.
	Result string `json:"result"`

	// HasData is true when VictoriaMetrics returned at least one of the four
	// gauges for this barcode. False means the tape has not been measured
	// yet (still writing, or the run never reached MeasureWriteHealth for
	// it) — distinct from a zero/false reading.
	HasData bool `json:"hasData"`

	ThroughputMBps     *float64 `json:"throughputMBps,omitempty"`
	Repositions        *int64   `json:"repositions,omitempty"`
	TapeAlertFlagCount *int     `json:"tapeAlertFlagCount,omitempty"`
	BelowFloor         *bool    `json:"belowFloor,omitempty"`

	// FloorMBps/FloorKnown are this tape's speed-matching floor, sourced
	// from the run's own history (backup.WriteHealth via tapes.go's
	// TapeOutcome), not from VictoriaMetrics — the floor is a static,
	// generation-derived constant (workflows/backup/writehealth.go), never
	// exported as its own gauge.
	FloorMBps  float64 `json:"floorMBps,omitempty"`
	FloorKnown bool    `json:"floorKnown"`
}

// DriveMetricsResponse is the GET /api/runs/{runID}/metrics/drives response
// body.
type DriveMetricsResponse struct {
	RunID  string        `json:"runId"`
	Drives []DriveMetric `json:"drives"`
}

// MetricPoint is one sample in a GET
// /api/runs/{runID}/metrics/drives/{barcode}/history series.
type MetricPoint struct {
	Time  time.Time `json:"time"`
	Value float64   `json:"value"`
}

// DriveMetricHistoryResponse is the GET
// /api/runs/{runID}/metrics/drives/{barcode}/history response body.
type DriveMetricHistoryResponse struct {
	RunID   string        `json:"runId"`
	Barcode string        `json:"barcode"`
	Metric  string        `json:"metric"`
	Points  []MetricPoint `json:"points"`
}

// getRunDriveMetrics implements GET /api/runs/{runID}/metrics/drives:
// current write-health readings for every tape this run has loaded, live
// from VictoriaMetrics (issue #275 AC3/AC4).
func (h *handler) getRunDriveMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	runID := r.PathValue("runID")
	if runID == "" {
		writeError(w, http.StatusBadRequest, errors.New("runID is required"))

		return
	}

	vmURL := h.getenv(victoriaMetricsEnvVar)
	if vmURL == "" {
		writeError(w, http.StatusServiceUnavailable, errVMUnconfigured)

		return
	}

	history, err := fetchRunHistory(ctx, h.temporalClient, runID)
	if err != nil {
		writeHistoryError(ctx, w, h.temporalClient, runID, err)

		return
	}

	outcomes := deriveTapeOutcomes(history.Activities)
	if len(outcomes) == 0 {
		// No tape has been loaded yet (e.g. the run has not reached the
		// Load/Write phases) — a normal, not-writing-yet state, not an
		// error (issue #275's "no-data" state).
		writeJSON(w, http.StatusOK, DriveMetricsResponse{RunID: runID})

		return
	}

	barcodes := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		barcodes = append(barcodes, outcome.Barcode)
	}

	samples, err := queryDriveSamples(ctx, vmURL, barcodes)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("query VictoriaMetrics: %w", err))

		return
	}

	drives := make([]DriveMetric, 0, len(outcomes))
	for _, outcome := range outcomes {
		drives = append(drives, toDriveMetric(outcome, samples[outcome.Barcode]))
	}

	sort.Slice(drives, func(i, j int) bool {
		if drives[i].TapeIndex != drives[j].TapeIndex {
			return drives[i].TapeIndex < drives[j].TapeIndex
		}

		return drives[i].CopyIndex < drives[j].CopyIndex
	})

	writeJSON(w, http.StatusOK, DriveMetricsResponse{RunID: runID, Drives: drives})
}

// getRunDriveMetricsHistory implements GET
// /api/runs/{runID}/metrics/drives/{barcode}/history: a fixed-shape,
// fixed-window sparkline series for one metric of one of this run's own
// tapes (issue #275's write-rate sparkline).
func (h *handler) getRunDriveMetricsHistory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	runID := r.PathValue("runID")
	barcode := r.PathValue("barcode")

	if runID == "" || barcode == "" {
		writeError(w, http.StatusBadRequest, errors.New("runID and barcode are required"))

		return
	}

	metricParam := r.URL.Query().Get("metric")
	if metricParam == "" {
		metricParam = defaultSparklineMetric
	}

	metricName, ok := sparklineMetricAllowlist[metricParam]
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown metric %q", metricParam))

		return
	}

	vmURL := h.getenv(victoriaMetricsEnvVar)
	if vmURL == "" {
		writeError(w, http.StatusServiceUnavailable, errVMUnconfigured)

		return
	}

	history, err := fetchRunHistory(ctx, h.temporalClient, runID)
	if err != nil {
		writeHistoryError(ctx, w, h.temporalClient, runID, err)

		return
	}

	if !runHasBarcode(deriveTapeOutcomes(history.Activities), barcode) {
		writeError(w, http.StatusNotFound, fmt.Errorf("tape %q not found in run %q", barcode, runID))

		return
	}

	end := time.Now()
	start := end.Add(-sparklineStep * (sparklinePoints - 1))

	points, err := queryDriveRange(ctx, vmURL, metricName, barcode, start, end, sparklineStep)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("query VictoriaMetrics range: %w", err))

		return
	}

	writeJSON(w, http.StatusOK, DriveMetricHistoryResponse{
		RunID:   runID,
		Barcode: barcode,
		Metric:  metricParam,
		Points:  points,
	})
}

// runHasBarcode reports whether barcode is one this run actually loaded,
// per its own reconstructed tape outcomes — the membership check that keeps
// the history endpoint from being usable to probe metrics for a barcode
// outside this run.
func runHasBarcode(outcomes []TapeOutcome, barcode string) bool {
	for _, outcome := range outcomes {
		if outcome.Barcode == barcode {
			return true
		}
	}

	return false
}

// driveSample holds whatever subset of the four write-health gauges
// VictoriaMetrics reported for one barcode; a nil field means that gauge had
// no sample (unmeasured, or the drive's generation has no known floor for
// belowFloor).
type driveSample struct {
	throughput  *float64
	repositions *int64
	tapeAlerts  *int
	belowFloor  *bool
}

// toDriveMetric merges one run-derived TapeOutcome with its VictoriaMetrics
// sample (zero value when the barcode had none) into the API's DriveMetric
// shape.
func toDriveMetric(outcome TapeOutcome, sample driveSample) DriveMetric {
	metric := DriveMetric{
		Barcode:            outcome.Barcode,
		TapeIndex:          outcome.TapeIndex,
		CopyIndex:          outcome.CopyIndex,
		DriveIndex:         outcome.DriveIndex,
		Result:             outcome.Result,
		HasData:            sample.throughput != nil || sample.repositions != nil || sample.tapeAlerts != nil || sample.belowFloor != nil,
		ThroughputMBps:     sample.throughput,
		Repositions:        sample.repositions,
		TapeAlertFlagCount: sample.tapeAlerts,
		BelowFloor:         sample.belowFloor,
	}

	if outcome.WriteHealth != nil {
		metric.FloorMBps = outcome.WriteHealth.FloorMBps
		metric.FloorKnown = outcome.WriteHealth.FloorKnown
	}

	return metric
}

// --- VictoriaMetrics HTTP client ---
//
// VictoriaMetrics implements the same query/query_range HTTP API as
// Prometheus (docs/web-ui.md's dev stack points cmd/web at it directly), so
// these helpers speak that wire format.

// vmResponse is the shared envelope for both /api/v1/query and
// /api/v1/query_range.
type vmResponse struct {
	Status string `json:"status"`
	Data   vmData `json:"data"`
	Error  string `json:"error,omitempty"`
}

type vmData struct {
	ResultType string     `json:"resultType"`
	Result     []vmResult `json:"result"`
}

// vmResult is one time series. Value is set for an instant query
// (/api/v1/query), Values for a range query (/api/v1/query_range) — each a
// Prometheus-style [timestamp, "value-as-string"] pair, decoded lazily via
// json.RawMessage since the timestamp and value have different JSON types.
type vmResult struct {
	Metric map[string]string   `json:"metric"`
	Value  []json.RawMessage   `json:"value,omitempty"`
	Values [][]json.RawMessage `json:"values,omitempty"`
}

// vmDo issues one GET request against VictoriaMetrics' Prometheus-compatible
// HTTP API and returns the decoded result series, or an error for any
// non-success outcome (transport failure, non-200 status, malformed body, or
// a {"status":"error",...} response body) — every case getRunDriveMetrics/
// getRunDriveMetricsHistory fold into a single 503, never a 500.
func vmDo(ctx context.Context, baseURL, path string, values url.Values) ([]vmResult, error) {
	queryCtx, cancel := context.WithTimeout(ctx, vmQueryTimeout)
	defer cancel()

	endpoint := strings.TrimRight(baseURL, "/") + path + "?" + values.Encode()

	req, err := http.NewRequestWithContext(queryCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request VictoriaMetrics: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("VictoriaMetrics returned status %d", resp.StatusCode)
	}

	var decoded vmResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if decoded.Status != "success" {
		return nil, fmt.Errorf("VictoriaMetrics query failed: %s", decoded.Error)
	}

	return decoded.Data.Result, nil
}

// metricNameAlternation is a PromQL regex alternation of the four
// write-health gauge names, built from the named constants (not a
// hand-duplicated literal) so it cannot silently drift from them.
var metricNameAlternation = strings.Join(
	[]string{metricThroughput, metricRepositions, metricTapeAlerts, metricBelowFloor}, "|",
)

// barcodeRegex builds a PromQL label-match regex selecting exactly the given
// barcodes. PromQL label regexes are implicitly fully anchored (matched as a
// whole, like Go's regexp.MustCompile("^(?:...)$")), so a plain
// QuoteMeta-escaped alternation is sufficient — no explicit ^/$ needed, and
// escaping rules out any barcode value being interpreted as a wider regex.
func barcodeRegex(barcodes []string) string {
	quoted := make([]string, len(barcodes))
	for i, barcode := range barcodes {
		quoted[i] = regexp.QuoteMeta(barcode)
	}

	return strings.Join(quoted, "|")
}

// queryDriveSamples issues one instant query for all four write-health
// gauges across every given barcode in a single VictoriaMetrics round trip.
func queryDriveSamples(ctx context.Context, baseURL string, barcodes []string) (map[string]driveSample, error) {
	promql := fmt.Sprintf(`{__name__=~%q,barcode=~%q}`, metricNameAlternation, barcodeRegex(barcodes))

	results, err := vmDo(ctx, baseURL, "/api/v1/query", url.Values{"query": {promql}})
	if err != nil {
		return nil, err
	}

	samples := make(map[string]driveSample, len(barcodes))

	for _, result := range results {
		barcode := result.Metric["barcode"]

		value, err := parseSampleValue(result.Value)
		if err != nil {
			continue
		}

		sample := samples[barcode]

		switch result.Metric["__name__"] {
		case metricThroughput:
			sample.throughput = &value
		case metricRepositions:
			repositions := int64(value)
			sample.repositions = &repositions
		case metricTapeAlerts:
			count := int(value)
			sample.tapeAlerts = &count
		case metricBelowFloor:
			belowFloor := value != 0
			sample.belowFloor = &belowFloor
		}

		samples[barcode] = sample
	}

	return samples, nil
}

// queryDriveRange issues a range query for one metric/barcode pair over
// [start, end] at step, returning every sample VictoriaMetrics has — the
// caller (getRunDriveMetricsHistory) always passes the fixed sparkline
// window/step, never client-supplied values.
func queryDriveRange(ctx context.Context, baseURL, metricName, barcode string, start, end time.Time, step time.Duration) ([]MetricPoint, error) {
	promql := fmt.Sprintf(`%s{barcode=%q}`, metricName, barcode)

	values := url.Values{
		"query": {promql},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64) + "s"},
	}

	results, err := vmDo(ctx, baseURL, "/api/v1/query_range", values)
	if err != nil {
		return nil, err
	}

	if len(results) == 0 {
		return nil, nil
	}

	points := make([]MetricPoint, 0, len(results[0].Values))

	for _, pair := range results[0].Values {
		point, ok := parseRangePoint(pair)
		if !ok {
			continue
		}

		points = append(points, point)
	}

	return points, nil
}

// parseSampleValue decodes a Prometheus/VictoriaMetrics instant-query
// [timestamp, "value"] pair, returning just the value (the timestamp is not
// needed for an instant reading).
func parseSampleValue(raw []json.RawMessage) (float64, error) {
	if len(raw) != 2 {
		return 0, fmt.Errorf("malformed sample: expected 2 elements, got %d", len(raw))
	}

	var value string
	if err := json.Unmarshal(raw[1], &value); err != nil {
		return 0, fmt.Errorf("decode sample value: %w", err)
	}

	return strconv.ParseFloat(value, 64)
}

// parseRangePoint decodes one [timestamp, "value"] pair from a range query's
// "values" array into a MetricPoint. ok is false for a malformed pair, which
// the caller skips rather than failing the whole series over one bad sample.
func parseRangePoint(raw []json.RawMessage) (MetricPoint, bool) {
	if len(raw) != 2 {
		return MetricPoint{}, false
	}

	var timestamp float64
	if err := json.Unmarshal(raw[0], &timestamp); err != nil {
		return MetricPoint{}, false
	}

	var value string
	if err := json.Unmarshal(raw[1], &value); err != nil {
		return MetricPoint{}, false
	}

	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return MetricPoint{}, false
	}

	return MetricPoint{Time: time.Unix(int64(timestamp), 0).UTC(), Value: parsed}, true
}
