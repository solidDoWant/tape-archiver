// This file implements the drive write-health metrics endpoints (issue #275), a
// read-only proxy over VictoriaMetrics for the gauges in
// workflows/backup/writehealth.go. It must never become an open PromQL proxy:
// queries are built server-side from a fixed metric allowlist, a barcode this
// run actually loaded, and a fixed range/step. Any VictoriaMetrics problem,
// including being unconfigured, degrades to 503, never 500.
package runsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// victoriaMetricsEnvVar holds VictoriaMetrics' base URL; unset means unconfigured.
const victoriaMetricsEnvVar = "VICTORIAMETRICS_URL"

// errVMUnconfigured is the stable 503 body when VICTORIAMETRICS_URL is unset (issue #275 AC1).
var errVMUnconfigured = errors.New("VictoriaMetrics is not configured (VICTORIAMETRICS_URL is unset)")

// The write-health gauges registered in workflows/backup/writehealth.go; keep in sync.
const (
	metricThroughput  = "tape_archiver_write_throughput_mbps"
	metricRepositions = "tape_archiver_write_repositions"
	metricTapeAlerts  = "tape_archiver_write_tapealert_flags"
	metricBelowFloor  = "tape_archiver_write_below_floor"
)

// sparklineMetricAllowlist maps the public `metric` query parameter to a metric
// name, so clients can never supply a raw metric name.
var sparklineMetricAllowlist = map[string]string{
	"throughput":  metricThroughput,
	"repositions": metricRepositions,
	"tapealerts":  metricTapeAlerts,
	"belowfloor":  metricBelowFloor,
}

const defaultSparklineMetric = "throughput"

// sparklinePoints/sparklineStep fix the sparkline window server-side so the
// endpoint cannot page through arbitrary history (issue #275 non-goal).
const (
	sparklinePoints = 8
	sparklineStep   = 90 * time.Second
)

// vmQueryTimeout is well inside requestTimeout so a slow backend still leaves
// time to degrade to 503.
const vmQueryTimeout = 5 * time.Second

// DriveMetric is one tape's write-health reading, merging run history with
// VictoriaMetrics gauges.
type DriveMetric struct {
	Barcode    string `json:"barcode"`
	TapeIndex  int    `json:"tapeIndex"`
	CopyIndex  int    `json:"copyIndex"`
	DriveIndex int    `json:"driveIndex"`
	// Result mirrors TapeOutcome.Result ("loaded", "written", or "failed").
	Result string `json:"result"`

	// HasData is false when no gauge was reported yet, as distinct from a zero reading.
	HasData bool `json:"hasData"`

	ThroughputMBps     *float64 `json:"throughputMBps,omitempty"`
	Repositions        *int64   `json:"repositions,omitempty"`
	TapeAlertFlagCount *int     `json:"tapeAlertFlagCount,omitempty"`
	BelowFloor         *bool    `json:"belowFloor,omitempty"`

	// FloorMBps/FloorKnown come from run history; the floor is not exported as a gauge.
	FloorMBps  float64 `json:"floorMBps,omitempty"`
	FloorKnown bool    `json:"floorKnown"`
	// ThroughputUnmeasurable comes from run history: the write was too small
	// to judge, so no throughput gauge is exported for it.
	ThroughputUnmeasurable bool `json:"throughputUnmeasurable"`
}

// DriveMetricsResponse is the GET /api/runs/{runID}/metrics/drives response body.
type DriveMetricsResponse struct {
	RunID  string        `json:"runId"`
	Drives []DriveMetric `json:"drives"`
}

// MetricPoint is one sample in a drive metric history series.
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

// getRunDriveMetrics implements GET /api/runs/{runID}/metrics/drives.
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
		// No tape loaded yet: a normal no-data state.
		writeJSON(w, http.StatusOK, DriveMetricsResponse{RunID: runID})

		return
	}

	barcodes := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		barcodes = append(barcodes, outcome.Barcode)
	}

	// Anchor to the run's window so a finished run never reads a later reuse of the tape.
	samples, err := queryDriveSamples(ctx, vmURL, barcodes, dryRunFromMemo(history.StartMemo), metricQueryWindow(history, time.Now()))
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

// getRunDriveMetricsHistory implements GET /api/runs/{runID}/metrics/drives/{barcode}/history.
func (h *handler) getRunDriveMetricsHistory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	runID := r.PathValue("runID")
	barcode := r.PathValue("barcode")

	if runID == "" || barcode == "" {
		writeError(w, http.StatusBadRequest, errors.New("runID and barcode are required"))

		return
	}

	// Checked before the metric allowlist so unconfigured is always 503, never a 400.
	vmURL := h.getenv(victoriaMetricsEnvVar)
	if vmURL == "" {
		writeError(w, http.StatusServiceUnavailable, errVMUnconfigured)

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

	history, err := fetchRunHistory(ctx, h.temporalClient, runID)
	if err != nil {
		writeHistoryError(ctx, w, h.temporalClient, runID, err)

		return
	}

	if !runHasBarcode(deriveTapeOutcomes(history.Activities), barcode) {
		writeError(w, http.StatusNotFound, fmt.Errorf("tape %q not found in run %q", barcode, runID))

		return
	}

	// A closed run's window ends when it wrote, not at the current wall clock.
	end := metricQueryWindow(history, time.Now())
	start := end.Add(-sparklineStep * (sparklinePoints - 1))

	points, err := queryDriveRange(ctx, vmURL, metricName, barcode, dryRunFromMemo(history.StartMemo), start, end, sparklineStep)
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

// runHasBarcode prevents probing metrics for barcodes outside this run.
func runHasBarcode(outcomes []TapeOutcome, barcode string) bool {
	for _, outcome := range outcomes {
		if outcome.Barcode == barcode {
			return true
		}
	}

	return false
}

// driveSample holds the gauges reported for one barcode; nil means no sample.
type driveSample struct {
	throughput  *float64
	repositions *int64
	tapeAlerts  *int
	belowFloor  *bool
}

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
		metric.ThroughputUnmeasurable = outcome.WriteHealth.ThroughputUnmeasurable
	}

	return metric
}

// vmResponse is the Prometheus-compatible envelope for both /api/v1/query and
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

// vmResult is one series: Value for instant queries, Values for range queries,
// each a [timestamp, "value"] pair of mixed JSON types.
type vmResult struct {
	Metric map[string]string   `json:"metric"`
	Value  []json.RawMessage   `json:"value,omitempty"`
	Values [][]json.RawMessage `json:"values,omitempty"`
}

// vmDo issues one query and returns its series, or an error for any non-success outcome.
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

var metricNameAlternation = strings.Join(
	[]string{metricThroughput, metricRepositions, metricTapeAlerts, metricBelowFloor}, "|",
)

// barcodeRegex builds a label regex matching exactly barcodes; PromQL label
// regexes are implicitly anchored, so QuoteMeta alone suffices.
func barcodeRegex(barcodes []string) string {
	quoted := make([]string, len(barcodes))
	for i, barcode := range barcodes {
		quoted[i] = regexp.QuoteMeta(barcode)
	}

	return strings.Join(quoted, "|")
}

// metricQueryWindow returns now for an open run, else the end of its Write
// phase. Not the close time: gauges are emitted only while writing, and a run
// can sit paused in Eject/Burn/Deliver long enough that a window ending at close
// would miss every sample. Close time is the fallback.
func metricQueryWindow(history runHistory, now time.Time) time.Time {
	if !history.Closed {
		return now
	}

	if writeEnd, ok := writePhaseEnd(history); ok {
		return writeEnd
	}

	if !history.CloseTime.IsZero() {
		return history.CloseTime
	}

	return now
}

// writePhaseEnd returns the latest end time of a finished Write-phase activity.
func writePhaseEnd(history runHistory) (time.Time, bool) {
	var latest time.Time

	for _, record := range history.Activities {
		phase, ok := phaseForActivity(record.Name, record.Input)
		if !ok || phase != backup.PhaseWrite {
			continue
		}

		if record.EndTime.IsZero() {
			continue
		}

		if record.EndTime.After(latest) {
			latest = record.EndTime
		}
	}

	return latest, !latest.IsZero()
}

// queryDriveSamples reads all gauges for all barcodes in one instant query at `at`.
func queryDriveSamples(ctx context.Context, baseURL string, barcodes []string, dryRun bool, at time.Time) (map[string]driveSample, error) {
	promql := fmt.Sprintf(`{__name__=~%q,barcode=~%q,%s}`, metricNameAlternation, barcodeRegex(barcodes), dryRunMatcher(dryRun))

	values := url.Values{"query": {promql}}
	if !at.IsZero() {
		// A closed run must not read a later run's reuse of the barcode.
		values.Set("time", strconv.FormatInt(at.Unix(), 10))
	}

	results, err := vmDo(ctx, baseURL, "/api/v1/query", values)
	if err != nil {
		return nil, err
	}

	// A data-worker restart can split a barcode+metric into two series (different
	// instance/job labels); prefer the freshest sample.
	type tsValue struct {
		timestamp float64
		value     float64
	}

	best := make(map[string]map[string]tsValue, len(barcodes))

	for _, result := range results {
		barcode := result.Metric["barcode"]
		name := result.Metric["__name__"]

		timestamp, value, err := parseSampleWithTime(result.Value)
		if err != nil {
			continue
		}

		metrics := best[barcode]
		if metrics == nil {
			metrics = make(map[string]tsValue, 4)
			best[barcode] = metrics
		}

		if current, ok := metrics[name]; !ok || timestamp >= current.timestamp {
			metrics[name] = tsValue{timestamp: timestamp, value: value}
		}
	}

	samples := make(map[string]driveSample, len(best))

	for barcode, metrics := range best {
		var sample driveSample

		if entry, ok := metrics[metricThroughput]; ok {
			value := entry.value
			sample.throughput = &value
		}

		if entry, ok := metrics[metricRepositions]; ok {
			repositions := int64(entry.value)
			sample.repositions = &repositions
		}

		if entry, ok := metrics[metricTapeAlerts]; ok {
			count := int(entry.value)
			sample.tapeAlerts = &count
		}

		if entry, ok := metrics[metricBelowFloor]; ok {
			belowFloor := entry.value != 0
			sample.belowFloor = &belowFloor
		}

		samples[barcode] = sample
	}

	return samples, nil
}

// queryDriveRange issues a range query for one metric/barcode over [start, end].
func queryDriveRange(ctx context.Context, baseURL, metricName, barcode string, dryRun bool, start, end time.Time, step time.Duration) ([]MetricPoint, error) {
	promql := fmt.Sprintf(`%s{barcode=%q,%s}`, metricName, barcode, dryRunMatcher(dryRun))

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

	// Merge all series: a worker restart splits one barcode across series. Points
	// must be non-nil so it marshals to [] rather than null.
	byTime := make(map[int64]MetricPoint)

	for _, result := range results {
		for _, pair := range result.Values {
			point, ok := parseRangePoint(pair)
			if !ok {
				continue
			}

			byTime[point.Time.Unix()] = point
		}
	}

	points := make([]MetricPoint, 0, len(byTime))
	for _, point := range byTime {
		points = append(points, point)
	}

	sort.Slice(points, func(i, j int) bool {
		return points[i].Time.Before(points[j].Time)
	})

	return points, nil
}

// parseSampleWithTime decodes a [timestamp, "value"] pair. Non-finite values are
// rejected because encoding/json cannot marshal them, and writeJSON has already
// sent a 200 by the time Encode fails.
func parseSampleWithTime(raw []json.RawMessage) (timestamp float64, value float64, err error) {
	if len(raw) != 2 {
		return 0, 0, fmt.Errorf("malformed sample: expected 2 elements, got %d", len(raw))
	}

	if err := json.Unmarshal(raw[0], &timestamp); err != nil {
		return 0, 0, fmt.Errorf("decode sample timestamp: %w", err)
	}

	var text string
	if err := json.Unmarshal(raw[1], &text); err != nil {
		return 0, 0, fmt.Errorf("decode sample value: %w", err)
	}

	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, 0, err
	}

	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, 0, fmt.Errorf("non-finite sample value %q", text)
	}

	return timestamp, parsed, nil
}

func parseSampleValue(raw []json.RawMessage) (float64, error) {
	_, value, err := parseSampleWithTime(raw)

	return value, err
}

// parseRangePoint decodes one range pair; ok is false for a malformed pair.
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

	// Non-finite values cannot be marshaled; see parseSampleWithTime.
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return MetricPoint{}, false
	}

	return MetricPoint{Time: time.Unix(int64(timestamp), 0).UTC(), Value: parsed}, true
}

// dryRunMatcher selects the series written by a run of the same kind: a dry run
// reuses real barcodes (#412). Series recorded before the label existed have
// none, so a real run matches them too.
func dryRunMatcher(dryRun bool) string {
	if dryRun {
		return `dry_run="true"`
	}

	return `dry_run!="true"`
}
