package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/par2"
)

// gib is one gibibyte. The Pack tests plan at realistic (multi-GiB) tape scale,
// where the honest PAR2 reservation (par2.MaxOutputBytes) is close to the nominal
// redundancy percentage; at tiny byte sizes par2's fixed per-block and critical
// overhead dwarfs the data, so small magic capacities no longer model real tapes.
const gib = int64(1) << 30

// targetRedundancy is a fixed-percentage PAR2 config at the given percent.
func targetRedundancy(percent float64) config.Redundancy {
	return config.Redundancy{TargetPercentage: &percent}
}

// fillRedundancy is a fill-to-capacity PAR2 config at the given floor percent.
func fillRedundancy(floor float64) config.Redundancy {
	return config.Redundancy{FillToCapacity: &config.FillConfig{Floor: floor}}
}

// packConfig is a run config sized for the Pack tests: the given native tape
// capacity, copy count, drive count, and PAR2 mode.
func packConfig(capacity int64, copies, drives int, redundancy config.Redundancy) config.Config {
	return config.Config{
		Copies:     copies,
		Library:    config.Library{Drives: make([]string, drives), TapeCapacityBytes: capacity},
		Redundancy: redundancy,
	}
}

// stagedArchive is a StagedArchive of the given source index and measured size,
// with a single slice of that size (its on-disk contents are irrelevant to
// packing, which plans purely by measured size).
func stagedArchive(sourceIndex int, size int64) StagedArchive {
	return StagedArchive{
		SourceIndex: sourceIndex,
		Slices:      []StagedSlice{{Path: "slice", SHA256: "", SizeBytes: size}},
		SizeBytes:   size,
	}
}

// TestPackBinPacksWithinCapacity covers AC1: archives are bin-packed onto tapes
// by measured size, each tape's planned contents (data plus reserved PAR2) stay
// within capacity after LTFS overhead, and every archive is placed exactly once.
func TestPackBinPacksWithinCapacity(t *testing.T) {
	t.Parallel()

	// Native 12 GiB; PAR2 reserve 10% → each 4 GiB archive has a ~4.84 GiB
	// footprint (data + the honest par2.MaxOutputBytes reserve). Usable < native
	// (LTFS reserved), so two archives fit per tape and the third spills onto a
	// second tape.
	const archiveBytes = 4 * gib

	cfg := packConfig(12*gib, 2, 2, targetRedundancy(10))
	staged := []StagedArchive{
		stagedArchive(0, archiveBytes),
		stagedArchive(1, archiveBytes),
		stagedArchive(2, archiveBytes),
	}

	plan, err := pack(cfg, staged)
	require.NoError(t, err)

	// The N copies are recorded for parallel writing (AC1).
	assert.Equal(t, 2, plan.Copies)

	// LTFS overhead is reserved: usable capacity is strictly below native.
	require.NotEmpty(t, plan.Tapes)

	// The PAR2 reserve is the honest upper bound on real par2 output, above the
	// naive measured×percent figure it replaced (issue #148).
	wantReserve := par2.MaxOutputBytes(archiveBytes, 1, 10)
	naiveReserve := archiveBytes * 10 / 100
	require.Greater(t, wantReserve, naiveReserve)

	for _, tape := range plan.Tapes {
		assert.Less(t, tape.UsableBytes, cfg.Library.TapeCapacityBytes,
			"usable capacity must reserve LTFS overhead below native capacity")

		// No tape's planned contents exceed its usable capacity (AC1).
		assert.LessOrEqual(t, tape.PlannedBytes(), tape.UsableBytes,
			"a tape's data plus reserved PAR2 must fit within usable capacity")

		// Each placement reserves the honest PAR2 upper bound on top of the
		// measured data (AC1), and records the slice count it was sized from.
		for _, archive := range tape.Archives {
			assert.Equal(t, 1, archive.SliceCount, "the slice count must be recorded on the plan")
			assert.Equal(t, wantReserve, archive.PAR2ReservedBytes,
				"PAR2 must be reserved as the honest upper bound on real par2 output")
			assert.Greater(t, archive.PAR2ReservedBytes, naiveReserve,
				"the honest reserve must exceed the naive measured×percent figure")
			assert.Equal(t, archiveBytes+wantReserve, archive.Footprint())
		}
	}

	// Three ~4.84 GiB footprints into ~11.4 GiB usable tapes need two tapes.
	assert.Len(t, plan.Tapes, 2)

	// Every staged archive is placed exactly once across all tapes.
	assert.ElementsMatch(t, []int{0, 1, 2}, placedSourceIndices(plan))
}

// TestPackFillModeReservesFloor verifies fill-to-capacity packing reserves PAR2
// at the floor percentage — the minimum footprint that fill mode later grows.
func TestPackFillModeReservesFloor(t *testing.T) {
	t.Parallel()

	const archiveBytes = 2 * gib

	cfg := packConfig(3*gib, 1, 1, fillRedundancy(5))
	plan, err := pack(cfg, []StagedArchive{stagedArchive(0, archiveBytes)})
	require.NoError(t, err)

	require.Len(t, plan.Tapes, 1)
	require.Len(t, plan.Tapes[0].Archives, 1)

	// The floor's honest PAR2 upper bound (par2.MaxOutputBytes at 5%) is reserved —
	// the minimum footprint fill mode later grows into the tape's slack — and it
	// exceeds the naive 5%-of-measured figure it replaced (issue #148).
	wantReserve := par2.MaxOutputBytes(archiveBytes, 1, 5)
	assert.Equal(t, wantReserve, plan.Tapes[0].Archives[0].PAR2ReservedBytes)
	assert.Greater(t, wantReserve, archiveBytes*5/100)
}

// TestPackRejectsOversizedArchive covers the measured-size guard: an archive
// whose measured footprint exceeds one tape's usable capacity fails the run,
// naming the source — the Resolve estimate can run below the measured size.
func TestPackRejectsOversizedArchive(t *testing.T) {
	t.Parallel()

	cfg := packConfig(3*gib, 1, 1, targetRedundancy(10))

	// An archive whose measured data alone nearly fills the tape cannot also hold
	// its PAR2 reserve: ~2.69 GiB data fits under the ~2.85 GiB usable, but adding
	// its ~0.37 GiB PAR2 reserve pushes the footprint over.
	_, err := pack(cfg, []StagedArchive{stagedArchive(4, 2750*(1<<20))})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sources[4]")
	assert.Contains(t, err.Error(), "exceeds one tape's usable capacity")
}

// TestPackAllowsCopiesExceedingDrives verifies the copy count may exceed the drive
// count: the tape path writes the copies of each logical tape in successive
// drive-sets, so the plan is not bounded by the number of drives (issue #66).
func TestPackAllowsCopiesExceedingDrives(t *testing.T) {
	t.Parallel()

	cfg := packConfig(3*gib, 3, 2, targetRedundancy(10))

	plan, err := pack(cfg, []StagedArchive{stagedArchive(0, 1*gib)})
	require.NoError(t, err)
	assert.Equal(t, 3, plan.Copies, "the plan records the requested copy count even though it exceeds the drives")
	assert.NotEmpty(t, plan.Tapes)
}

// TestPackEmpty verifies a run with nothing staged yields an empty plan that
// still records the copy count, without consulting capacity.
func TestPackEmpty(t *testing.T) {
	t.Parallel()

	cfg := packConfig(1_000_000, 2, 2, targetRedundancy(10))

	plan, err := pack(cfg, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, plan.Copies)
	assert.Empty(t, plan.Tapes)
}

// placedSourceIndices returns the SourceIndex of every archive placed across all
// tapes in the plan, so a test can assert nothing is dropped or duplicated.
func placedSourceIndices(plan TapePlan) []int {
	var indices []int

	for _, tape := range plan.Tapes {
		for _, archive := range tape.Archives {
			indices = append(indices, archive.SourceIndex)
		}
	}

	return indices
}

// TestRedundancyPercentIsConsistent covers AC2: for any config that passes
// validation, the Resolve feasibility pre-check (par2Fraction) and the Pack
// reservation (par2ReservePercent) resolve to the same integer percentage — the
// configured value, with clampPercent and rounding no longer altering it. This
// holds because config validation now rejects out-of-range and fractional
// redundancy percentages, so both paths see the exact configured whole number.
func TestRedundancyPercentIsConsistent(t *testing.T) {
	t.Parallel()

	// Whole percentages spanning the validated [1, 100] range, in both PAR2
	// modes. Each must satisfy config validation and then agree between the
	// feasibility pre-check and the Pack reservation.
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

				// Each percent is a whole number in [1, 100] — exactly what config
				// validation now guarantees before planning (asserted separately in
				// internal/config). Under that guarantee both paths must agree.
				reserve := par2ReservePercent(mode.redundancy)
				fraction := par2Fraction(mode.redundancy)

				// The reservation equals the configured whole percent (no clamping).
				assert.Equal(t, int(percent), reserve)
				// The feasibility fraction is that same percent, so both agree.
				assert.InDelta(t, fraction*100, float64(reserve), 1e-9)
			})
		}
	}
}
