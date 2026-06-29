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

	cfg := packConfig(10_000_000, 1, 1, targetRedundancy(20))

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

	// native 301,508 → usable 300,000 (0.5% LTFS reserve). Both 100,000-byte
	// archives land on one tape (data 200,000), leaving 100,000 for PAR2 → a
	// uniform 50% fill, well above the 5% floor.
	const floor = 5

	cfg := packConfig(301_508, 1, 1, fillRedundancy(floor))

	plan, err := pack(cfg, staged)
	require.NoError(t, err)
	require.Len(t, plan.Tapes, 1, "both archives must pack onto a single tape")

	sets, err := generatePAR2(t.Context(), cfg, plan, staged)
	require.NoError(t, err)
	require.Len(t, sets, 2)

	// The fill percentage consumes the tape's remaining capacity, uniformly
	// across both archives on the tape (AC3).
	wantPercent := fillPercent(plan.Tapes[0], floor)
	assert.Equal(t, 50, wantPercent, "remaining capacity should fill to 50%")

	for index, set := range sets {
		assert.Equal(t, wantPercent, set.RedundancyPercent,
			"every archive on a tape gets the same fill percentage")
		assert.GreaterOrEqual(t, set.RedundancyPercent, floor,
			"the fill percentage never drops below the floor")
		assertStagedRecoverySet(t, staged[index], set)
	}
}

// TestGeneratePAR2Empty verifies a run with nothing staged generates no recovery
// sets and does not invoke par2.
func TestGeneratePAR2Empty(t *testing.T) {
	t.Parallel()

	sets, err := generatePAR2(t.Context(), packConfig(1_000_000, 1, 1, targetRedundancy(10)), TapePlan{}, nil)
	require.NoError(t, err)
	assert.Empty(t, sets)
}

// TestFillPercent unit-tests the fill-to-capacity percentage purely, covering the
// raise-to-fill, floor, cap, and degenerate cases.
func TestFillPercent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		us, d int64
		floor int
		want  int
	}{
		{name: "consumes remaining capacity", us: 300_000, d: 200_000, floor: 5, want: 50},
		{name: "floored when little remains", us: 210_000, d: 200_000, floor: 5, want: 5},
		{name: "capped at 100 percent", us: 1_000_000, d: 200_000, floor: 5, want: 100},
		{name: "floor when no data", us: 300_000, d: 0, floor: 7, want: 7},
		{name: "rounded down to stay within the tape", us: 250_001, d: 200_000, floor: 5, want: 25},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			tape := PlannedTape{UsableBytes: test.us}
			if test.d > 0 {
				tape.Archives = []PlannedArchive{{SourceIndex: 0, DataBytes: test.d}}
			}

			assert.Equal(t, test.want, fillPercent(tape, test.floor))
		})
	}
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
