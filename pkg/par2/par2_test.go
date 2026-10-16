package par2_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/par2"
)

const maxSourceBlocks = 32768

var dataBlocksPattern = regexp.MustCompile(`total of (\d+) data blocks`)

// TestGenerate exercises generate, verify, corrupt, and repair through the
// real par2 binary.
func TestGenerate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		sliceSizes        []int
		redundancyPercent int
		// corruptBytes stays well within the redundancy so repair must succeed.
		corruptBytes int
		// memoryBudget, when set, is passed via WithMemoryBudget.
		memoryBudget int64
		// deleteSlice removes the first slice instead of corrupting it.
		deleteSlice bool
	}{
		{
			// A 1 MiB budget forces par2 into many strided passes.
			name:              "multi-pass under a tight memory budget",
			sliceSizes:        []int{4_000_000, 4_000_000, 4_000_000},
			redundancyPercent: 20,
			corruptBytes:      100_000,
			memoryBudget:      1 << 20,
		},
		{
			name:              "multi-pass repairs a deleted slice",
			sliceSizes:        []int{1_000_000, 1_000_000, 1_000_000, 1_000_000},
			redundancyPercent: 30,
			memoryBudget:      1 << 20,
			deleteSlice:       true,
		},
		{
			name:              "single pass repairs a deleted slice",
			sliceSizes:        []int{100_000, 100_000, 100_000, 100_000},
			redundancyPercent: 30,
			deleteSlice:       true,
		},
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
			// A 4-byte block size would blow the block limit here.
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

			var opts []par2.Option

			if test.memoryBudget > 0 {
				plan := par2.PlanMemory(sliceSizes(test.sliceSizes), test.redundancyPercent, test.memoryBudget)
				require.Greater(t, plan.Passes, int64(1), "the budget must force multiple passes")

				opts = append(opts, par2.WithMemoryBudget(test.memoryBudget))
			}

			err := par2.Generate(t.Context(), recoverySetPath, paths, test.redundancyPercent, opts...)
			require.NoError(t, err)

			assert.FileExists(t, recoverySetPath)

			volumes, globErr := filepath.Glob(filepath.Join(dir, "archive.vol*.par2"))
			require.NoError(t, globErr)
			assert.NotEmpty(t, volumes, "recovery set must contain recovery volume files")

			blockCount := par2Verify(t, recoverySetPath)
			assert.LessOrEqual(t, blockCount, maxSourceBlocks,
				"source block count must not exceed PAR2's 32,768-block limit")

			if test.deleteSlice {
				require.NoError(t, os.Remove(paths[0]))
			} else {
				corrupt(t, paths[0], test.corruptBytes)
				require.NotEqual(t, originals[0], readFile(t, paths[0]),
					"corruption must actually change the slice")
			}

			par2Repair(t, recoverySetPath)

			for index, path := range paths {
				assert.Equal(t, originals[index], readFile(t, path),
					"repaired slice %d must match the original bytes", index)
			}
		})
	}
}

func TestGenerateRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// build returns (recoverySetPath, dataFiles, redundancyPercent).
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

			assert.NoFileExists(t, recoverySetPath)
		})
	}
}

func TestGenerateReportsProgress(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	paths, _ := writeSlices(t, dir, []int{1_000_000, 1_000_000})

	recoverySetPath := filepath.Join(dir, "archive.par2")

	var (
		mu        sync.Mutex
		fractions []float64
	)

	err := par2.Generate(t.Context(), recoverySetPath, paths, 30, par2.WithProgress(func(fraction float64) {
		mu.Lock()
		defer mu.Unlock()

		fractions = append(fractions, fraction)
	}))
	require.NoError(t, err)

	// Dropping -qq for progress must not change the output: a valid set is written.
	assert.FileExists(t, recoverySetPath)
	blockCount := par2Verify(t, recoverySetPath)
	assert.LessOrEqual(t, blockCount, maxSourceBlocks)

	require.NotEmpty(t, fractions, "progress callback must fire at least once")

	for index, fraction := range fractions {
		assert.GreaterOrEqual(t, fraction, 0.0, "fraction must not be negative")
		assert.LessOrEqual(t, fraction, 1.0, "fraction must not exceed 1")

		if index > 0 {
			assert.GreaterOrEqual(t, fraction, fractions[index-1],
				"par2 create progress must be non-decreasing")
		}
	}

	assert.GreaterOrEqual(t, fractions[len(fractions)-1], 0.99, "progress must reach completion")
}

// TestMaxOutputBytes proves MaxOutputBytes bounds real par2 output (issue #148).
func TestMaxOutputBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		sliceSizes        []int
		redundancyPercent int
	}{
		{
			name:              "near-100 percent redundancy",
			sliceSizes:        []int{100_000, 100_000, 100_000},
			redundancyPercent: 100,
		},
		{
			// The issue's 550-file block-padding repro, scaled down.
			name:              "many small slices",
			sliceSizes:        repeatSizes(550, 100_000),
			redundancyPercent: 10,
		},
		{
			name:              "few large slices",
			sliceSizes:        []int{2_000_000, 2_000_000, 2_000_000, 2_000_000},
			redundancyPercent: 50,
		},
		{
			name:              "single slice",
			sliceSizes:        []int{2_000_000},
			redundancyPercent: 30,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			paths, _ := writeSlices(t, dir, test.sliceSizes)

			var totalData int64
			for _, size := range test.sliceSizes {
				totalData += int64(size)
			}

			recoverySetPath := filepath.Join(dir, "archive.par2")

			require.NoError(t, par2.Generate(t.Context(), recoverySetPath, paths, test.redundancyPercent))

			matches, globErr := filepath.Glob(filepath.Join(dir, "archive*.par2"))
			require.NoError(t, globErr)
			require.NotEmpty(t, matches)

			var realOutput int64

			for _, path := range matches {
				info, statErr := os.Stat(path)
				require.NoError(t, statErr)

				realOutput += info.Size()
			}

			bound := par2.MaxOutputBytes(totalData, len(test.sliceSizes), test.redundancyPercent)

			assert.GreaterOrEqual(t, bound, realOutput,
				"MaxOutputBytes (%d) must be an upper bound on real par2 output (%d)", bound, realOutput)
		})
	}
}

// TestMaxOutputBytesTightAtScale: at terabyte scale the bound should run at
// about percent × (1 + sliceCount/2,000), so the reservation wastes little tape.
func TestMaxOutputBytesTightAtScale(t *testing.T) {
	t.Parallel()

	const (
		terabyte   = int64(1) << 40
		sliceCount = 200
		percent    = 10
	)

	data := 6 * terabyte
	bound := par2.MaxOutputBytes(data, sliceCount, percent)

	assert.Greater(t, bound, int64(float64(data)*0.10))
	assert.Less(t, bound, int64(float64(data)*0.115))
}

func repeatSizes(count, size int) []int {
	sizes := make([]int, count)
	for index := range sizes {
		sizes[index] = size
	}

	return sizes
}

func sliceSizes(sizes []int) []int64 {
	converted := make([]int64, len(sizes))
	for index, size := range sizes {
		converted[index] = int64(size)
	}

	return converted
}

func writeSlices(t testing.TB, dir string, sizes []int) ([]string, [][]byte) {
	t.Helper()

	paths := make([]string, len(sizes))
	originals := make([][]byte, len(sizes))

	for index, size := range sizes {
		data := make([]byte, size)

		// Distinct per block, so corruption is genuinely detectable.
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

// corrupt inverts the first n bytes, modeling localized media decay.
func corrupt(t *testing.T, path string, n int) {
	t.Helper()

	data := readFile(t, path)
	require.GreaterOrEqual(t, len(data), n, "slice too small to corrupt %d bytes", n)

	for index := 0; index < n; index++ {
		data[index] = ^data[index]
	}

	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// par2Verify returns the source block count par2 reports.
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

func pad(index int) string {
	digits := []byte{
		byte('0' + index/100%10),
		byte('0' + index/10%10),
		byte('0' + index%10),
	}

	return string(digits)
}
