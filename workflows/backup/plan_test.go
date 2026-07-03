package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// targetRedundancy is a fixed-percentage PAR2 config at the given percent.
func targetRedundancy(percent float64) config.Redundancy {
	return config.Redundancy{TargetPercentage: &percent, SliceSizeBytes: 1 << 20}
}

// fillRedundancy is a fill-to-capacity PAR2 config at the given floor percent.
func fillRedundancy(floor float64) config.Redundancy {
	return config.Redundancy{FillToCapacity: &config.FillConfig{Floor: floor}, SliceSizeBytes: 1 << 20}
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

	// Capacity 1,000,000; PAR2 reserve 10% → each 400,000-byte archive has a
	// 440,000-byte footprint. Usable < native (LTFS reserved), so two fit per
	// tape and the third spills onto a second tape.
	cfg := packConfig(1_000_000, 2, 2, targetRedundancy(10))
	staged := []StagedArchive{
		stagedArchive(0, 400_000),
		stagedArchive(1, 400_000),
		stagedArchive(2, 400_000),
	}

	plan, err := pack(cfg, staged)
	require.NoError(t, err)

	// The N copies are recorded for parallel writing (AC1).
	assert.Equal(t, 2, plan.Copies)

	// LTFS overhead is reserved: usable capacity is strictly below native.
	require.NotEmpty(t, plan.Tapes)

	for _, tape := range plan.Tapes {
		assert.Less(t, tape.UsableBytes, cfg.Library.TapeCapacityBytes,
			"usable capacity must reserve LTFS overhead below native capacity")

		// No tape's planned contents exceed its usable capacity (AC1).
		assert.LessOrEqual(t, tape.PlannedBytes(), tape.UsableBytes,
			"a tape's data plus reserved PAR2 must fit within usable capacity")

		// Each placement reserves PAR2 space on top of the measured data (AC1).
		for _, archive := range tape.Archives {
			assert.Equal(t, int64(40_000), archive.PAR2ReservedBytes,
				"PAR2 must be reserved at the configured percentage of measured size")
			assert.Equal(t, int64(440_000), archive.Footprint())
		}
	}

	// Three 440,000-byte footprints into ~995,000-byte tapes need two tapes.
	assert.Len(t, plan.Tapes, 2)

	// Every staged archive is placed exactly once across all tapes.
	assert.ElementsMatch(t, []int{0, 1, 2}, placedSourceIndices(plan))
}

// TestPackFillModeReservesFloor verifies fill-to-capacity packing reserves PAR2
// at the floor percentage — the minimum footprint that fill mode later grows.
func TestPackFillModeReservesFloor(t *testing.T) {
	t.Parallel()

	cfg := packConfig(10_000_000, 1, 1, fillRedundancy(5))
	plan, err := pack(cfg, []StagedArchive{stagedArchive(0, 1_000_000)})
	require.NoError(t, err)

	require.Len(t, plan.Tapes, 1)
	require.Len(t, plan.Tapes[0].Archives, 1)

	// 5% of 1,000,000 measured bytes is reserved for PAR2.
	assert.Equal(t, int64(50_000), plan.Tapes[0].Archives[0].PAR2ReservedBytes)
}

// TestPackRejectsOversizedArchive covers the measured-size guard: an archive
// whose measured footprint exceeds one tape's usable capacity fails the run,
// naming the source — the Resolve estimate can run below the measured size.
func TestPackRejectsOversizedArchive(t *testing.T) {
	t.Parallel()

	cfg := packConfig(1_000_000, 1, 1, targetRedundancy(10))

	// 950,000 data + 95,000 PAR2 = 1,045,000 > usable (~995,000).
	_, err := pack(cfg, []StagedArchive{stagedArchive(4, 950_000)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sources[4]")
	assert.Contains(t, err.Error(), "exceeds one tape's usable capacity")
}

// TestPackAllowsCopiesExceedingDrives verifies the copy count may exceed the drive
// count: the tape path writes the copies of each logical tape in successive
// drive-sets, so the plan is not bounded by the number of drives (issue #66).
func TestPackAllowsCopiesExceedingDrives(t *testing.T) {
	t.Parallel()

	cfg := packConfig(1_000_000, 3, 2, targetRedundancy(10))

	plan, err := pack(cfg, []StagedArchive{stagedArchive(0, 100_000)})
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
