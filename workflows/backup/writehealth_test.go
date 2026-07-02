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

	tests := []struct {
		name            string
		stagedBytes     int64
		elapsed         time.Duration
		logs            tape.LogPageResult
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
			wantThroughput: 100,
			wantHealthy:    true,
		},
		{
			// 2.4 GB in 60 s = 40 MB/s: below the ~50 MB/s LTO-6 floor.
			name:           "below floor",
			stagedBytes:    2_400_000_000,
			elapsed:        60 * time.Second,
			logs:           tape.LogPageResult{Repositions: 0},
			wantThroughput: 40,
			wantBelowFloor: true,
		},
		{
			name:            "repositions flagged",
			stagedBytes:     6_000_000_000,
			elapsed:         60 * time.Second,
			logs:            tape.LogPageResult{Repositions: 5},
			wantThroughput:  100,
			wantRepositions: 5,
		},
		{
			name:           "tapealert flagged",
			stagedBytes:    6_000_000_000,
			elapsed:        60 * time.Second,
			logs:           tape.LogPageResult{TapeAlert: alertFlags("Cleaning required")},
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
			wantThroughput: 100,
			wantHealthy:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			health := evaluateWriteHealth(test.stagedBytes, test.elapsed, test.logs)

			assert.True(t, health.Measured)
			assert.InDelta(t, test.wantThroughput, health.ThroughputMBps, 0.001)
			assert.InDelta(t, lto6SpeedMatchingFloorMBps, health.FloorMBps, 0.001)
			assert.Equal(t, test.wantBelowFloor, health.BelowFloor)
			assert.Equal(t, test.wantRepositions, health.Repositions)
			assert.Len(t, health.TapeAlertFlags, test.wantAlertCount)
			assert.Equal(t, test.wantHealthy, health.Healthy())
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

	health := evaluateWriteHealth(6_000_000_000, 60*time.Second, logs)

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
		Device:      "/dev/does-not-exist",
		Barcode:     "TAPE0001L6",
		StagedBytes: 6_000_000_000,
		Elapsed:     60 * time.Second,
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

	// 2.4 GB in 60 s = 40 MB/s: below the floor.
	_, err := activities.MeasureWriteHealth(ctx, MeasureWriteHealthInput{
		Device:      "/dev/does-not-exist",
		Barcode:     "TAPE0002L6",
		StagedBytes: 2_400_000_000,
		Elapsed:     60 * time.Second,
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
		FloorMBps:      lto6SpeedMatchingFloorMBps,
		BelowFloor:     false,
		Repositions:    2,
		TapeAlertFlags: []string{"8: Cleaning required"},
	})

	require.NotNil(t, mapped)
	assert.InDelta(t, 100.0, mapped.ThroughputMBps, 0.001)
	assert.InDelta(t, lto6SpeedMatchingFloorMBps, mapped.FloorMBps, 0.001)
	assert.Equal(t, int64(2), mapped.Repositions)
	assert.Equal(t, []string{"8: Cleaning required"}, mapped.TapeAlertFlags)
	assert.False(t, mapped.Healthy, "repositions make the tape unhealthy")
}

// TestStagedBytes asserts the throughput numerator sums only the archive slice
// bytes (StagedArchive.SizeBytes), excluding PAR2 recovery files (issue #70).
func TestStagedBytes(t *testing.T) {
	t.Parallel()

	archives := []TapeWriteArchive{
		{
			Slices:    []StagedSlice{{SizeBytes: 1_000}, {SizeBytes: 2_000}},
			PAR2Files: []StagedSlice{{SizeBytes: 9_000}},
		},
		{
			Slices: []StagedSlice{{SizeBytes: 3_000}},
		},
	}

	assert.Equal(t, int64(6_000), stagedBytes(archives), "PAR2 bytes are excluded from staged bytes")
}
