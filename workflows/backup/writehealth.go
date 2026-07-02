package backup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// Write-health measurement (SPEC §2 principle 2, §14) is purely observational: it
// records how well each tape streamed so the anti-shoe-shining rate can be evaluated
// on every run against the real workload. It never gates, fails, or aborts a run — a
// scrape or timing failure is logged and the run still completes.
//
// After a tape's write window closes (WriteTree → FinalizeTape, i.e. unmount and the
// deferred LTFS index sync have settled), the MeasureWriteHealth activity scrapes the
// drive's SCSI log pages (repositions from page 0x24, TapeAlert flags from page 0x2e)
// via pkg/tape.LogPageReader, and computes sustained write throughput as the tape's
// staged bytes divided by the write-window elapsed time measured by the workflow.

// lto6SpeedMatchingFloorMBps is the LTO-6 native speed-matching floor (~50 MB/s,
// SPEC §14). A sustained throughput below it means the drive could not stream and
// likely shoe-shined. It is used only to compute the below-floor flag, never to gate
// the run, and is deliberately not configurable or derived per LTO generation (issue
// #70 non-goals).
const lto6SpeedMatchingFloorMBps = 50.0

// bytesPerMB is the decimal megabyte (1e6 bytes) used for throughput, matching how
// drive/tape rates are conventionally quoted (MB/s, not MiB/s).
const bytesPerMB = 1_000_000.0

// WriteHealth is the per-tape write-health measurement carried on WrittenTape and
// rendered into the run report and Prometheus metrics. It is observational only.
type WriteHealth struct {
	// Measured is true when a measurement was taken. It is false when the
	// MeasureWriteHealth activity could not run at all; the run still succeeds.
	Measured bool
	// ThroughputMBps is the sustained write throughput over the tape's write
	// window: staged bytes / elapsed seconds, in MB/s (decimal, 1 MB = 1e6 B).
	ThroughputMBps float64
	// FloorMBps is the speed-matching floor ThroughputMBps was compared against.
	FloorMBps float64
	// BelowFloor is true when a throughput was measured and it fell below FloorMBps.
	BelowFloor bool
	// Repositions is the drive's back-hitch count from log page 0x24; zero when the
	// drive does not support the page (pkg/tape.LogPageReader behaviour).
	Repositions int64
	// TapeAlertFlags are the labels of the active TapeAlert flags from page 0x2e.
	TapeAlertFlags []string
}

// Healthy reports whether the tape streamed cleanly: measured, at or above the floor,
// with no repositions and no active TapeAlert flags.
func (h WriteHealth) Healthy() bool {
	return h.Measured && !h.BelowFloor && h.Repositions == 0 && len(h.TapeAlertFlags) == 0
}

// evaluateWriteHealth computes the write-health verdict from the tape's staged size,
// its write-window elapsed time, and the scraped log pages. It is pure so the flag
// logic is unit-testable without hardware. Throughput is only meaningful for a
// positive elapsed time; a non-positive elapsed yields a zero throughput that is not
// flagged below-floor (the rate could not be measured, not that it was slow).
func evaluateWriteHealth(stagedBytes int64, elapsed time.Duration, logs tape.LogPageResult) WriteHealth {
	health := WriteHealth{
		Measured:    true,
		FloorMBps:   lto6SpeedMatchingFloorMBps,
		Repositions: logs.Repositions,
	}

	for _, flag := range logs.TapeAlert.Flags {
		if flag.Set {
			health.TapeAlertFlags = append(health.TapeAlertFlags, tapeAlertLabel(flag))
		}
	}

	seconds := elapsed.Seconds()
	if seconds > 0 {
		health.ThroughputMBps = float64(stagedBytes) / bytesPerMB / seconds
		health.BelowFloor = health.ThroughputMBps < health.FloorMBps
	}

	return health
}

// tapeAlertLabel renders an active TapeAlert flag for the report and logs: its number
// and description, or just the number when the description is unknown.
func tapeAlertLabel(flag tape.TapeAlertFlag) string {
	if flag.Description != "" {
		return fmt.Sprintf("%d: %s", flag.Number, flag.Description)
	}

	return fmt.Sprintf("flag %d", flag.Number)
}

// writeHealthMetrics holds the Prometheus gauges write-health is exported through,
// each labelled by tape barcode. It is nil when metrics are disabled (no registry) or
// when registration failed, in which case record is a no-op.
type writeHealthMetrics struct {
	throughput  *prometheus.GaugeVec
	repositions *prometheus.GaugeVec
	tapeAlerts  *prometheus.GaugeVec
	belowFloor  *prometheus.GaugeVec
}

// newWriteHealthMetrics registers the write-health gauges against reg. A nil reg
// (metrics disabled) yields a nil *writeHealthMetrics. A registration error is
// returned so the caller can decide how to degrade — write-health is observability
// only, so a failure here must never take down the worker.
func newWriteHealthMetrics(reg prometheus.Registerer) (*writeHealthMetrics, error) {
	if reg == nil {
		return nil, nil
	}

	metrics := &writeHealthMetrics{
		throughput: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tape_archiver",
			Subsystem: "write",
			Name:      "throughput_mbps",
			Help:      "Sustained write throughput over the tape's write window, in MB/s (staged bytes / elapsed).",
		}, []string{"barcode"}),
		repositions: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tape_archiver",
			Subsystem: "write",
			Name:      "repositions",
			Help:      "Drive reposition (back-hitch) count from SCSI log page 0x24; zero when unsupported.",
		}, []string{"barcode"}),
		tapeAlerts: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tape_archiver",
			Subsystem: "write",
			Name:      "tapealert_flags",
			Help:      "Number of active TapeAlert flags from SCSI log page 0x2e.",
		}, []string{"barcode"}),
		belowFloor: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tape_archiver",
			Subsystem: "write",
			Name:      "below_floor",
			Help:      "1 when the measured throughput was below the LTO-6 speed-matching floor, else 0.",
		}, []string{"barcode"}),
	}

	for _, collector := range []prometheus.Collector{
		metrics.throughput, metrics.repositions, metrics.tapeAlerts, metrics.belowFloor,
	} {
		if err := reg.Register(collector); err != nil {
			return nil, fmt.Errorf("register write-health metric: %w", err)
		}
	}

	return metrics, nil
}

// record sets the gauges for one tape from its write-health verdict. It is a no-op on
// a nil receiver (metrics disabled) and when the measurement was not taken.
func (m *writeHealthMetrics) record(barcode string, health WriteHealth) {
	if m == nil || !health.Measured {
		return
	}

	labels := prometheus.Labels{"barcode": barcode}
	m.throughput.With(labels).Set(health.ThroughputMBps)
	m.repositions.With(labels).Set(float64(health.Repositions))
	m.tapeAlerts.With(labels).Set(float64(len(health.TapeAlertFlags)))
	m.belowFloor.With(labels).Set(boolToFloat(health.BelowFloor))
}

// boolToFloat maps a bool to the Prometheus 1/0 convention for a boolean gauge.
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}

	return 0
}

// WriteHealthActivities hosts the data-side MeasureWriteHealth activity. It holds the
// Prometheus gauges write-health is exported through (nil when metrics are disabled).
type WriteHealthActivities struct {
	metrics *writeHealthMetrics
}

// newWriteHealthActivities builds the write-health activity, registering its gauges
// against reg. Because write-health is observability only, a metrics registration
// failure degrades to metrics-off (gauges disabled) with a warning rather than
// failing worker startup; the report still records write-health.
func newWriteHealthActivities(reg prometheus.Registerer) *WriteHealthActivities {
	metrics, err := newWriteHealthMetrics(reg)
	if err != nil {
		slog.Warn("write-health: metrics registration failed; write-health metrics disabled", "error", err)

		metrics = nil
	}

	return &WriteHealthActivities{metrics: metrics}
}

// MeasureWriteHealthInput is the payload for the MeasureWriteHealth activity.
type MeasureWriteHealthInput struct {
	// Device is the SCSI generic node of the drive that wrote the tape (e.g.
	// /dev/sg1), scraped for log pages.
	Device string
	// Barcode identifies the tape in the report and metric labels.
	Barcode tape.Barcode
	// StagedBytes is the tape's staged size (sum of StagedArchive.SizeBytes for its
	// archives) — the numerator of the throughput.
	StagedBytes int64
	// Elapsed is the write-window wall-clock the workflow measured around the
	// WriteTree → FinalizeTape span — the denominator of the throughput.
	Elapsed time.Duration
}

// MeasureWriteHealth scrapes the drive's log pages after the write window closed and
// returns the tape's write-health verdict, also recording it to Prometheus. It is
// read-only and idempotent, so it is safely retriable. It is observational only: a
// log-page scrape failure is logged and treated as empty (no repositions, no
// TapeAlert flags) rather than failing — the throughput, which needs no hardware, is
// still reported. It therefore returns a nil error on the normal path so a
// measurement problem never fails the run.
func (a *WriteHealthActivities) MeasureWriteHealth(ctx context.Context, input MeasureWriteHealthInput) (WriteHealth, error) {
	logs, err := tape.NewLogPageReader(input.Device).ReadLogPages(ctx)
	if err != nil {
		slog.Warn("write-health: could not read drive log pages; reporting throughput only",
			"device", input.Device, "barcode", input.Barcode, "error", err)

		logs = tape.LogPageResult{}
	}

	health := evaluateWriteHealth(input.StagedBytes, input.Elapsed, logs)

	a.metrics.record(string(input.Barcode), health)

	return health, nil
}
