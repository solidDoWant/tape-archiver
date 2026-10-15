package backup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// Write-health measurement (SPEC §2 principle 2, §14) is purely observational:
// it records how well each tape streamed and never gates or fails a run.
// bytesPerMB is the decimal megabyte, as drive rates are quoted.
const bytesPerMB = 1_000_000.0

// minMeasurableWriteBytes is the smallest write whose rate can indicate
// streaming: LTFS's write cache plus the drive buffer (256 MB on LTO-5) absorb a
// smaller copy before the tape catches up, so 1 GB keeps buffering a minority.
const minMeasurableWriteBytes int64 = 1_000_000_000

// speedMatchingFloorsMBps maps an LTO generation to the lowest rate the drive
// can slow to before it must back-hitch; sustained throughput below it means the
// drive likely shoe-shined (SPEC §14). Values are the conservative published
// minimums (vendors differ by a few MB/s) so the flag under- rather than
// over-reports. Unknown generations get no floor rather than a guessed one.
var speedMatchingFloorsMBps = map[string]float64{
	"LTO-5": 40,
	"LTO-6": 50,
	"LTO-7": 100,
	"LTO-8": 112,
	"LTO-9": 180,
}

// writeHealthFloor returns the floor for the generation implied by the native
// capacity; known is false when no sourced floor exists.
func writeHealthFloor(capacityBytes int64) (floorMBps float64, known bool) {
	floor, ok := speedMatchingFloorsMBps[ltoGeneration(capacityBytes)]

	return floor, ok
}

// WriteHealth is the observational per-tape write-health measurement.
type WriteHealth struct {
	// Measured is false when the activity could not run at all.
	Measured bool
	// ThroughputMBps is bytes written (slices + PAR2) / streaming-copy seconds;
	// a zero placeholder when ThroughputUnmeasurable.
	ThroughputMBps float64
	// ThroughputUnmeasurable is true when the write was too small (or its
	// duration unknown) to judge streaming, so no below-floor verdict is made.
	// Its zero value keeps runs recorded before #409 rendering as before.
	ThroughputUnmeasurable bool
	// FloorMBps is meaningless when FloorKnown is false.
	FloorMBps float64
	// FloorKnown false means no below-floor verdict is made.
	FloorKnown bool
	BelowFloor bool
	// Repositions is the back-hitch count (total_suspended_writes, page 0x30).
	Repositions int64
	// RepositionsMeasured is false when page 0x30 was unread, so Repositions is
	// a placeholder and a clean write is never certified from it.
	RepositionsMeasured bool
	// TapeAlertFlags labels the active TapeAlert flags (page 0x2e).
	TapeAlertFlags []string
}

// Healthy reports whether the tape streamed cleanly. A tape with no known floor
// or an unread reposition counter is never healthy, since it cannot be judged.
func (h WriteHealth) Healthy() bool {
	return h.Measured && h.FloorKnown && !h.BelowFloor &&
		h.RepositionsMeasured && h.Repositions == 0 && len(h.TapeAlertFlags) == 0
}

// evaluateWriteHealth computes the verdict. A write below
// minMeasurableWriteBytes or with a non-positive elapsed is unmeasurable: zero
// throughput and no below-floor verdict (not slow, just unjudgeable).
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
	if seconds <= 0 || bytesWritten < minMeasurableWriteBytes {
		health.ThroughputUnmeasurable = true

		return health
	}

	health.ThroughputMBps = float64(bytesWritten) / bytesPerMB / seconds
	if floorKnown {
		health.BelowFloor = health.ThroughputMBps < floorMBps
	}

	return health
}

// tapeAlertLabel renders an active TapeAlert flag for the report and logs.
func tapeAlertLabel(flag tape.TapeAlertFlag) string {
	if flag.Description != "" {
		return fmt.Sprintf("%d: %s", flag.Number, flag.Description)
	}

	return fmt.Sprintf("flag %d", flag.Number)
}

// writeHealthMetrics holds the per-barcode gauges; nil means metrics are off.
type writeHealthMetrics struct {
	throughput  *prometheus.GaugeVec
	repositions *prometheus.GaugeVec
	tapeAlerts  *prometheus.GaugeVec
	belowFloor  *prometheus.GaugeVec
}

// newWriteHealthMetrics registers the gauges; a nil reg yields nil.
func newWriteHealthMetrics(reg prometheus.Registerer) (*writeHealthMetrics, error) {
	if reg == nil {
		return nil, nil
	}

	metrics := &writeHealthMetrics{
		throughput: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tape_archiver",
			Subsystem: "write",
			Name:      "throughput_mbps",
			Help:      "Sustained write throughput while streaming the staged tree to the mounted tape, in MB/s (bytes written to tape (slices + PAR2) / copy duration). Unset when the write was too small to measure.",
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
			Help:      "1 when the measured throughput was below the tape generation's speed-matching floor, else 0 (including a write too small to measure). Unset when the generation has no known floor.",
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

// record sets one tape's gauges; a no-op on nil or an unmeasured tape.
func (m *writeHealthMetrics) record(barcode string, health WriteHealth) {
	if m == nil || !health.Measured {
		return
	}

	labels := prometheus.Labels{"barcode": barcode}

	// Deleted rather than zeroed so a stale reading for the barcode never lingers.
	if health.ThroughputUnmeasurable {
		m.throughput.Delete(labels)
	} else {
		m.throughput.With(labels).Set(health.ThroughputMBps)
	}

	m.tapeAlerts.With(labels).Set(float64(len(health.TapeAlertFlags)))

	// Left unset when unread so a zero is never implied (SPEC §14).
	if health.RepositionsMeasured {
		m.repositions.With(labels).Set(float64(health.Repositions))
	}

	// Left unset for an unknown generation rather than implying "not below".
	if health.FloorKnown {
		m.belowFloor.With(labels).Set(boolToFloat(health.BelowFloor))
	}
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}

	return 0
}

// WriteHealthActivities hosts the data-side MeasureWriteHealth activity.
type WriteHealthActivities struct {
	metrics *writeHealthMetrics
}

// newWriteHealthActivities degrades to metrics-off on a registration failure
// rather than failing worker startup; write-health is observability only.
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
	// Device is the drive's SCSI generic node, scraped for log pages.
	Device  string
	Barcode tape.Barcode
	// BytesWritten is slices plus PAR2 files written in the window.
	BytesWritten int64
	// Elapsed is WriteTree's streaming-copy duration (no mount or unmount); zero
	// means unknown and yields an unmeasurable throughput.
	Elapsed    time.Duration
	FloorMBps  float64
	FloorKnown bool
}

// TapeMeasurement is the post-write scrape of one tape. The medium generation
// must be read now, while loaded: by Report the tape is exported and an empty
// drive describes no medium (#340).
type TapeMeasurement struct {
	Health WriteHealth
	// MediaGeneration is the tape's own generation (what a recoverer needs),
	// not the writing drive's; "unknown" when unreadable.
	MediaGeneration string
}

// MeasureWriteHealth scrapes the drive after the write window. It is read-only
// and retriable, and a scrape failure degrades rather than failing the run.
func (a *WriteHealthActivities) MeasureWriteHealth(ctx context.Context, input MeasureWriteHealthInput) (TapeMeasurement, error) {
	logs, err := tape.NewLogPageReader(input.Device).ReadLogPages(ctx)
	if err != nil {
		slog.WarnContext(ctx, "write-health: could not read drive log pages; reporting throughput only",
			"device", input.Device, "barcode", input.Barcode, "error", err)

		logs = tape.LogPageResult{}
	}

	health := evaluateWriteHealth(input.BytesWritten, input.Elapsed, logs, input.FloorMBps, input.FloorKnown)

	a.metrics.record(string(input.Barcode), health)

	generation, err := tape.MediumGeneration(ctx, input.Device)
	if err != nil {
		slog.WarnContext(ctx, "write-health: could not read the medium's generation; the report will not name one",
			"device", input.Device, "barcode", input.Barcode, "error", err)
	}

	slog.InfoContext(ctx, "write-health: measured tape write performance",
		"barcode", input.Barcode, "throughputMBps", health.ThroughputMBps,
		"throughputUnmeasurable", health.ThroughputUnmeasurable,
		"floorMBps", health.FloorMBps, "floorKnown", health.FloorKnown, "belowFloor", health.BelowFloor,
		"repositions", health.Repositions, "repositionsMeasured", health.RepositionsMeasured,
		"tapeAlertFlags", len(health.TapeAlertFlags), "healthy", health.Healthy(),
		"mediaGeneration", generation)

	return TapeMeasurement{Health: health, MediaGeneration: generation}, nil
}
