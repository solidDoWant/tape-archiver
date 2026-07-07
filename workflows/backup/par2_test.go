package backup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/par2"
)

// par2BlockLimit mirrors PAR2's hard 32,768-block limit (SPEC §8); a generated
// recovery set must never exceed it.
const par2BlockLimit = 32768

// dataBlocksPattern extracts the source block count from `par2 verify -v` output
// ("There are a total of N data blocks.").
var dataBlocksPattern = regexp.MustCompile(`total of (\d+) data blocks`)

// TestGeneratePAR2FixedPercentage covers AC2 and AC4: in fixed mode each
// archive's recovery set is sized to the target percentage, its source block
// count stays within PAR2's limit, and the output is staged to disk and
// checksummed.
func TestGeneratePAR2FixedPercentage(t *testing.T) {
	t.Parallel()

	staged := []StagedArchive{
		writeStagedArchive(t, 0, []int{100_000, 100_000}),
		writeStagedArchive(t, 1, []int{120_000}),
	}

	// A generous capacity: at these tiny slice sizes par2's per-block and replicated
	// critical-packet overhead inflates the honest PAR2 reserve (par2.MaxOutputBytes)
	// far past the nominal 20% — negligible at LTO's TB scale, but it would overflow a
	// small test tape at Pack time (issue #148).
	cfg := packConfig(500_000_000, 1, 1, targetRedundancy(20))

	plan, err := pack(cfg, staged)
	require.NoError(t, err)

	sets, err := generatePAR2(t.Context(), cfg, plan, staged)
	require.NoError(t, err)
	require.Len(t, sets, len(staged))

	for index, set := range sets {
		assert.Equal(t, staged[index].SourceIndex, set.SourceIndex)

		// Each recovery set is sized to the configured target percentage (AC2).
		assert.Equal(t, 20, set.RedundancyPercent)

		// The output is a valid recovery set: par2 verifies it and the source
		// block count stays within PAR2's limit (AC2).
		assertStagedRecoverySet(t, staged[index], set)
	}
}

// TestGeneratePAR2FillToCapacity covers AC3: in fill-to-capacity mode the
// percentage is raised uniformly across a tape's archives to consume the tape's
// remaining capacity, at or above the floor, and the block count stays within the
// limit.
func TestGeneratePAR2FillToCapacity(t *testing.T) {
	t.Parallel()

	staged := []StagedArchive{
		writeStagedArchive(t, 0, []int{100_000}),
		writeStagedArchive(t, 1, []int{100_000}),
	}

	// A generous capacity so both tiny archives pack onto one tape with slack for
	// fill to raise the redundancy above the floor. Exact fill percentages are not
	// asserted here: at these sizes par2's fixed critical-packet overhead dominates
	// the honest bound, so the meaningful guarantees are that the fill is uniform,
	// at or above the floor, and (below) that the real recovery set still fits.
	const floor = 5

	cfg := packConfig(500_000_000, 1, 1, fillRedundancy(floor))

	plan, err := pack(cfg, staged)
	require.NoError(t, err)
	require.Len(t, plan.Tapes, 1, "both archives must pack onto a single tape")

	sets, err := generatePAR2(t.Context(), cfg, plan, staged)
	require.NoError(t, err)
	require.Len(t, sets, 2)

	// The fill percentage is uniform across the tape's archives and never below the
	// floor (AC3).
	wantPercent := fillPercent(plan.Tapes[0], floor)
	assert.GreaterOrEqual(t, wantPercent, floor)

	for index, set := range sets {
		assert.Equal(t, wantPercent, set.RedundancyPercent,
			"every archive on a tape gets the same fill percentage")
		assert.GreaterOrEqual(t, set.RedundancyPercent, floor,
			"the fill percentage never drops below the floor")
		assertStagedRecoverySet(t, staged[index], set)
	}
}

// TestGeneratePAR2FillToCapacityFitsRealPAR2 covers AC2 of issue #148: a
// fill-to-capacity tape packed with minimal slack, whose PAR2 sets are generated
// with the real par2 binary (per-slice block padding and replicated critical
// packets included), still fits within the tape's usable capacity — so Verify
// passes after the PAR2 compute instead of aborting the run with an over-capacity
// tape. The naive reservation this fix replaced would have let the real recovery
// set overflow here.
func TestGeneratePAR2FillToCapacityFitsRealPAR2(t *testing.T) {
	t.Parallel()

	// Several small slices per archive so the recovery sets carry real per-slice
	// block padding and widely replicated critical packets — the overhead the fix
	// must account for.
	staged := []StagedArchive{
		writeStagedArchive(t, 0, []int{80_000, 80_000, 80_000}),
		writeStagedArchive(t, 1, []int{64_000, 64_000}),
	}

	const floor = 5

	// A tape sized so the archives pack with only modest slack for parity: fill must
	// grow the recovery sets to consume the slack without overrunning it. At this
	// capacity fill lands well below 100%, so the real recovery set — not a capped
	// one — is what must fit.
	cfg := packConfig(285_000_000, 1, 1, fillRedundancy(floor))

	plan, err := pack(cfg, staged)
	require.NoError(t, err)
	require.Len(t, plan.Tapes, 1, "both archives must pack onto a single tape")

	sets, err := generatePAR2(t.Context(), cfg, plan, staged)
	require.NoError(t, err)
	require.Len(t, sets, len(staged))

	// The fill grew the recovery sets above the floor (there was real slack to fill).
	for _, set := range sets {
		assert.Greater(t, set.RedundancyPercent, floor,
			"fill mode should raise redundancy above the floor when capacity allows")
	}

	// The real on-disk footprint — measured archive slices plus the real par2
	// recovery files — fits within the tape's usable capacity.
	onDisk := measuredTapeFootprint(t, plan.Tapes[0], staged, sets)
	assert.LessOrEqual(t, onDisk, plan.Tapes[0].UsableBytes,
		"the real staged tree (data + real par2 output) must fit the tape's usable capacity")

	// And Verify — the hard pre-write gate — accepts the tree instead of aborting on
	// an over-capacity tape.
	_, err = verify(t.Context(), plan, staged, sets)
	require.NoError(t, err)
}

// measuredTapeFootprint sums the real on-disk sizes of every archive slice and
// PAR2 recovery file on a planned tape, so a test can compare the actual staged
// tree against the tape's usable capacity.
func measuredTapeFootprint(t *testing.T, tape PlannedTape, staged []StagedArchive, sets []PAR2Set) int64 {
	t.Helper()

	slicesByIndex := make(map[int]StagedArchive, len(staged))
	for _, archive := range staged {
		slicesByIndex[archive.SourceIndex] = archive
	}

	par2ByIndex := make(map[int]PAR2Set, len(sets))
	for _, set := range sets {
		par2ByIndex[set.SourceIndex] = set
	}

	var total int64

	for _, placement := range tape.Archives {
		for _, slice := range slicesByIndex[placement.SourceIndex].Slices {
			info, err := os.Stat(slice.Path)
			require.NoError(t, err)

			total += info.Size()
		}

		for _, file := range par2ByIndex[placement.SourceIndex].Files {
			info, err := os.Stat(file.Path)
			require.NoError(t, err)

			total += info.Size()
		}
	}

	return total
}

// TestGeneratePAR2Empty verifies a run with nothing staged generates no recovery
// sets and does not invoke par2.
func TestGeneratePAR2Empty(t *testing.T) {
	t.Parallel()

	sets, err := generatePAR2(t.Context(), packConfig(1_000_000, 1, 1, targetRedundancy(10)), TapePlan{}, nil)
	require.NoError(t, err)
	assert.Empty(t, sets)
}

// TestFillPercent unit-tests the fill-to-capacity percentage purely against its
// contract: the largest percentage, from the floor up to 100, whose honest PAR2
// upper bound (par2.MaxOutputBytes) still fits the tape's remaining capacity. It
// sizes at realistic scale because that bound is dominated by fixed par2 overhead
// at tiny sizes.
func TestFillPercent(t *testing.T) {
	t.Parallel()

	// oneArchiveTape is a single-archive tape of the given usable capacity and data
	// size (one slice), or a data-less tape when data is 0.
	oneArchiveTape := func(usable, data int64) PlannedTape {
		tape := PlannedTape{UsableBytes: usable}
		if data > 0 {
			tape.Archives = []PlannedArchive{{SourceIndex: 0, DataBytes: data, SliceCount: 1}}
		}

		return tape
	}

	t.Run("raises to the largest percent whose real par2 output still fits", func(t *testing.T) {
		t.Parallel()

		tape := oneArchiveTape(130_150_000, 240_000)

		got := fillPercent(tape, 5)
		assert.GreaterOrEqual(t, got, 5, "never below the floor")
		assert.Less(t, got, 100, "capacity here does not permit a full 100%")

		// It is the largest fitting percent: this one fits the slack; one more overflows.
		slack := tape.UsableBytes - tape.DataBytes()
		assert.LessOrEqual(t, tapePAR2Bound(tape, got), slack)
		assert.Greater(t, tapePAR2Bound(tape, got+1), slack,
			"the chosen percent must be the largest whose PAR2 bound fits")
	})

	t.Run("floored when almost no capacity remains", func(t *testing.T) {
		t.Parallel()

		// Usable is exactly the floor's PAR2 reserve above the data, so the fill
		// cannot grow past the floor.
		const data = int64(240_000)

		tape := oneArchiveTape(data+par2.MaxOutputBytes(data, 1, 5), data)
		assert.Equal(t, 5, fillPercent(tape, 5))
	})

	t.Run("capped at 100 percent when capacity is ample", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, 100, fillPercent(oneArchiveTape(5_000_000_000, 240_000), 5))
	})

	t.Run("floor when the tape carries no data", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, 7, fillPercent(oneArchiveTape(300_000_000, 0), 7))
	})
}

// assertStagedRecoverySet verifies a generated recovery set is well-formed: the
// index and at least one volume file are present and staged beside the slices,
// every recorded file exists on disk with a matching size and SHA-256 (AC4), and
// par2 verifies the set with a source block count within PAR2's limit (AC2/AC3).
func assertStagedRecoverySet(t *testing.T, archive StagedArchive, set PAR2Set) {
	t.Helper()

	archiveDir := filepath.Dir(archive.Slices[0].Path)

	require.NotEmpty(t, set.Files, "a recovery set must contain at least the index file")

	var hasIndex, hasVolume bool

	for _, file := range set.Files {
		assert.Equal(t, archiveDir, filepath.Dir(file.Path),
			"PAR2 files must be staged beside the archive's slices")

		info, err := os.Stat(file.Path)
		require.NoError(t, err, "every recorded PAR2 file must exist on disk")
		assert.Equal(t, info.Size(), file.SizeBytes, "recorded PAR2 size must match the file")
		assert.Equal(t, sha256OfFile(t, file.Path), file.SHA256, "recorded PAR2 checksum must match the file")

		base := filepath.Base(file.Path)

		switch {
		case strings.Contains(base, ".vol"):
			hasVolume = true
		case base == par2SetName:
			hasIndex = true
		}
	}

	assert.True(t, hasIndex, "the recovery set must include its index file")
	assert.True(t, hasVolume, "the recovery set must include recovery volume files")

	// par2 verifies the intact data against the set, and the source block count
	// stays within PAR2's 32,768-block limit.
	blocks := par2BlockCount(t, filepath.Join(archiveDir, par2SetName))
	assert.LessOrEqual(t, blocks, par2BlockLimit,
		"source block count must not exceed PAR2's 32,768-block limit")
}

// writeStagedArchive creates a StagedArchive of the given source index whose
// slices are real files on disk (one per size), so par2 can compute parity over
// them. Each archive gets its own staging directory, mirroring Prepare.
func writeStagedArchive(t *testing.T, sourceIndex int, sizes []int) StagedArchive {
	t.Helper()

	dir := t.TempDir()
	slices := make([]StagedSlice, len(sizes))

	var total int64

	for index, size := range sizes {
		data := make([]byte, size)

		// A varied, deterministic pattern so par2 sees real content, distinct
		// across slices.
		state := uint32(index*2_654_435_761 + sourceIndex + 1)
		for byteIndex := range data {
			state = state*1_664_525 + 1_013_904_223
			data[byteIndex] = byte(state >> 24)
		}

		path := filepath.Join(dir, fmt.Sprintf("archive.%03d", index))
		require.NoError(t, os.WriteFile(path, data, 0o600))

		slices[index] = StagedSlice{Path: path, SHA256: sha256OfFile(t, path), SizeBytes: int64(size)}
		total += int64(size)
	}

	return StagedArchive{SourceIndex: sourceIndex, Slices: slices, SizeBytes: total}
}

// par2BlockCount runs `par2 verify -v` on the recovery set and returns the
// reported source block count, failing the test if verification does not succeed.
func par2BlockCount(t *testing.T, recoverySetPath string) int {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "par2", "verify", "-v", filepath.Base(recoverySetPath))
	cmd.Dir = filepath.Dir(recoverySetPath)

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "par2 verify failed: %s", output)

	match := dataBlocksPattern.FindSubmatch(output)
	require.NotNil(t, match, "par2 verify output missing block count: %s", output)

	blocks, err := strconv.Atoi(string(match[1]))
	require.NoError(t, err)

	return blocks
}
