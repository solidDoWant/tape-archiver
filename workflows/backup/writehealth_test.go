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

	// measuredZero: page 0x30 read, counter zero (the clean-drive case).
	measuredZero := func() tape.LogPageResult {
		return tape.LogPageResult{RepositionsMeasured: true}
	}

	tests := []struct {
		name                    string
		stagedBytes             int64
		elapsed                 time.Duration
		logs                    tape.LogPageResult
		floorMBps               float64
		floorKnown              bool
		wantThroughput          float64
		wantUnmeasurable        bool
		wantBelowFloor          bool
		wantRepositions         int64
		wantRepositionsMeasured bool
		wantAlertCount          int
		wantHealthy             bool
	}{
		{
			// 6 GB in 60 s = 100 MB/s: above the floor, no repositions, no flags.
			name:                    "healthy above floor",
			stagedBytes:             6_000_000_000,
			elapsed:                 60 * time.Second,
			logs:                    measuredZero(),
			floorMBps:               floor,
			floorKnown:              true,
			wantThroughput:          100,
			wantRepositionsMeasured: true,
			wantHealthy:             true,
		},
		{
			// 2.4 GB in 60 s = 40 MB/s: below a 50 MB/s floor.
			name:                    "below floor",
			stagedBytes:             2_400_000_000,
			elapsed:                 60 * time.Second,
			logs:                    measuredZero(),
			floorMBps:               floor,
			floorKnown:              true,
			wantThroughput:          40,
			wantBelowFloor:          true,
			wantRepositionsMeasured: true,
		},
		{
			// Any back-hitch is unhealthy even at good throughput (AC1).
			name:                    "repositions flagged",
			stagedBytes:             6_000_000_000,
			elapsed:                 60 * time.Second,
			logs:                    tape.LogPageResult{Repositions: 5, RepositionsMeasured: true},
			floorMBps:               floor,
			floorKnown:              true,
			wantThroughput:          100,
			wantRepositions:         5,
			wantRepositionsMeasured: true,
			wantHealthy:             false,
		},
		{
			name:                    "tapealert flagged",
			stagedBytes:             6_000_000_000,
			elapsed:                 60 * time.Second,
			logs:                    tape.LogPageResult{TapeAlert: alertFlags("Cleaning required"), RepositionsMeasured: true},
			floorMBps:               floor,
			floorKnown:              true,
			wantThroughput:          100,
			wantRepositionsMeasured: true,
			wantAlertCount:          1,
		},
		{
			// Page 0x30 unsupported: an unread counter never certifies a clean
			// write (AC3).
			name:                    "unmeasured reposition counter is not healthy",
			stagedBytes:             6_000_000_000,
			elapsed:                 60 * time.Second,
			logs:                    tape.LogPageResult{RepositionsMeasured: false},
			floorMBps:               floor,
			floorKnown:              true,
			wantThroughput:          100,
			wantRepositionsMeasured: false,
			wantHealthy:             false,
		},
		{
			// No known floor: no below-floor verdict and not "healthy".
			name:                    "unknown floor is not judged",
			stagedBytes:             2_400_000_000,
			elapsed:                 60 * time.Second,
			logs:                    measuredZero(),
			floorMBps:               0,
			floorKnown:              false,
			wantThroughput:          40,
			wantBelowFloor:          false,
			wantRepositionsMeasured: true,
			wantHealthy:             false,
		},
		{
			// Prod #409: ~1.9 MB copied quickly. Too little data to judge
			// streaming: no rate, no below-floor verdict, not failed for it.
			name:                    "tiny write is unmeasurable",
			stagedBytes:             1_900_000,
			elapsed:                 2 * time.Second,
			logs:                    measuredZero(),
			floorMBps:               floor,
			floorKnown:              true,
			wantUnmeasurable:        true,
			wantRepositionsMeasured: true,
			wantHealthy:             true,
		},
		{
			// Just under the threshold, even at a rate that would be below floor.
			name:                    "just under the minimum is unmeasurable",
			stagedBytes:             minMeasurableWriteBytes - 1,
			elapsed:                 time.Hour,
			logs:                    measuredZero(),
			floorMBps:               floor,
			floorKnown:              true,
			wantUnmeasurable:        true,
			wantRepositionsMeasured: true,
			wantHealthy:             true,
		},
		{
			// 1 GB in 100 s = 10 MB/s: large enough to judge, so below floor.
			name:                    "minimum-size slow write is below floor",
			stagedBytes:             minMeasurableWriteBytes,
			elapsed:                 100 * time.Second,
			logs:                    measuredZero(),
			floorMBps:               floor,
			floorKnown:              true,
			wantThroughput:          10,
			wantBelowFloor:          true,
			wantRepositionsMeasured: true,
		},
		{
			// An unknown duration (e.g. a pre-#409 WriteTree) is not a 0 MB/s rate.
			name:                    "zero duration is unmeasurable",
			stagedBytes:             6_000_000_000,
			elapsed:                 0,
			logs:                    measuredZero(),
			floorMBps:               floor,
			floorKnown:              true,
			wantUnmeasurable:        true,
			wantRepositionsMeasured: true,
			wantHealthy:             true,
		},
		{
			// Unmeasurable throughput does not mask other faults.
			name:                    "tiny write with repositions is unhealthy",
			stagedBytes:             1_900_000,
			elapsed:                 2 * time.Second,
			logs:                    tape.LogPageResult{Repositions: 3, RepositionsMeasured: true},
			floorMBps:               floor,
			floorKnown:              true,
			wantUnmeasurable:        true,
			wantRepositions:         3,
			wantRepositionsMeasured: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			health := evaluateWriteHealth(test.stagedBytes, test.elapsed, test.logs, test.floorMBps, test.floorKnown)

			assert.True(t, health.Measured)
			assert.InDelta(t, test.wantThroughput, health.ThroughputMBps, 0.001)
			assert.Equal(t, test.wantUnmeasurable, health.ThroughputUnmeasurable)
			assert.Equal(t, test.floorKnown, health.FloorKnown)
			assert.InDelta(t, test.floorMBps, health.FloorMBps, 0.001)
			assert.Equal(t, test.wantBelowFloor, health.BelowFloor)
			assert.Equal(t, test.wantRepositions, health.Repositions)
			assert.Equal(t, test.wantRepositionsMeasured, health.RepositionsMeasured)
			assert.Len(t, health.TapeAlertFlags, test.wantAlertCount)
			assert.Equal(t, test.wantHealthy, health.Healthy())
		})
	}
}

// TestWriteHealthFloor asserts per-generation floors and "unknown" (not a
// guess) for generations without a sourced floor.
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

// TestEvaluateWriteHealthLabelsTapeAlertFlags asserts flag label rendering.
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

// TestMeasureWriteHealthReportsThroughputWhenScrapeFails proves a scrape failure
// (forced by a cancelled context) still reports throughput with no error.
func TestMeasureWriteHealthReportsThroughputWhenScrapeFails(t *testing.T) {
	t.Parallel()

	activities := newWriteHealthActivities(nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	measurement, err := activities.MeasureWriteHealth(ctx, MeasureWriteHealthInput{
		Device:       "/dev/does-not-exist",
		Barcode:      "TAPE0001L6",
		BytesWritten: 6_000_000_000,
		Elapsed:      60 * time.Second,
		FloorMBps:    50,
		FloorKnown:   true,
	})

	require.NoError(t, err, "a scrape failure must not fail the run")

	health := measurement.Health
	assert.True(t, health.Measured)
	assert.InDelta(t, 100.0, health.ThroughputMBps, 0.01)
	assert.False(t, health.BelowFloor)
	assert.Equal(t, int64(0), health.Repositions, "a failed scrape reports zero repositions")
	assert.Empty(t, health.TapeAlertFlags)

	// The generation read fails too and degrades to "unknown" (#340).
	assert.Equal(t, "unknown", measurement.MediaGeneration,
		"an unreadable medium generation must be reported as unknown, not guessed")
}

// TestMeasureWriteHealthRecordsMetrics asserts the gauges are set per barcode.
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

	assert.InDelta(t, 40.0, testutil.ToFloat64(activities.metrics.throughput.WithLabelValues("TAPE0002L6", "false")), 0.01)
	assert.InDelta(t, 1.0, testutil.ToFloat64(activities.metrics.belowFloor.WithLabelValues("TAPE0002L6", "false")), 0.001)
	assert.InDelta(t, 0.0, testutil.ToFloat64(activities.metrics.repositions.WithLabelValues("TAPE0002L6", "false")), 0.001)
	assert.InDelta(t, 0.0, testutil.ToFloat64(activities.metrics.tapeAlerts.WithLabelValues("TAPE0002L6", "false")), 0.001)
}

// TestMeasureWriteHealthUnmeasurableMetrics: a write too small to judge exports
// no throughput reading (dropping any stale one) and below_floor 0 (#409).
func TestMeasureWriteHealthUnmeasurableMetrics(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	activities := newWriteHealthActivities(reg)
	require.NotNil(t, activities.metrics)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	const barcode = "TAPE0003L5"

	input := MeasureWriteHealthInput{
		Device: "/dev/does-not-exist", Barcode: barcode,
		BytesWritten: 2_400_000_000, Elapsed: 60 * time.Second, FloorMBps: 40, FloorKnown: true,
	}

	// An earlier measurable write of the same barcode leaves a reading behind.
	_, err := activities.MeasureWriteHealth(ctx, input)
	require.NoError(t, err)
	require.Equal(t, 1, testutil.CollectAndCount(activities.metrics.throughput))

	input.BytesWritten = 1_900_000
	measurement, err := activities.MeasureWriteHealth(ctx, input)
	require.NoError(t, err)

	assert.True(t, measurement.Health.ThroughputUnmeasurable)
	assert.Equal(t, 0, testutil.CollectAndCount(activities.metrics.throughput),
		"an unmeasurable write must not export a throughput reading")
	assert.InDelta(t, 0.0, testutil.ToFloat64(activities.metrics.belowFloor.WithLabelValues(barcode, "false")), 0.001,
		"an unmeasurable write is not below floor")
}

// TestMeasureWriteHealthDryRunKeepsRealSeries: a dry run writes a virtual tape
// carrying a real barcode; its reading must not overwrite the real tape's (#412).
func TestMeasureWriteHealthDryRunKeepsRealSeries(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	activities := newWriteHealthActivities(reg)
	require.NotNil(t, activities.metrics)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	const barcode = "JCY632L5"

	// Real write: 6 GB in 60 s = 100 MB/s.
	_, err := activities.MeasureWriteHealth(ctx, MeasureWriteHealthInput{
		Device: "/dev/does-not-exist", Barcode: barcode,
		BytesWritten: 6_000_000_000, Elapsed: 60 * time.Second, FloorMBps: 40, FloorKnown: true,
	})
	require.NoError(t, err)

	// Dry run of the same barcode: 2.4 GB in 120 s = 20 MB/s, below floor.
	_, err = activities.MeasureWriteHealth(ctx, MeasureWriteHealthInput{
		Device: "/dev/does-not-exist", Barcode: barcode,
		BytesWritten: 2_400_000_000, Elapsed: 120 * time.Second, FloorMBps: 40, FloorKnown: true,
		DryRun: true,
	})
	require.NoError(t, err)

	assert.InDelta(t, 100.0, testutil.ToFloat64(activities.metrics.throughput.WithLabelValues(barcode, "false")), 0.01,
		"a dry run must not overwrite the real tape's throughput")
	assert.InDelta(t, 0.0, testutil.ToFloat64(activities.metrics.belowFloor.WithLabelValues(barcode, "false")), 0.001,
		"a dry run must not mark the real tape below floor")
	assert.InDelta(t, 20.0, testutil.ToFloat64(activities.metrics.throughput.WithLabelValues(barcode, "true")), 0.01)
	assert.InDelta(t, 1.0, testutil.ToFloat64(activities.metrics.belowFloor.WithLabelValues(barcode, "true")), 0.001)
}

// TestNewWriteHealthActivitiesNilRegisterer asserts a nil registry is a safe no-op.
func TestNewWriteHealthActivitiesNilRegisterer(t *testing.T) {
	t.Parallel()

	activities := newWriteHealthActivities(nil)
	assert.Nil(t, activities.metrics, "a nil registry disables the gauges")

	assert.NotPanics(t, func() {
		activities.metrics.record("TAPE0001L6", false, WriteHealth{Measured: true, ThroughputMBps: 100})
	})
}

func TestReportWriteHealthMapping(t *testing.T) {
	t.Parallel()

	assert.Nil(t, reportWriteHealth(WriteHealth{Measured: false}),
		"an unmeasured tape maps to nil so the report renders \"not measured\"")

	mapped := reportWriteHealth(WriteHealth{
		Measured:            true,
		ThroughputMBps:      100,
		FloorMBps:           50,
		FloorKnown:          true,
		BelowFloor:          false,
		Repositions:         2,
		RepositionsMeasured: true,
		TapeAlertFlags:      []string{"8: Cleaning required"},
	})

	require.NotNil(t, mapped)
	assert.InDelta(t, 100.0, mapped.ThroughputMBps, 0.001)
	assert.InDelta(t, 50.0, mapped.FloorMBps, 0.001)
	assert.True(t, mapped.FloorKnown)
	assert.Equal(t, int64(2), mapped.Repositions)
	assert.True(t, mapped.RepositionsMeasured)
	assert.Equal(t, []string{"8: Cleaning required"}, mapped.TapeAlertFlags)
	assert.False(t, mapped.Healthy, "repositions make the tape unhealthy")
	assert.False(t, mapped.ThroughputUnmeasurable)

	unmeasurable := reportWriteHealth(WriteHealth{
		Measured: true, ThroughputUnmeasurable: true, FloorMBps: 40, FloorKnown: true, RepositionsMeasured: true,
	})
	require.NotNil(t, unmeasurable)
	assert.True(t, unmeasurable.ThroughputUnmeasurable, "the report must learn the throughput was not measurable")
	assert.True(t, unmeasurable.Healthy, "unmeasurable throughput alone never makes a tape unhealthy")
}

// TestTapeWrittenBytes asserts the throughput numerator counts slices AND PAR2,
// matching the write-window denominator (#146).
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

// TestWriteHealthNotFalseFlaggedByPAR2Window is the AC3 regression: a slice-only
// numerator over the PAR2-inclusive window halved the rate in fill mode and
// false-flagged a healthy drive.
func TestWriteHealthNotFalseFlaggedByPAR2Window(t *testing.T) {
	t.Parallel()

	// LTO-9 floor 180 MB/s; at maximum parity PAR2 ≈ slice bytes.
	const floor = 180.0

	sliceBytes := int64(18_000_000_000_000) // ~full LTO-9 native capacity of slices
	par2Bytes := int64(18_000_000_000_000)  // maximum parity in fill mode

	archives := []TapeWriteArchive{{
		Slices:    []StagedSlice{{SizeBytes: sliceBytes}},
		PAR2Files: []StagedSlice{{SizeBytes: par2Bytes}},
	}}

	written := tapeWrittenBytes(archives)
	require.Equal(t, sliceBytes+par2Bytes, written)

	// 36 TB in 150_000 s = 240 MB/s, above the floor.
	elapsed := 150_000 * time.Second

	health := evaluateWriteHealth(written, elapsed, tape.LogPageResult{}, floor, true)

	assert.InDelta(t, 240.0, health.ThroughputMBps, 0.001, "throughput reflects all bytes written to tape")
	assert.False(t, health.BelowFloor, "a drive above its floor for all written bytes must not be flagged below-floor")

	// The old numerator: 18 TB / 150_000 s = 120 MB/s, spuriously below.
	slicesOnly := evaluateWriteHealth(sliceBytes, elapsed, tape.LogPageResult{}, floor, true)
	assert.InDelta(t, 120.0, slicesOnly.ThroughputMBps, 0.001)
	assert.True(t, slicesOnly.BelowFloor,
		"slice-only counting mismeasures the fill-to-capacity window as below floor — the bug this fixes")
}
