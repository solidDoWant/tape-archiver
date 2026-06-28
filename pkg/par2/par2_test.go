package par2_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/par2"
)

// maxRecoveryBlocks mirrors PAR2's hard limit (SPEC §8); the recovery set must
// never exceed it.
const maxRecoveryBlocks = 32768

// dataBlocksPattern extracts the source block count from `par2 verify -v`
// output ("There are a total of N data blocks.").
var dataBlocksPattern = regexp.MustCompile(`total of (\d+) data blocks`)

// TestGenerate exercises the full recovery lifecycle through the real par2
// binary: a recovery set is produced, par2 verifies it, its source block count
// stays within PAR2's 32,768-block limit, and after corrupting part of one
// slice par2 repairs it back to the exact original bytes.
func TestGenerate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		sliceSizes        []int
		redundancyPercent int
		// corruptBytes is the size of the region zeroed in one slice before
		// repair; it stays well within the redundancy so repair must succeed.
		corruptBytes int
	}{
		{
			name:              "several equal slices",
			sliceSizes:        []int{100_000, 100_000, 100_000},
			redundancyPercent: 50,
			corruptBytes:      5_000,
		},
		{
			name:              "single slice",
			sliceSizes:        []int{200_000},
			redundancyPercent: 30,
			corruptBytes:      4_000,
		},
		{
			name:              "uneven slices with small remainder",
			sliceSizes:        []int{128_000, 128_000, 7_000},
			redundancyPercent: 40,
			corruptBytes:      3_000,
		},
		{
			// Total data large enough that the minimum 4-byte block size would
			// blow the 32,768-block limit, forcing a larger computed block size.
			name:              "large total stays within block limit",
			sliceSizes:        []int{1_500_000, 1_500_000, 1_500_000, 1_500_000},
			redundancyPercent: 10,
			corruptBytes:      20_000,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			paths, originals := writeSlices(t, dir, test.sliceSizes)

			recoverySetPath := filepath.Join(dir, "archive.par2")

			err := par2.Generate(t.Context(), recoverySetPath, paths, test.redundancyPercent)
			require.NoError(t, err)

			// A valid recovery set was written: the main index plus at least one
			// volume file holding recovery blocks.
			assert.FileExists(t, recoverySetPath)

			volumes, globErr := filepath.Glob(filepath.Join(dir, "archive.vol*.par2"))
			require.NoError(t, globErr)
			assert.NotEmpty(t, volumes, "recovery set must contain recovery volume files")

			// par2 verifies the set against the intact data, and the source block
			// count stays within PAR2's hard limit (SPEC §8).
			blockCount := par2Verify(t, recoverySetPath)
			assert.LessOrEqual(t, blockCount, maxRecoveryBlocks,
				"source block count must not exceed PAR2's 32,768-block limit")

			// Corrupt a region of the first slice, then repair: par2 must restore
			// the exact original bytes using only the recovery set.
			corrupt(t, paths[0], test.corruptBytes)
			require.NotEqual(t, originals[0], readFile(t, paths[0]),
				"corruption must actually change the slice")

			par2Repair(t, recoverySetPath)

			for index, path := range paths {
				assert.Equal(t, originals[index], readFile(t, path),
					"repaired slice %d must match the original bytes", index)
			}
		})
	}
}

// TestGenerateRejectsInvalidInput confirms Generate validates its arguments
// before invoking par2, so no recovery set is produced for bad input.
func TestGenerateRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// build returns (recoverySetPath, dataFiles, redundancyPercent) for a
		// fresh temp dir.
		build func(t *testing.T, dir string) (string, []string, int)
	}{
		{
			name: "zero redundancy",
			build: func(t *testing.T, dir string) (string, []string, int) {
				paths, _ := writeSlices(t, dir, []int{1000})
				return filepath.Join(dir, "archive.par2"), paths, 0
			},
		},
		{
			name: "redundancy above 100",
			build: func(t *testing.T, dir string) (string, []string, int) {
				paths, _ := writeSlices(t, dir, []int{1000})
				return filepath.Join(dir, "archive.par2"), paths, 101
			},
		},
		{
			name: "no data files",
			build: func(t *testing.T, dir string) (string, []string, int) {
				return filepath.Join(dir, "archive.par2"), nil, 5
			},
		},
		{
			name: "data file outside recovery set directory",
			build: func(t *testing.T, dir string) (string, []string, int) {
				outside := filepath.Join(t.TempDir(), "slice.000")
				require.NoError(t, os.WriteFile(outside, []byte("data"), 0o600))

				return filepath.Join(dir, "archive.par2"), []string{outside}, 5
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			recoverySetPath, dataFiles, redundancy := test.build(t, dir)

			err := par2.Generate(t.Context(), recoverySetPath, dataFiles, redundancy)
			require.Error(t, err)

			// No recovery set should have been produced for invalid input.
			assert.NoFileExists(t, recoverySetPath)
		})
	}
}

// writeSlices creates one file per size in dir, filled with varied bytes, and
// returns the slice paths in order alongside a copy of each file's contents.
func writeSlices(t *testing.T, dir string, sizes []int) ([]string, [][]byte) {
	t.Helper()

	paths := make([]string, len(sizes))
	originals := make([][]byte, len(sizes))

	for index, size := range sizes {
		data := make([]byte, size)

		// A varied, deterministic pattern: distinct across slices and across
		// blocks within a slice, so corruption is genuinely detectable.
		state := uint32(index*2_654_435_761 + 1)
		for byteIndex := range data {
			state = state*1_664_525 + 1_013_904_223
			data[byteIndex] = byte(state >> 24)
		}

		path := filepath.Join(dir, "archive."+pad(index))
		require.NoError(t, os.WriteFile(path, data, 0o600))

		paths[index] = path
		originals[index] = data
	}

	return paths, originals
}

// corrupt overwrites the first n bytes of the file at path with their bitwise
// complement, modeling localized media decay within a single slice.
func corrupt(t *testing.T, path string, n int) {
	t.Helper()

	data := readFile(t, path)
	require.GreaterOrEqual(t, len(data), n, "slice too small to corrupt %d bytes", n)

	for index := 0; index < n; index++ {
		data[index] = ^data[index]
	}

	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// par2Verify runs `par2 verify -v` on the recovery set and returns the reported
// source block count, failing the test if verification does not succeed.
func par2Verify(t *testing.T, recoverySetPath string) int {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "par2", "verify", "-v", filepath.Base(recoverySetPath))
	cmd.Dir = filepath.Dir(recoverySetPath)

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "par2 verify failed: %s", output)

	match := dataBlocksPattern.FindSubmatch(output)
	require.NotNil(t, match, "par2 verify output missing data block count: %s", output)

	blockCount, convErr := strconv.Atoi(string(match[1]))
	require.NoError(t, convErr)

	return blockCount
}

// par2Repair runs `par2 repair` on the recovery set, failing the test if the
// repair does not succeed.
func par2Repair(t *testing.T, recoverySetPath string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "par2", "repair", "-qq", filepath.Base(recoverySetPath))
	cmd.Dir = filepath.Dir(recoverySetPath)

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "par2 repair failed: %s", output)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	return data
}

// pad renders a slice index as the three-digit suffix used by archive slices.
func pad(index int) string {
	digits := []byte{
		byte('0' + index/100%10),
		byte('0' + index/10%10),
		byte('0' + index%10),
	}

	return string(digits)
}
