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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/par2"
)

// par2BlockLimit mirrors PAR2's hard 32,768-block limit (SPEC §8).
const par2BlockLimit = 32768

// dataBlocksPattern parses `par2 verify -v` ("There are a total of N data blocks.").
var dataBlocksPattern = regexp.MustCompile(`total of (\d+) data blocks`)

// TestGeneratePAR2FixedPercentage covers AC2 and AC4: sets are sized to the target
// percentage, stay within the block limit, and are staged and checksummed.
func TestGeneratePAR2FixedPercentage(t *testing.T) {
	t.Parallel()

	staged := []StagedArchive{
		writeStagedArchive(t, 0, []int{100_000, 100_000}),
		writeStagedArchive(t, 1, []int{120_000}),
	}

	// Generous capacity: at tiny slice sizes par2's fixed overhead inflates the
	// honest reserve far past 20% and would overflow a small tape at Pack (#148).
	cfg := packConfig(500_000_000, 1, 1, targetRedundancy(20))

	plan, err := pack(cfg, staged)
	require.NoError(t, err)

	sets, err := generatePAR2(t.Context(), cfg, plan, staged, nil)
	require.NoError(t, err)
	require.Len(t, sets, len(staged))

	for index, set := range sets {
		assert.Equal(t, staged[index].SourceIndex, set.SourceIndex)

		assert.Equal(t, 20, set.RedundancyPercent)

		assertStagedRecoverySet(t, staged[index], set)
	}
}

// TestGeneratePAR2FillToCapacity covers AC3: fill raises the percentage uniformly
// per tape, at or above the floor, within the block limit.
func TestGeneratePAR2FillToCapacity(t *testing.T) {
	t.Parallel()

	staged := []StagedArchive{
		writeStagedArchive(t, 0, []int{100_000}),
		writeStagedArchive(t, 1, []int{100_000}),
	}

	// Exact fill percentages are not asserted: at these sizes par2's fixed overhead
	// dominates the bound.
	const floor = 5

	cfg := packConfig(500_000_000, 1, 1, fillRedundancy(floor))

	plan, err := pack(cfg, staged)
	require.NoError(t, err)
	require.Len(t, plan.Tapes, 1, "both archives must pack onto a single tape")

	sets, err := generatePAR2(t.Context(), cfg, plan, staged, nil)
	require.NoError(t, err)
	require.Len(t, sets, 2)

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

// TestGeneratePAR2FillToCapacityFitsRealPAR2 covers #148 AC2: a minimally-slack
// fill tape's real par2 output still fits usable capacity, so Verify passes. The
// naive reservation would have overflowed here.
func TestGeneratePAR2FillToCapacityFitsRealPAR2(t *testing.T) {
	t.Parallel()

	// Several slices per archive exercise per-slice padding and replicated packets.
	staged := []StagedArchive{
		writeStagedArchive(t, 0, []int{80_000, 80_000, 80_000}),
		writeStagedArchive(t, 1, []int{64_000, 64_000}),
	}

	const floor = 5

	// Modest slack keeps fill well below 100%, so the uncapped real set must fit.
	cfg := packConfig(285_000_000, 1, 1, fillRedundancy(floor))

	plan, err := pack(cfg, staged)
	require.NoError(t, err)
	require.Len(t, plan.Tapes, 1, "both archives must pack onto a single tape")

	sets, err := generatePAR2(t.Context(), cfg, plan, staged, nil)
	require.NoError(t, err)
	require.Len(t, sets, len(staged))

	for _, set := range sets {
		assert.Greater(t, set.RedundancyPercent, floor,
			"fill mode should raise redundancy above the floor when capacity allows")
	}

	onDisk := measuredTapeFootprint(t, plan.Tapes[0], staged, sets)
	assert.LessOrEqual(t, onDisk, plan.Tapes[0].UsableBytes,
		"the real staged tree (data + real par2 output) must fit the tape's usable capacity")

	_, err = verify(t.Context(), plan, staged, sets)
	require.NoError(t, err)
}

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

// TestGeneratePAR2RetriesAfterPartialAttempt covers AC1 and AC2: a retry over a
// directory already holding recovery files (plus a stale leftover) succeeds with
// complete valid sets and records no stale file.
func TestGeneratePAR2RetriesAfterPartialAttempt(t *testing.T) {
	t.Parallel()

	staged := []StagedArchive{
		writeStagedArchive(t, 0, []int{100_000, 100_000}),
		writeStagedArchive(t, 1, []int{120_000}),
	}

	// Generous capacity: par2's per-block overhead dominates at tiny sizes.
	cfg := packConfig(500_000_000, 1, 1, targetRedundancy(20))

	plan, err := pack(cfg, staged)
	require.NoError(t, err)

	_, err = generatePAR2(t.Context(), cfg, plan, staged, nil)
	require.NoError(t, err)

	staleDir := filepath.Dir(staged[0].Slices[0].Path)
	stalePath := filepath.Join(staleDir, "archive.vol900+99.par2")
	require.NoError(t, os.WriteFile(stalePath, []byte("stale leftover"), 0o600))

	// Without the purge this fails: par2 create refuses existing recovery files.
	sets, err := generatePAR2(t.Context(), cfg, plan, staged, nil)
	require.NoError(t, err)
	require.Len(t, sets, len(staged))

	// AC2: the stale leftover is gone and recorded in no set.
	_, statErr := os.Stat(stalePath)
	assert.True(t, os.IsNotExist(statErr), "stale leftover PAR2 file must be purged from disk")

	for index, set := range sets {
		assert.Equal(t, staged[index].SourceIndex, set.SourceIndex)

		for _, file := range set.Files {
			assert.NotEqual(t, stalePath, file.Path, "no stale file may be recorded")
		}

		assertStagedRecoverySet(t, staged[index], set)
	}
}

func TestGeneratePAR2Empty(t *testing.T) {
	t.Parallel()

	sets, err := generatePAR2(t.Context(), packConfig(1_000_000, 1, 1, targetRedundancy(10)), TapePlan{}, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, sets)
}

// TestFillPercent: the largest percentage in [floor, 100] whose
// par2.MaxOutputBytes fits the slack, sized at realistic scale.
func TestFillPercent(t *testing.T) {
	t.Parallel()

	oneArchiveTape := func(usable, data int64) PlannedTape {
		tape := PlannedTape{UsableBytes: usable}
		if data > 0 {
			tape.Archives = []PlannedArchive{{SourceIndex: 0, DataBytes: data, SliceCount: 1}}
		}

		return tape
	}

	t.Run("raises to the largest percent whose real par2 output still fits", func(t *testing.T) {
		t.Parallel()

		tape := oneArchiveTape(368_000_000, 240_000_000)

		got := fillPercent(tape, 5)
		assert.GreaterOrEqual(t, got, 5, "never below the floor")
		assert.Less(t, got, 100, "capacity here does not permit a full 100%")

		// The largest fitting percent: one more overflows.
		slack := tape.UsableBytes - tape.DataBytes()
		assert.LessOrEqual(t, tapePAR2Bound(tape, got), slack)
		assert.Greater(t, tapePAR2Bound(tape, got+1), slack,
			"the chosen percent must be the largest whose PAR2 bound fits")
	})

	t.Run("floored when almost no capacity remains", func(t *testing.T) {
		t.Parallel()

		// Usable is exactly the floor's reserve above the data.
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

// TestPAR2ProgressTracker uses an injected clock so the ETA is deterministic.
func TestPAR2ProgressTracker(t *testing.T) {
	t.Parallel()

	current := time.Unix(0, 0)
	clock := func() time.Time { return current }
	advance := func(elapsed time.Duration) { current = current.Add(elapsed) }

	archives := []StagedArchive{
		{SourceIndex: 3, SizeBytes: 100},
		{SourceIndex: 7, SizeBytes: 300},
	}

	tracker := newPAR2ProgressTracker(archives, clock)

	assert.Equal(t, PAR2Progress{TotalArchives: 2}, tracker.snapshot())

	// 50/400 = 12.5% after 10s → remaining 350 bytes at 5 B/s = 70s.
	tracker.startArchive(3, 20, 100)
	tracker.updateArchiveProgress(0.5)
	advance(10 * time.Second)

	assert.Equal(t, PAR2Progress{
		TotalArchives:            2,
		CurrentSourceIndex:       3,
		CurrentRedundancyPercent: 20,
		CurrentArchivePercent:    50,
		OverallPercent:           12.5,
		EstimatedTimeRemaining:   70 * time.Second,
	}, tracker.snapshot())

	tracker.finishArchive()

	got := tracker.snapshot()
	assert.Equal(t, 1, got.CompletedArchives)
	assert.Zero(t, got.CurrentSourceIndex, "the in-flight slot clears between archives")
	assert.Equal(t, 25.0, got.OverallPercent, "100 of 400 bytes are done")

	tracker.startArchive(7, 35, 300)
	tracker.updateArchiveProgress(1)
	tracker.finishArchive()

	got = tracker.snapshot()
	assert.Equal(t, 2, got.CompletedArchives)
	assert.Equal(t, 100.0, got.OverallPercent)
	assert.Zero(t, got.EstimatedTimeRemaining, "no time remains once every archive is done")

	clampTracker := newPAR2ProgressTracker(archives, clock)
	clampTracker.startArchive(1, 10, 100)
	clampTracker.updateArchiveProgress(2)
	assert.Equal(t, 100.0, clampTracker.snapshot().CurrentArchivePercent, "fraction above 1 clamps to 100%")
	clampTracker.updateArchiveProgress(-1)
	assert.Equal(t, 0.0, clampTracker.snapshot().CurrentArchivePercent, "fraction below 0 clamps to 0%")

	var nilTracker *par2ProgressTracker

	assert.NotPanics(t, func() {
		nilTracker.startArchive(1, 10, 100)
		nilTracker.updateArchiveProgress(0.5)
		nilTracker.finishArchive()
	})
}

func TestGeneratePAR2ReportsProgress(t *testing.T) {
	t.Parallel()

	staged := []StagedArchive{
		writeStagedArchive(t, 0, []int{100_000}),
		writeStagedArchive(t, 1, []int{100_000}),
	}

	cfg := packConfig(500_000_000, 1, 1, targetRedundancy(20))

	plan, err := pack(cfg, staged)
	require.NoError(t, err)

	tracker := newPAR2ProgressTracker(staged, time.Now)

	sets, err := generatePAR2(t.Context(), cfg, plan, staged, tracker)
	require.NoError(t, err)
	require.Len(t, sets, len(staged))

	got := tracker.snapshot()
	assert.Equal(t, len(staged), got.TotalArchives)
	assert.Equal(t, len(staged), got.CompletedArchives, "every archive must be counted complete")
	assert.Equal(t, 100.0, got.OverallPercent, "all bytes are done once every archive is generated")
	assert.Zero(t, got.EstimatedTimeRemaining, "no time remains once the phase is done")
}

// assertStagedRecoverySet checks the index and volumes sit beside the slices, each
// recorded size and SHA-256 matches (AC4), and par2 verifies the set (AC2/AC3).
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

	blocks := par2BlockCount(t, filepath.Join(archiveDir, par2SetName))
	assert.LessOrEqual(t, blocks, par2BlockLimit,
		"source block count must not exceed PAR2's 32,768-block limit")
}

// writeStagedArchive writes one real slice file per size into its own staging
// directory, mirroring Prepare.
func writeStagedArchive(t *testing.T, sourceIndex int, sizes []int) StagedArchive {
	t.Helper()

	dir := t.TempDir()
	slices := make([]StagedSlice, len(sizes))

	var total int64

	for index, size := range sizes {
		data := make([]byte, size)

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
