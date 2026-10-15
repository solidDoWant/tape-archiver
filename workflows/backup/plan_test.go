package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/par2"
)

// gib: Pack tests plan at multi-GiB scale, where par2's fixed overhead no longer
// dwarfs the data.
const gib = int64(1) << 30

func targetRedundancy(percent float64) config.Redundancy {
	return config.Redundancy{TargetPercentage: &percent}
}

func fillRedundancy(floor float64) config.Redundancy {
	return config.Redundancy{FillToCapacity: &config.FillConfig{Floor: floor}}
}

func packConfig(capacity int64, copies, drives int, redundancy config.Redundancy) config.Config {
	return config.Config{
		Copies:     copies,
		Library:    config.Library{Drives: make([]string, drives), TapeCapacityBytes: capacity},
		Redundancy: redundancy,
	}
}

// stagedArchive is a single-slice StagedArchive of the given measured size.
func stagedArchive(sourceIndex int, size int64) StagedArchive {
	return StagedArchive{
		SourceIndex: sourceIndex,
		Slices:      []StagedSlice{{Path: "slice", SHA256: "", SizeBytes: size}},
		SizeBytes:   size,
	}
}

// TestPackBinPacksWithinCapacity covers AC1: each tape's data plus PAR2 reserve
// stays within usable capacity and every archive is placed exactly once.
func TestPackBinPacksWithinCapacity(t *testing.T) {
	t.Parallel()

	// 12 GiB native, 10% PAR2 → ~4.84 GiB footprints: two per tape, the third
	// spills onto a second tape.
	const archiveBytes = 4 * gib

	cfg := packConfig(12*gib, 2, 2, targetRedundancy(10))
	staged := []StagedArchive{
		stagedArchive(0, archiveBytes),
		stagedArchive(1, archiveBytes),
		stagedArchive(2, archiveBytes),
	}

	plan, err := pack(cfg, staged)
	require.NoError(t, err)

	assert.Equal(t, 2, plan.Copies)

	require.NotEmpty(t, plan.Tapes)

	// The reserve exceeds the naive measured×percent figure (#148).
	wantReserve := par2.MaxOutputBytes(archiveBytes, 1, 10)
	naiveReserve := archiveBytes * 10 / 100
	require.Greater(t, wantReserve, naiveReserve)

	for _, tape := range plan.Tapes {
		assert.Less(t, tape.UsableBytes, cfg.Library.TapeCapacityBytes,
			"usable capacity must reserve LTFS overhead below native capacity")

		assert.LessOrEqual(t, tape.PlannedBytes(), tape.UsableBytes,
			"a tape's data plus reserved PAR2 must fit within usable capacity")

		for _, archive := range tape.Archives {
			assert.Equal(t, 1, archive.SliceCount, "the slice count must be recorded on the plan")
			assert.Equal(t, wantReserve, archive.PAR2ReservedBytes,
				"PAR2 must be reserved as the honest upper bound on real par2 output")
			assert.Greater(t, archive.PAR2ReservedBytes, naiveReserve,
				"the honest reserve must exceed the naive measured×percent figure")
			assert.Equal(t, archiveBytes+wantReserve, archive.Footprint())
		}
	}

	assert.Len(t, plan.Tapes, 2)

	assert.ElementsMatch(t, []int{0, 1, 2}, placedSourceIndices(plan))
}

func TestPackFillModeReservesFloor(t *testing.T) {
	t.Parallel()

	const archiveBytes = 2 * gib

	cfg := packConfig(3*gib, 1, 1, fillRedundancy(5))
	plan, err := pack(cfg, []StagedArchive{stagedArchive(0, archiveBytes)})
	require.NoError(t, err)

	require.Len(t, plan.Tapes, 1)
	require.Len(t, plan.Tapes[0].Archives, 1)

	// The floor's honest bound is reserved, above the naive 5% figure (#148).
	wantReserve := par2.MaxOutputBytes(archiveBytes, 1, 5)
	assert.Equal(t, wantReserve, plan.Tapes[0].Archives[0].PAR2ReservedBytes)
	assert.Greater(t, wantReserve, archiveBytes*5/100)
}

// TestPackRejectsOversizedArchive: the measured size can exceed the Resolve
// estimate, so Pack must reject an oversized footprint, naming the source.
func TestPackRejectsOversizedArchive(t *testing.T) {
	t.Parallel()

	cfg := packConfig(3*gib, 1, 1, targetRedundancy(10))

	// ~2.69 GiB data fits ~2.85 GiB usable, but its ~0.37 GiB PAR2 reserve does not.
	_, err := pack(cfg, []StagedArchive{stagedArchive(4, 2750*(1<<20))})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sources[4]")
	assert.Contains(t, err.Error(), "exceeds one tape's usable capacity")
}

// TestPackAllowsCopiesExceedingDrives: copies are written in successive
// drive-sets, so the plan is not bounded by drive count (#66).
func TestPackAllowsCopiesExceedingDrives(t *testing.T) {
	t.Parallel()

	cfg := packConfig(3*gib, 3, 2, targetRedundancy(10))

	plan, err := pack(cfg, []StagedArchive{stagedArchive(0, 1*gib)})
	require.NoError(t, err)
	assert.Equal(t, 3, plan.Copies, "the plan records the requested copy count even though it exceeds the drives")
	assert.NotEmpty(t, plan.Tapes)
}

func TestPackEmpty(t *testing.T) {
	t.Parallel()

	cfg := packConfig(1_000_000, 2, 2, targetRedundancy(10))

	plan, err := pack(cfg, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, plan.Copies)
	assert.Empty(t, plan.Tapes)
}

func placedSourceIndices(plan TapePlan) []int {
	var indices []int

	for _, tape := range plan.Tapes {
		for _, archive := range tape.Archives {
			indices = append(indices, archive.SourceIndex)
		}
	}

	return indices
}

// TestRedundancyPercentIsConsistent covers AC2: for any valid config the Resolve
// pre-check (par2Fraction) and Pack reservation (par2ReservePercent) agree on the
// configured whole percentage.
func TestRedundancyPercentIsConsistent(t *testing.T) {
	t.Parallel()

	percents := []float64{1, 10, 15, 50, 99, 100}

	for _, percent := range percents {
		for _, mode := range []struct {
			name       string
			redundancy config.Redundancy
		}{
			{name: "fixed target", redundancy: targetRedundancy(percent)},
			{name: "fill floor", redundancy: fillRedundancy(percent)},
		} {
			t.Run(mode.name, func(t *testing.T) {
				t.Parallel()

				// Config validation guarantees a whole number in [1, 100].
				reserve := par2ReservePercent(mode.redundancy)
				fraction := par2Fraction(mode.redundancy)

				assert.Equal(t, int(percent), reserve)
				assert.InDelta(t, fraction*100, float64(reserve), 1e-9)
			})
		}
	}
}
