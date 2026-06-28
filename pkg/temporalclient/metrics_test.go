package temporalclient

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
)

// flushTimeout caps how long a test will wait for tally's reporting
// goroutine to push values into the Prometheus registry. Tally flushes on a
// fixed interval; the closer also blocks until a final flush completes.
const flushTimeout = 5 * time.Second

// buildHandler wires a tally-backed MetricsHandler against a fresh
// Prometheus registry and returns both, alongside a flush function that
// drains pending samples synchronously by closing the tally scope.
func buildHandler(t *testing.T) (client.MetricsHandler, *prometheus.Registry, func()) {
	t.Helper()

	registry := prometheus.NewRegistry()

	handler, closer := newMetricsHandler(registry)

	flushed := false
	flush := func() {
		if flushed {
			return
		}

		flushed = true

		done := make(chan error, 1)

		go func() { done <- closer.Close() }()

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(flushTimeout):
			t.Fatalf("tally scope did not flush within %s", flushTimeout)
		}
	}

	t.Cleanup(flush)

	return handler, registry, flush
}

func TestNewMetricsHandlerNilRegistererReturnsNopHandler(t *testing.T) {
	handler, closer := newMetricsHandler(nil)

	assert.Equal(t, client.MetricsNopHandler, handler)
	require.NoError(t, closer.Close())
}

func TestMetricsHandlerCounterAppearsOnRegistryWithTotalSuffix(t *testing.T) {
	handler, registry, flush := buildHandler(t)

	handler.Counter("temporal_request").Inc(3)
	handler.Counter("temporal_request").Inc(2)

	flush()

	mf := findMetricFamily(t, gather(t, registry), "temporal_request_total")
	require.Len(t, mf.GetMetric(), 1)
	assert.Equal(t, float64(5), mf.GetMetric()[0].GetCounter().GetValue())
}

func TestMetricsHandlerGaugeReportsLatestValue(t *testing.T) {
	handler, registry, flush := buildHandler(t)

	handler.Gauge("temporal_workers_busy").Update(7)
	handler.Gauge("temporal_workers_busy").Update(4)

	flush()

	mf := findMetricFamily(t, gather(t, registry), "temporal_workers_busy")
	require.Len(t, mf.GetMetric(), 1)
	assert.Equal(t, float64(4), mf.GetMetric()[0].GetGauge().GetValue())
}

func TestMetricsHandlerTimerEmitsHistogramSeconds(t *testing.T) {
	handler, registry, flush := buildHandler(t)

	handler.Timer("temporal_long_request_latency").Record(250 * time.Millisecond)

	flush()

	mf := findMetricFamily(t, gather(t, registry), "temporal_long_request_latency_seconds")
	require.Len(t, mf.GetMetric(), 1)

	hist := mf.GetMetric()[0].GetHistogram()
	require.NotNil(t, hist, "timers should be emitted as histograms (not summaries)")
	assert.Equal(t, uint64(1), hist.GetSampleCount())
	assert.InDelta(t, 0.25, hist.GetSampleSum(), 0.001)
}

func TestMetricsHandlerWithTagsAttachesPromLabels(t *testing.T) {
	handler, registry, flush := buildHandler(t)

	tagged := handler.WithTags(map[string]string{"namespace": "default", "task_queue": "media"}).
		WithTags(map[string]string{"task_queue": "override"})

	tagged.Counter("temporal_request").Inc(1)

	flush()

	mf := findMetricFamily(t, gather(t, registry), "temporal_request_total")
	require.Len(t, mf.GetMetric(), 1)

	labels := map[string]string{}
	for _, lp := range mf.GetMetric()[0].GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}

	assert.Equal(t, "default", labels["namespace"])
	assert.Equal(t, "override", labels["task_queue"], "child WithTags should overwrite parent values for the same key")
}

func gather(t *testing.T, registry *prometheus.Registry) []*dto.MetricFamily {
	t.Helper()

	mfs, err := registry.Gather()
	require.NoError(t, err)

	return mfs
}

func findMetricFamily(t *testing.T, gathered []*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()

	for _, mf := range gathered {
		if mf.GetName() == name {
			return mf
		}
	}

	names := make([]string, 0, len(gathered))
	for _, mf := range gathered {
		names = append(names, mf.GetName())
	}

	t.Fatalf("metric family %q not present in gathered output:\n%s", name, strings.Join(names, "\n"))

	return nil
}
