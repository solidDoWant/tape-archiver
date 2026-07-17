package runsapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
)

// envFor builds a getenv func serving vmURL for VICTORIAMETRICS_URL and ""
// for anything else — the metrics.go handlers only ever read that one
// variable.
func envFor(vmURL string) func(string) string {
	return func(key string) string {
		if key == victoriaMetricsEnvVar {
			return vmURL
		}

		return ""
	}
}

// rawPair encodes one Prometheus/VictoriaMetrics [timestamp, "value"] pair
// the way the real API does: a bare JSON number timestamp and a JSON string
// value.
func rawPair(t *testing.T, timestamp float64, value string) []json.RawMessage {
	t.Helper()

	ts, err := json.Marshal(timestamp)
	require.NoError(t, err)

	val, err := json.Marshal(value)
	require.NoError(t, err)

	return []json.RawMessage{ts, val}
}

// fakeVictoriaMetrics serves instant to /api/v1/query and rangeResp to
// /api/v1/query_range — a hand-rolled fake of the one real dependency these
// handlers proxy, per the "never mock the component under test" rule.
func fakeVictoriaMetrics(t *testing.T, instant, rangeResp vmResponse) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var body vmResponse

		switch r.URL.Path {
		case "/api/v1/query":
			body = instant
		case "/api/v1/query_range":
			body = rangeResp
		default:
			w.WriteHeader(http.StatusNotFound)

			return
		}

		require.NoError(t, json.NewEncoder(w).Encode(body))
	}))
}

func TestGetRunDriveMetricsHandler(t *testing.T) {
	t.Run("unconfigured VictoriaMetrics is a stable 503, not a raw error or a hang", func(t *testing.T) {
		handler := newMux(newHandler(&fakeTemporalClient{}, emptyEnv))

		first := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/metrics/drives", nil)
		require.Equal(t, http.StatusServiceUnavailable, first.Code)

		var body errorResponse
		require.NoError(t, json.Unmarshal(first.Body.Bytes(), &body))
		assert.Equal(t, errVMUnconfigured.Error(), body.Error)

		// A second, independent request gets byte-identical output — "stable"
		// per issue #275 AC1, not just "some 503".
		second := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/metrics/drives", nil)
		assert.Equal(t, first.Body.String(), second.Body.String())
	})

	t.Run("configured but unreachable VictoriaMetrics degrades to 503, never 500", func(t *testing.T) {
		fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
		}}
		// Nothing listens on this loopback port.
		handler := newMux(newHandler(fake, envFor("http://127.0.0.1:1")))

		recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/metrics/drives", nil)
		assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	})

	t.Run("a run with no tapes loaded yet is a not-writing empty list, not an error", func(t *testing.T) {
		fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{}
		}}

		server := fakeVictoriaMetrics(t, vmResponse{Status: "success"}, vmResponse{Status: "success"})
		defer server.Close()

		handler := newMux(newHandler(fake, envFor(server.URL)))

		recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/metrics/drives", nil)
		require.Equal(t, http.StatusOK, recorder.Code)

		var body DriveMetricsResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		assert.Equal(t, "run-1", body.RunID)
		assert.Empty(t, body.Drives)
	})

	t.Run("reports live VictoriaMetrics samples merged with the run's own floor, distinguishing an unmeasured tape", func(t *testing.T) {
		fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
		}}

		instant := vmResponse{Status: "success", Data: vmData{ResultType: "vector", Result: []vmResult{
			{Metric: map[string]string{"__name__": metricThroughput, "barcode": "GOODTAPE01"}, Value: rawPair(t, 1700000000, "142.5")},
			{Metric: map[string]string{"__name__": metricRepositions, "barcode": "GOODTAPE01"}, Value: rawPair(t, 1700000000, "2")},
			{Metric: map[string]string{"__name__": metricTapeAlerts, "barcode": "GOODTAPE01"}, Value: rawPair(t, 1700000000, "1")},
			{Metric: map[string]string{"__name__": metricBelowFloor, "barcode": "GOODTAPE01"}, Value: rawPair(t, 1700000000, "1")},
		}}}

		server := fakeVictoriaMetrics(t, instant, vmResponse{Status: "success"})
		defer server.Close()

		handler := newMux(newHandler(fake, envFor(server.URL)))

		recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/metrics/drives", nil)
		require.Equal(t, http.StatusOK, recorder.Code)

		var body DriveMetricsResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		require.Len(t, body.Drives, 2, "both this run's tapes (FAILTAPE01, GOODTAPE01) must be reported")

		byBarcode := make(map[string]DriveMetric, len(body.Drives))
		for _, drive := range body.Drives {
			byBarcode[drive.Barcode] = drive
		}

		good := byBarcode["GOODTAPE01"]
		assert.True(t, good.HasData)
		require.NotNil(t, good.ThroughputMBps)
		assert.InDelta(t, 142.5, *good.ThroughputMBps, 0.001)
		require.NotNil(t, good.Repositions)
		assert.Equal(t, int64(2), *good.Repositions)
		require.NotNil(t, good.TapeAlertFlagCount)
		assert.Equal(t, 1, *good.TapeAlertFlagCount)
		require.NotNil(t, good.BelowFloor)
		assert.True(t, *good.BelowFloor, "the below-floor gauge must be visibly surfaced (issue #275 AC4)")
		assert.True(t, good.FloorKnown)
		assert.InDelta(t, 50, good.FloorMBps, 0.001, "floor comes from the run's own history, not VictoriaMetrics")

		failed := byBarcode["FAILTAPE01"]
		assert.False(t, failed.HasData, "a tape that never reached MeasureWriteHealth must report no data, not a zero reading")
		assert.Nil(t, failed.ThroughputMBps)
	})

	t.Run("picks the freshest series when a worker restart left two for one barcode", func(t *testing.T) {
		fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
		}}

		// A data-worker restart mid-run leaves two throughput series for the same
		// barcode (distinct instance labels). The newer sample is listed FIRST, so
		// a last-writer-wins merge would wrongly keep the older; freshest-by-time
		// must keep the newer.
		instant := vmResponse{Status: "success", Data: vmData{ResultType: "vector", Result: []vmResult{
			{Metric: map[string]string{"__name__": metricThroughput, "barcode": "GOODTAPE01", "instance": "worker-new"}, Value: rawPair(t, 1700000600, "142.5")},
			{Metric: map[string]string{"__name__": metricThroughput, "barcode": "GOODTAPE01", "instance": "worker-old"}, Value: rawPair(t, 1700000000, "9.9")},
		}}}

		server := fakeVictoriaMetrics(t, instant, vmResponse{Status: "success"})
		defer server.Close()

		handler := newMux(newHandler(fake, envFor(server.URL)))

		recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/metrics/drives", nil)
		require.Equal(t, http.StatusOK, recorder.Code)

		var body DriveMetricsResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

		var good DriveMetric

		for _, drive := range body.Drives {
			if drive.Barcode == "GOODTAPE01" {
				good = drive
			}
		}

		require.NotNil(t, good.ThroughputMBps)
		assert.InDelta(t, 142.5, *good.ThroughputMBps, 0.001, "the newer instance's reading must win, regardless of result order")
	})
}

func TestMetricQueryWindow(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	closeTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	assert.Equal(t, now, metricQueryWindow(runHistory{Closed: false, CloseTime: closeTime}, now),
		"an open run is queried at the current time")
	assert.Equal(t, closeTime, metricQueryWindow(runHistory{Closed: true, CloseTime: closeTime}, now),
		"a closed run is queried at its own close time, not the wall clock")
	assert.Equal(t, now, metricQueryWindow(runHistory{Closed: true}, now),
		"a closed run with no recorded close time falls back to now")
}

func TestGetRunDriveMetricsHistoryHandler(t *testing.T) {
	fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
	}}

	t.Run("unconfigured VictoriaMetrics is a 503", func(t *testing.T) {
		handler := newMux(newHandler(fake, emptyEnv))
		recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/metrics/drives/GOODTAPE01/history", nil)
		assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	})

	t.Run("unconfigured VictoriaMetrics is a 503 even with an unknown metric", func(t *testing.T) {
		// The config check must run before the metric-param allowlist, so a
		// client that keys "metrics unavailable" on 503 sees it rather than a
		// spurious 400 for the bad param.
		handler := newMux(newHandler(fake, emptyEnv))
		recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/metrics/drives/GOODTAPE01/history?metric=bogus", nil)
		assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	})

	t.Run("an unknown metric name is rejected before any VictoriaMetrics call", func(t *testing.T) {
		server := fakeVictoriaMetrics(t, vmResponse{Status: "success"}, vmResponse{Status: "success"})
		defer server.Close()

		handler := newMux(newHandler(fake, envFor(server.URL)))
		recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/metrics/drives/GOODTAPE01/history?metric=bogus", nil)
		assert.Equal(t, http.StatusBadRequest, recorder.Code)
	})

	t.Run("a barcode this run never loaded is a 404, not an open metrics probe", func(t *testing.T) {
		server := fakeVictoriaMetrics(t, vmResponse{Status: "success"}, vmResponse{Status: "success"})
		defer server.Close()

		handler := newMux(newHandler(fake, envFor(server.URL)))
		recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/metrics/drives/SOME-OTHER-TAPE/history", nil)
		assert.Equal(t, http.StatusNotFound, recorder.Code)
	})

	t.Run("returns the fixed sparkline series for a tape this run did load", func(t *testing.T) {
		rangeResp := vmResponse{Status: "success", Data: vmData{ResultType: "matrix", Result: []vmResult{
			{
				Metric: map[string]string{"barcode": "GOODTAPE01"},
				Values: [][]json.RawMessage{
					rawPair(t, 1700000000, "100"),
					rawPair(t, 1700000090, "120"),
				},
			},
		}}}

		server := fakeVictoriaMetrics(t, vmResponse{Status: "success"}, rangeResp)
		defer server.Close()

		handler := newMux(newHandler(fake, envFor(server.URL)))
		recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/metrics/drives/GOODTAPE01/history", nil)
		require.Equal(t, http.StatusOK, recorder.Code)

		var body DriveMetricHistoryResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		assert.Equal(t, "run-1", body.RunID)
		assert.Equal(t, "GOODTAPE01", body.Barcode)
		assert.Equal(t, defaultSparklineMetric, body.Metric)
		require.Len(t, body.Points, 2)
		assert.InDelta(t, 100, body.Points[0].Value, 0.001)
		assert.InDelta(t, 120, body.Points[1].Value, 0.001)
	})

	t.Run("merges the sparkline across two series left by a mid-run worker restart", func(t *testing.T) {
		// A restart splits the barcode's samples across two series (distinct
		// instance labels), each covering part of the window. Taking only the
		// first series would drop half the sparkline; the points must be merged.
		rangeResp := vmResponse{Status: "success", Data: vmData{ResultType: "matrix", Result: []vmResult{
			{
				Metric: map[string]string{"barcode": "GOODTAPE01", "instance": "worker-old"},
				Values: [][]json.RawMessage{rawPair(t, 1700000000, "100")},
			},
			{
				Metric: map[string]string{"barcode": "GOODTAPE01", "instance": "worker-new"},
				Values: [][]json.RawMessage{rawPair(t, 1700000090, "120"), rawPair(t, 1700000180, "130")},
			},
		}}}

		server := fakeVictoriaMetrics(t, vmResponse{Status: "success"}, rangeResp)
		defer server.Close()

		handler := newMux(newHandler(fake, envFor(server.URL)))
		recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/metrics/drives/GOODTAPE01/history", nil)
		require.Equal(t, http.StatusOK, recorder.Code)

		var body DriveMetricHistoryResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

		require.Len(t, body.Points, 3, "points from both series must be merged, not just the first series")
		assert.InDelta(t, 100, body.Points[0].Value, 0.001)
		assert.InDelta(t, 120, body.Points[1].Value, 0.001)
		assert.InDelta(t, 130, body.Points[2].Value, 0.001)
	})

	t.Run("VictoriaMetrics reporting no series for the barcode yet is an empty, not an error", func(t *testing.T) {
		server := fakeVictoriaMetrics(t, vmResponse{Status: "success"}, vmResponse{Status: "success"})
		defer server.Close()

		handler := newMux(newHandler(fake, envFor(server.URL)))
		recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/metrics/drives/GOODTAPE01/history", nil)
		require.Equal(t, http.StatusOK, recorder.Code)

		var body DriveMetricHistoryResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		assert.Empty(t, body.Points)
	})
}

func TestBarcodeRegex(t *testing.T) {
	tests := []struct {
		name     string
		barcodes []string
		want     string
	}{
		{name: "single barcode", barcodes: []string{"TA0001L6"}, want: `TA0001L6`},
		{name: "multiple barcodes joined with alternation", barcodes: []string{"A", "B"}, want: `A|B`},
		{name: "regex metacharacters are escaped", barcodes: []string{"A.B"}, want: `A\.B`},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.want, barcodeRegex(testCase.barcodes))
		})
	}
}

func TestParseSampleValue(t *testing.T) {
	tests := []struct {
		name      string
		raw       []json.RawMessage
		want      float64
		assertErr require.ErrorAssertionFunc
	}{
		{
			name:      "valid pair",
			raw:       rawPair(t, 1700000000, "42.5"),
			want:      42.5,
			assertErr: require.NoError,
		},
		{
			name:      "wrong element count",
			raw:       []json.RawMessage{[]byte(`1700000000`)},
			assertErr: require.Error,
		},
		{
			name:      "non-numeric value",
			raw:       rawPair(t, 1700000000, "not-a-number"),
			assertErr: require.Error,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := parseSampleValue(testCase.raw)
			testCase.assertErr(t, err)

			if err == nil {
				assert.InDelta(t, testCase.want, got, 0.001)
			}
		})
	}
}

func TestParseRangePoint(t *testing.T) {
	point, ok := parseRangePoint(rawPair(t, 1700000000, "88"))
	require.True(t, ok)
	assert.InDelta(t, 88, point.Value, 0.001)
	assert.Equal(t, int64(1700000000), point.Time.Unix())

	_, ok = parseRangePoint([]json.RawMessage{[]byte(`1700000000`)})
	assert.False(t, ok, "a malformed pair must not be reported as a valid point")
}
