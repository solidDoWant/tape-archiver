package backup

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// alertFlags builds a TapeAlert result with the given active flag descriptions.
func alertFlags(descriptions ...string) tape.TapeAlertResult {
	flags := make([]tape.TapeAlertFlag, 0, len(descriptions))
	for i, description := range descriptions {
		flags = append(flags, tape.TapeAlertFlag{Number: i + 1, Description: description, Set: true})
	}

	return tape.TapeAlertResult{Flags: flags}
}

func TestEvaluateWriteHealth(t *testing.T) {
	t.Parallel()

	const floor = 50.0

	tests := []struct {
		name            string
		stagedBytes     int64
		elapsed         time.Duration
		logs            tape.LogPageResult
		floorMBps       float64
		floorKnown      bool
		wantThroughput  float64
		wantBelowFloor  bool
		wantRepositions int64
		wantAlertCount  int
		wantHealthy     bool
	}{
		{
			// 6 GB in 60 s = 100 MB/s: above the floor, no repositions, no flags.
			name:           "healthy above floor",
			stagedBytes:    6_000_000_000,
			elapsed:        60 * time.Second,
			logs:           tape.LogPageResult{Repositions: 0},
			floorMBps:      floor,
			floorKnown:     true,
			wantThroughput: 100,
			wantHealthy:    true,
		},
		{
			// 2.4 GB in 60 s = 40 MB/s: below a 50 MB/s floor.
			name:           "below floor",
			stagedBytes:    2_400_000_000,
			elapsed:        60 * time.Second,
			logs:           tape.LogPageResult{Repositions: 0},
			floorMBps:      floor,
			floorKnown:     true,
			wantThroughput: 40,
			wantBelowFloor: true,
		},
		{
			name:            "repositions flagged",
			stagedBytes:     6_000_000_000,
			elapsed:         60 * time.Second,
			logs:            tape.LogPageResult{Repositions: 5},
			floorMBps:       floor,
			floorKnown:      true,
			wantThroughput:  100,
			wantRepositions: 5,
		},
		{
			name:           "tapealert flagged",
			stagedBytes:    6_000_000_000,
			elapsed:        60 * time.Second,
			logs:           tape.LogPageResult{TapeAlert: alertFlags("Cleaning required")},
			floorMBps:      floor,
			floorKnown:     true,
			wantThroughput: 100,
			wantAlertCount: 1,
		},
		{
			// A drive that does not support log page 0x24 reports zero repositions
			// (pkg/tape.LogPageReader behaviour) — the tape is still healthy.
			name:           "unsupported reposition page is zero",
			stagedBytes:    6_000_000_000,
			elapsed:        60 * time.Second,
			logs:           tape.LogPageResult{Repositions: 0},
			floorMBps:      floor,
			floorKnown:     true,
			wantThroughput: 100,
			wantHealthy:    true,
		},
		{
			// A generation with no known floor: throughput is reported but no
			// below-floor verdict is made and the tape is not "healthy".
			name:           "unknown floor is not judged",
			stagedBytes:    2_400_000_000,
			elapsed:        60 * time.Second,
			logs:           tape.LogPageResult{Repositions: 0},
			floorMBps:      0,
			floorKnown:     false,
			wantThroughput: 40,
			wantBelowFloor: false,
			wantHealthy:    false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			health := evaluateWriteHealth(test.stagedBytes, test.elapsed, test.logs, test.floorMBps, test.floorKnown)

			assert.True(t, health.Measured)
			assert.InDelta(t, test.wantThroughput, health.ThroughputMBps, 0.001)
			assert.Equal(t, test.floorKnown, health.FloorKnown)
			assert.InDelta(t, test.floorMBps, health.FloorMBps, 0.001)
			assert.Equal(t, test.wantBelowFloor, health.BelowFloor)
			assert.Equal(t, test.wantRepositions, health.Repositions)
			assert.Len(t, health.TapeAlertFlags, test.wantAlertCount)
			assert.Equal(t, test.wantHealthy, health.Healthy())
		})
	}
}

// TestWriteHealthFloor asserts the floor is derived per LTO generation from the
// configured native capacity, and that generations without a sourced floor report
// unknown rather than a guessed value.
func TestWriteHealthFloor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		capacityBytes int64
		wantFloor     float64
		wantKnown     bool
	}{
		{name: "LTO-5", capacityBytes: 1_500_000_000_000, wantFloor: 40, wantKnown: true},
		{name: "LTO-6", capacityBytes: 2_500_000_000_000, wantFloor: 50, wantKnown: true},
		{name: "LTO-7", capacityBytes: 6_000_000_000_000, wantFloor: 100, wantKnown: true},
		{name: "LTO-8", capacityBytes: 12_000_000_000_000, wantFloor: 112, wantKnown: true},
		{name: "LTO-9", capacityBytes: 18_000_000_000_000, wantFloor: 180, wantKnown: true},
		{name: "unrecognized capacity below LTO-5", capacityBytes: 100, wantKnown: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			floor, known := writeHealthFloor(test.capacityBytes)
			assert.Equal(t, test.wantKnown, known)

			if test.wantKnown {
				assert.InDelta(t, test.wantFloor, floor, 0.001)
			}
		})
	}
}

// TestEvaluateWriteHealthLabelsTapeAlertFlags asserts an active flag is rendered
// with its number and description for the report.
func TestEvaluateWriteHealthLabelsTapeAlertFlags(t *testing.T) {
	t.Parallel()

	logs := tape.LogPageResult{TapeAlert: tape.TapeAlertResult{Flags: []tape.TapeAlertFlag{
		{Number: 8, Description: "Cleaning required", Set: true},
		{Number: 3, Description: "Not set", Set: false},
	}}}

	health := evaluateWriteHealth(6_000_000_000, 60*time.Second, logs, 50, true)

	require.Len(t, health.TapeAlertFlags, 1, "only active flags are recorded")
	assert.Equal(t, "8: Cleaning required", health.TapeAlertFlags[0])
}

// TestMeasureWriteHealthReportsThroughputWhenScrapeFails proves the observational
// contract (SPEC §2 principle 2): a log-page scrape failure is tolerated — the
// activity still reports throughput and returns no error, so the run never fails on
// a measurement problem. The cancelled context forces the scrape to fail fast
// without needing tape hardware.
func TestMeasureWriteHealthReportsThroughputWhenScrapeFails(t *testing.T) {
	t.Parallel()

	activities := newWriteHealthActivities(nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	health, err := activities.MeasureWriteHealth(ctx, MeasureWriteHealthInput{
		Device:       "/dev/does-not-exist",
		Barcode:      "TAPE0001L6",
		BytesWritten: 6_000_000_000,
		Elapsed:      60 * time.Second,
		FloorMBps:    50,
		FloorKnown:   true,
	})

	require.NoError(t, err, "a scrape failure must not fail the run")
	assert.True(t, health.Measured)
	assert.InDelta(t, 100.0, health.ThroughputMBps, 0.01)
	assert.False(t, health.BelowFloor)
	assert.Equal(t, int64(0), health.Repositions, "a failed scrape reports zero repositions")
	assert.Empty(t, health.TapeAlertFlags)
}

// TestMeasureWriteHealthRecordsMetrics asserts the throughput, below-floor,
// reposition, and TapeAlert gauges are set for the tape's barcode.
func TestMeasureWriteHealthRecordsMetrics(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	activities := newWriteHealthActivities(reg)
	require.NotNil(t, activities.metrics, "a non-nil registry must enable the gauges")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// 2.4 GB in 60 s = 40 MB/s: below a 50 MB/s floor.
	_, err := activities.MeasureWriteHealth(ctx, MeasureWriteHealthInput{
		Device:       "/dev/does-not-exist",
		Barcode:      "TAPE0002L6",
		BytesWritten: 2_400_000_000,
		Elapsed:      60 * time.Second,
		FloorMBps:    50,
		FloorKnown:   true,
	})
	require.NoError(t, err)

	assert.InDelta(t, 40.0, testutil.ToFloat64(activities.metrics.throughput.WithLabelValues("TAPE0002L6")), 0.01)
	assert.InDelta(t, 1.0, testutil.ToFloat64(activities.metrics.belowFloor.WithLabelValues("TAPE0002L6")), 0.001)
	assert.InDelta(t, 0.0, testutil.ToFloat64(activities.metrics.repositions.WithLabelValues("TAPE0002L6")), 0.001)
	assert.InDelta(t, 0.0, testutil.ToFloat64(activities.metrics.tapeAlerts.WithLabelValues("TAPE0002L6")), 0.001)
}

// TestNewWriteHealthActivitiesNilRegisterer asserts a nil registry (metrics
// disabled) yields nil gauges and that recording is a safe no-op.
func TestNewWriteHealthActivitiesNilRegisterer(t *testing.T) {
	t.Parallel()

	activities := newWriteHealthActivities(nil)
	assert.Nil(t, activities.metrics, "a nil registry disables the gauges")

	// record on the nil metrics holder must not panic.
	assert.NotPanics(t, func() {
		activities.metrics.record("TAPE0001L6", WriteHealth{Measured: true, ThroughputMBps: 100})
	})
}

func TestReportWriteHealthMapping(t *testing.T) {
	t.Parallel()

	assert.Nil(t, reportWriteHealth(WriteHealth{Measured: false}),
		"an unmeasured tape maps to nil so the report renders \"not measured\"")

	mapped := reportWriteHealth(WriteHealth{
		Measured:       true,
		ThroughputMBps: 100,
		FloorMBps:      50,
		FloorKnown:     true,
		BelowFloor:     false,
		Repositions:    2,
		TapeAlertFlags: []string{"8: Cleaning required"},
	})

	require.NotNil(t, mapped)
	assert.InDelta(t, 100.0, mapped.ThroughputMBps, 0.001)
	assert.InDelta(t, 50.0, mapped.FloorMBps, 0.001)
	assert.True(t, mapped.FloorKnown)
	assert.Equal(t, int64(2), mapped.Repositions)
	assert.Equal(t, []string{"8: Cleaning required"}, mapped.TapeAlertFlags)
	assert.False(t, mapped.Healthy, "repositions make the tape unhealthy")
}

// TestTapeWrittenBytes asserts the throughput numerator sums every byte physically
// written to the tape in the measured window — the archive slices AND the PAR2
// recovery files — so it matches the WriteTree → FinalizeTape denominator (copyTape
// writes both). Slice-only counting understated the rate by the PAR2 fraction (#146).
func TestTapeWrittenBytes(t *testing.T) {
	t.Parallel()

	archives := []TapeWriteArchive{
		{
			Slices:    []StagedSlice{{SizeBytes: 1_000}, {SizeBytes: 2_000}},
			PAR2Files: []StagedSlice{{SizeBytes: 9_000}},
		},
		{
			Slices:    []StagedSlice{{SizeBytes: 3_000}},
			PAR2Files: []StagedSlice{{SizeBytes: 300}, {SizeBytes: 700}},
		},
	}

	// slices 1_000 + 2_000 + 3_000 = 6_000; PAR2 9_000 + 300 + 700 = 10_000.
	assert.Equal(t, int64(16_000), tapeWrittenBytes(archives),
		"PAR2 bytes are included — they are copied inside the measured write window")
}

// TestWriteHealthNotFalseFlaggedByPAR2Window is the AC3 regression: a fill-to-capacity
// tape whose write window includes copying PAR2 at maximum parity, streamed at or above
// the generation floor for ALL bytes written, must NOT read below-floor, and the
// reported throughput must reflect the true sustained rate. The old slice-only numerator
// divided by the PAR2-inclusive window understated the rate (up to ~2x in fill mode) and
// false-flagged a healthy drive.
func TestWriteHealthNotFalseFlaggedByPAR2Window(t *testing.T) {
	t.Parallel()

	// LTO-9 floor is 180 MB/s. Fill-to-capacity at maximum parity: PAR2 ≈ the slice
	// bytes, so counting slices only would halve the measured rate.
	const floor = 180.0

	sliceBytes := int64(18_000_000_000_000) // ~full LTO-9 native capacity of slices
	par2Bytes := int64(18_000_000_000_000)  // maximum parity in fill mode

	archives := []TapeWriteArchive{{
		Slices:    []StagedSlice{{SizeBytes: sliceBytes}},
		PAR2Files: []StagedSlice{{SizeBytes: par2Bytes}},
	}}

	written := tapeWrittenBytes(archives)
	require.Equal(t, sliceBytes+par2Bytes, written)

	// The drive streamed all 36 TB in 150_000 s = 240 MB/s — comfortably above the
	// 180 MB/s floor. Elapsed is the true write-window span (slices + PAR2 copied).
	elapsed := 150_000 * time.Second

	health := evaluateWriteHealth(written, elapsed, tape.LogPageResult{}, floor, true)

	assert.InDelta(t, 240.0, health.ThroughputMBps, 0.001, "throughput reflects all bytes written to tape")
	assert.False(t, health.BelowFloor, "a drive above its floor for all written bytes must not be flagged below-floor")

	// Prove the old slice-only numerator would have false-flagged this healthy drive:
	// 18 TB ÷ 150_000 s = 120 MB/s, spuriously below the 180 MB/s floor.
	slicesOnly := evaluateWriteHealth(sliceBytes, elapsed, tape.LogPageResult{}, floor, true)
	assert.InDelta(t, 120.0, slicesOnly.ThroughputMBps, 0.001)
	assert.True(t, slicesOnly.BelowFloor,
		"slice-only counting mismeasures the fill-to-capacity window as below floor — the bug this fixes")
}
