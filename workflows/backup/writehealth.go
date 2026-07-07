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
// drive's SCSI log pages (repositions as total_suspended_writes from the Tape usage
// page 0x30, TapeAlert flags from page 0x2e) via pkg/tape.LogPageReader, and computes
// sustained write throughput as the bytes written to the tape (archive slices + PAR2
// recovery files) divided by the write-window elapsed time measured by the workflow.

// bytesPerMB is the decimal megabyte (1e6 bytes) used for throughput, matching how
// drive/tape rates are conventionally quoted (MB/s, not MiB/s).
const bytesPerMB = 1_000_000.0

// speedMatchingFloorsMBps maps an LTO generation to its speed-matching floor: the
// lowest native data rate the drive can dynamically slow to before it must stop and
// back-hitch. A sustained throughput below the floor means the drive could not stream
// and likely shoe-shined (SPEC §2 principle 2, §14). The floor is generation-specific
// — higher generations stream much faster, so a single hard-coded value would badly
// mis-flag other drives.
//
// Values are the published minimum native speed-matching (data-rate-matching) rates,
// nominal and slightly vendor-dependent (IBM vs HPE drives differ by a few MB/s); the
// conservative figure is used so the below-floor flag under- rather than over-reports:
//
//	LTO-5  40 MB/s  (IBM: 14 speeds, 40–140; HP drives ~47)
//	LTO-6  50 MB/s  (SPEC §2/§14; within IBM ~32–160 / HPE ~53.5–160)
//	LTO-7 100 MB/s  (IBM and HPE data-rate-matching range 100/101–300)
//	LTO-8 112 MB/s  (IBM drive specifications, range 112–360)
//	LTO-9 180 MB/s  (IBM drive specifications, range 180–400)
//
// A capacity that maps to no known generation (below LTO-5 or unrecognized) is treated
// as unknown by writeHealthFloor rather than assigned a guessed value, so the
// below-floor verdict is never asserted against a number we cannot defend.
var speedMatchingFloorsMBps = map[string]float64{
	"LTO-5": 40,
	"LTO-6": 50,
	"LTO-7": 100,
	"LTO-8": 112,
	"LTO-9": 180,
}

// writeHealthFloor returns the speed-matching floor for the tape generation implied by
// the configured native capacity (SPEC §5 library.tapeCapacityBytes), classifying the
// generation with ltoGeneration. known is false when the generation has no sourced
// floor, in which case the below-floor verdict is not evaluated (writeHealthFloor never
// invents a floor).
func writeHealthFloor(capacityBytes int64) (floorMBps float64, known bool) {
	floor, ok := speedMatchingFloorsMBps[ltoGeneration(capacityBytes)]

	return floor, ok
}

// WriteHealth is the per-tape write-health measurement carried on WrittenTape and
// rendered into the run report and Prometheus metrics. It is observational only.
type WriteHealth struct {
	// Measured is true when a measurement was taken. It is false when the
	// MeasureWriteHealth activity could not run at all; the run still succeeds.
	Measured bool
	// ThroughputMBps is the sustained write throughput over the tape's write
	// window: bytes written to tape (slices + PAR2) / elapsed seconds, in MB/s
	// (decimal, 1 MB = 1e6 B).
	ThroughputMBps float64
	// FloorMBps is the speed-matching floor ThroughputMBps was compared against,
	// derived from the tape generation. Zero and meaningless when FloorKnown is false.
	FloorMBps float64
	// FloorKnown is true when a speed-matching floor is known for the tape's
	// generation. When false the throughput is still reported but no below-floor
	// verdict is made (no floor is guessed).
	FloorKnown bool
	// BelowFloor is true when a throughput was measured against a known floor and
	// fell below it.
	BelowFloor bool
	// Repositions is the drive's back-hitch count (total_suspended_writes) from the
	// Tape usage log page 0x30. Meaningful only when RepositionsMeasured is true.
	Repositions int64
	// RepositionsMeasured is true when the reposition counter was actually read
	// from page 0x30. When false the drive did not support the page (or the scrape
	// failed) and Repositions is a placeholder, not a measured zero — so a clean
	// streaming write is never certified from an unread counter.
	RepositionsMeasured bool
	// TapeAlertFlags are the labels of the active TapeAlert flags from page 0x2e.
	TapeAlertFlags []string
}

// Healthy reports whether the tape streamed cleanly: measured against a known floor,
// at or above it, with the reposition counter measured and zero, and no active
// TapeAlert flags. A tape whose generation has no known floor, or whose reposition
// counter could not be read, is never reported healthy — its streaming could not be
// judged.
func (h WriteHealth) Healthy() bool {
	return h.Measured && h.FloorKnown && !h.BelowFloor &&
		h.RepositionsMeasured && h.Repositions == 0 && len(h.TapeAlertFlags) == 0
}

// evaluateWriteHealth computes the write-health verdict from the bytes written to the
// tape (slices + PAR2), its write-window elapsed time, and the scraped log pages. It
// is pure so the flag logic is unit-testable without hardware. Throughput is only
// meaningful for a positive elapsed time; a non-positive elapsed yields a zero
// throughput that is not flagged below-floor (the rate could not be measured, not that
// it was slow).
func evaluateWriteHealth(bytesWritten int64, elapsed time.Duration, logs tape.LogPageResult, floorMBps float64, floorKnown bool) WriteHealth {
	health := WriteHealth{
		Measured:            true,
		FloorMBps:           floorMBps,
		FloorKnown:          floorKnown,
		Repositions:         logs.Repositions,
		RepositionsMeasured: logs.RepositionsMeasured,
	}

	for _, flag := range logs.TapeAlert.Flags {
		if flag.Set {
			health.TapeAlertFlags = append(health.TapeAlertFlags, tapeAlertLabel(flag))
		}
	}

	seconds := elapsed.Seconds()
	if seconds > 0 {
		health.ThroughputMBps = float64(bytesWritten) / bytesPerMB / seconds
		if floorKnown {
			health.BelowFloor = health.ThroughputMBps < floorMBps
		}
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
			Help:      "Sustained write throughput over the tape's write window, in MB/s (bytes written to tape (slices + PAR2) / elapsed).",
		}, []string{"barcode"}),
		repositions: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tape_archiver",
			Subsystem: "write",
			Name:      "repositions",
			Help:      "Drive reposition (back-hitch) count (total_suspended_writes) from SCSI Tape usage log page 0x30. Only exported when the counter was measured (drive supports the page).",
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
			Help:      "1 when the measured throughput was below the tape generation's speed-matching floor, else 0. Unset when the generation has no known floor.",
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
	m.tapeAlerts.With(labels).Set(float64(len(health.TapeAlertFlags)))

	// Only export the reposition gauge when the counter was actually read; leaving
	// it unset when the drive does not support page 0x30 avoids implying a measured
	// zero back-hitch count from an unread counter (SPEC §14).
	if health.RepositionsMeasured {
		m.repositions.With(labels).Set(float64(health.Repositions))
	}

	// Only export the below-floor gauge when a floor is known; leaving it unset for
	// an unknown generation avoids implying "not below floor" when the floor could
	// not be evaluated.
	if health.FloorKnown {
		m.belowFloor.With(labels).Set(boolToFloat(health.BelowFloor))
	}
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
	// BytesWritten is the total bytes physically written to the tape in the measured
	// window — the archive slices plus the PAR2 recovery files — the numerator of
	// the throughput.
	BytesWritten int64
	// Elapsed is the write-window wall-clock the workflow measured around the
	// WriteTree → FinalizeTape span — the denominator of the throughput.
	Elapsed time.Duration
	// FloorMBps is the speed-matching floor for the tape generation being written,
	// derived by the workflow from the configured native capacity. Meaningful only
	// when FloorKnown is true.
	FloorMBps float64
	// FloorKnown is true when a floor is known for the tape generation. When false
	// no below-floor verdict is made.
	FloorKnown bool
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

	health := evaluateWriteHealth(input.BytesWritten, input.Elapsed, logs, input.FloorMBps, input.FloorKnown)

	a.metrics.record(string(input.Barcode), health)

	return health, nil
}
