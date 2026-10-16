package par2_test

import (
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

const (
	mebibyte = 1 << 20
	gibibyte = 1 << 30
)

// prodSlices is the archive from issue #433: 117.5 GB in 110 slices of 1 GiB.
var prodSlices = append(repeatSizes64(109, gibibyte), 117_500_000_000-109*gibibyte)

func TestPlanMemory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		fileSizes         []int64
		redundancyPercent int
		budget            int64
		want              par2.MemoryPlan
	}{
		{
			// The issue-#433 regime: par2's 128 MiB default meant ~100 passes of
			// ~600 KB strided reads.
			name:              "prod archive at par2's old default",
			fileSizes:         prodSlices,
			redundancyPercent: 10,
			budget:            par2.MinMemoryLimit,
			want: par2.MemoryPlan{
				BlockSize: 62_169_316, SourceBlocks: 1970, RecoveryBlocks: 197,
				Limit: 128 * mebibyte, ChunkSize: 601_872, Passes: 104, InputReads: 2,
			},
		},
		{
			// Blocks above par2's 32 MiB chunk cap: two passes of 32 MiB reads.
			name:              "prod archive within the max budget",
			fileSizes:         prodSlices,
			redundancyPercent: 10,
			budget:            par2.MaxMemoryBudget,
			want: par2.MemoryPlan{
				BlockSize: 62_169_316, SourceBlocks: 1970, RecoveryBlocks: 197,
				Limit: 7136 * mebibyte, ChunkSize: 32 * mebibyte, Passes: 2, InputReads: 2,
			},
		},
		{
			// Fits: one pass that also hashes, so the input is read once.
			name:              "small archive single pass",
			fileSizes:         repeatSizes64(4, gibibyte),
			redundancyPercent: 10,
			budget:            par2.MaxMemoryBudget,
			want: par2.MemoryPlan{
				BlockSize: 2_151_788, SourceBlocks: 1996, RecoveryBlocks: 200,
				Limit: 464 * mebibyte, ChunkSize: 2_151_788, Passes: 1, InputReads: 1,
			},
		},
		{
			name:              "budget below the single-pass need",
			fileSizes:         repeatSizes64(4, gibibyte),
			redundancyPercent: 10,
			budget:            256*mebibyte + 12345,
			want: par2.MemoryPlan{
				BlockSize: 2_151_788, SourceBlocks: 1996, RecoveryBlocks: 200,
				Limit: 256 * mebibyte, ChunkSize: 1_187_764, Passes: 2, InputReads: 2,
			},
		},
		{
			name:              "budget below one MiB",
			fileSizes:         repeatSizes64(4, gibibyte),
			redundancyPercent: 10,
			budget:            0,
			want: par2.MemoryPlan{
				BlockSize: 2_151_788, SourceBlocks: 1996, RecoveryBlocks: 200,
				Limit: mebibyte, ChunkSize: 4636, Passes: 465, InputReads: 2,
			},
		},
		{
			// par2 never asks for fewer than one recovery block.
			name:              "tiny archive rounds up to one recovery block",
			fileSizes:         []int64{1000},
			redundancyPercent: 1,
			budget:            par2.MaxMemoryBudget,
			want: par2.MemoryPlan{
				BlockSize: 4, SourceBlocks: 250, RecoveryBlocks: 3,
				Limit: mebibyte, ChunkSize: 4, Passes: 1, InputReads: 1,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			plan := par2.PlanMemory(test.fileSizes, test.redundancyPercent, test.budget)

			assert.Equal(t, test.want, plan)
			assert.Zero(t, plan.Limit%mebibyte, "-m takes whole MiB")
			assert.LessOrEqual(t, plan.Limit, max(test.budget, mebibyte), "the limit must respect the budget")
		})
	}
}

var (
	chunkSizePattern      = regexp.MustCompile(`Process chunk size: (\d+)`)
	sourceBlocksPattern   = regexp.MustCompile(`Source block count: (\d+)`)
	recoveryBlocksPattern = regexp.MustCompile(`Recovery block count: (\d+)`)
)

// TestPlanMemoryMatchesPar2 proves PlanMemory models the bundled par2: given
// the plan's block size and limit, par2 reports the same counts and chunk size.
func TestPlanMemoryMatchesPar2(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		sliceSizes        []int
		redundancyPercent int
		budget            int64
		wantPasses        int64
	}{
		{
			name:              "single pass",
			sliceSizes:        []int{1_000_000, 1_000_000, 500_000},
			redundancyPercent: 10,
			budget:            par2.MaxMemoryBudget,
			wantPasses:        1,
		},
		{
			name:              "multi-pass",
			sliceSizes:        []int{1_000_000, 1_000_000, 1_000_000, 1_000_000},
			redundancyPercent: 30,
			budget:            mebibyte,
			wantPasses:        2,
		},
		{
			name:              "many passes",
			sliceSizes:        []int{4_000_000, 4_000_000, 4_000_000},
			redundancyPercent: 20,
			budget:            mebibyte,
			wantPasses:        3,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			paths, _ := writeSlices(t, dir, test.sliceSizes)

			plan := par2.PlanMemory(sliceSizes(test.sliceSizes), test.redundancyPercent, test.budget)
			require.Equal(t, test.wantPasses, plan.Passes)

			args := []string{
				"create", "-vv",
				"-s" + strconv.FormatInt(plan.BlockSize, 10),
				"-m" + strconv.FormatInt(plan.Limit/mebibyte, 10),
				"-r" + strconv.Itoa(test.redundancyPercent),
				"-a", "archive.par2", "--",
			}
			for _, path := range paths {
				args = append(args, filepath.Base(path))
			}

			cmd := exec.CommandContext(t.Context(), "par2", args...)
			cmd.Dir = dir

			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "par2 create failed: %s", output)

			assert.Equal(t, plan.ChunkSize, parseInt(t, chunkSizePattern, output))
			assert.Equal(t, plan.SourceBlocks, parseInt(t, sourceBlocksPattern, output))
			assert.Equal(t, plan.RecoveryBlocks, parseInt(t, recoveryBlocksPattern, output))
		})
	}
}

// TestGenerateOutputIndependentOfMemory proves the memory limit changes only
// how par2 computes the set: multi-pass and single-pass output is identical.
func TestGenerateOutputIndependentOfMemory(t *testing.T) {
	t.Parallel()

	sizes := []int{1_000_000, 1_000_000, 1_000_000, 1_000_000}

	generate := func(opts ...par2.Option) map[string][]byte {
		dir := t.TempDir()
		paths, _ := writeSlices(t, dir, sizes)

		require.NoError(t, par2.Generate(t.Context(), filepath.Join(dir, "archive.par2"), paths, 30, opts...))

		outputs, err := filepath.Glob(filepath.Join(dir, "archive*.par2"))
		require.NoError(t, err)
		require.NotEmpty(t, outputs)

		files := make(map[string][]byte, len(outputs))
		for _, output := range outputs {
			files[filepath.Base(output)] = readFile(t, output)
		}

		return files
	}

	require.Greater(t, par2.PlanMemory(sliceSizes(sizes), 30, mebibyte).Passes, int64(1))
	require.Equal(t, int64(1), par2.PlanMemory(sliceSizes(sizes), 30, par2.MaxMemoryBudget).Passes)

	assert.Equal(t, generate(par2.WithMemoryBudget(par2.MaxMemoryBudget)), generate(par2.WithMemoryBudget(mebibyte)))
}

func TestBudgetFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		available int64
		want      int64
	}{
		{name: "unknown or none", available: 0, want: par2.MinMemoryLimit},
		{name: "below the floor", available: 200 * mebibyte, want: par2.MinMemoryLimit},
		{name: "half of available", available: 6 * gibibyte, want: 3 * gibibyte},
		{name: "capped", available: 62 * gibibyte, want: par2.MaxMemoryBudget},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, par2.BudgetFor(test.available))
		})
	}
}

func TestMemoryBudget(t *testing.T) {
	t.Parallel()

	budget := par2.MemoryBudget()

	assert.GreaterOrEqual(t, budget, int64(par2.MinMemoryLimit))
	assert.LessOrEqual(t, budget, int64(par2.MaxMemoryBudget))
}

// meminfo is a /proc/meminfo with availableGiB GiB available.
func meminfo(availableGiB int64) string {
	return "MemTotal:       65000000 kB\nMemFree:         1000000 kB\nMemAvailable:   " +
		strconv.FormatInt(availableGiB<<20, 10) + " kB\n"
}

func TestMemoryBudgetFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files map[string]string
		want  int64
	}{
		{
			name: "nothing readable falls back to par2's default",
			want: par2.MinMemoryLimit,
		},
		{
			name:  "meminfo without MemAvailable",
			files: map[string]string{"proc/meminfo": "MemTotal: 65000000 kB\n"},
			want:  par2.MinMemoryLimit,
		},
		{
			name:  "half of MemAvailable",
			files: map[string]string{"proc/meminfo": meminfo(10)},
			want:  5 * gibibyte,
		},
		{
			name:  "MemAvailable capped",
			files: map[string]string{"proc/meminfo": meminfo(40)},
			want:  par2.MaxMemoryBudget,
		},
		{
			name: "unlimited cgroup v2 container",
			files: map[string]string{
				"proc/meminfo":                 meminfo(40),
				"proc/self/cgroup":             "0::/\n",
				"sys/fs/cgroup/memory.max":     "max\n",
				"sys/fs/cgroup/memory.current": "1000\n",
			},
			want: par2.MaxMemoryBudget,
		},
		{
			name: "cgroup v2 container limit",
			files: map[string]string{
				"proc/meminfo":                 meminfo(40),
				"proc/self/cgroup":             "0::/\n",
				"sys/fs/cgroup/memory.max":     strconv.Itoa(8 * gibibyte),
				"sys/fs/cgroup/memory.current": strconv.Itoa(2 * gibibyte),
			},
			want: 3 * gibibyte,
		},
		{
			name: "cgroup v2 ancestor limit bounds an unlimited leaf",
			files: map[string]string{
				"proc/meminfo":     meminfo(40),
				"proc/self/cgroup": "0::/system.slice/worker.service\n",
				"sys/fs/cgroup/system.slice/worker.service/memory.max":     "max\n",
				"sys/fs/cgroup/system.slice/worker.service/memory.current": "1000\n",
				"sys/fs/cgroup/system.slice/memory.max":                    strconv.Itoa(6 * gibibyte),
				"sys/fs/cgroup/system.slice/memory.current":                strconv.Itoa(2 * gibibyte),
			},
			want: 2 * gibibyte,
		},
		{
			name: "cgroup v2 without meminfo",
			files: map[string]string{
				"proc/self/cgroup":             "0::/\n",
				"sys/fs/cgroup/memory.max":     strconv.Itoa(4 * gibibyte),
				"sys/fs/cgroup/memory.current": "0\n",
			},
			want: 2 * gibibyte,
		},
		{
			name: "cgroup over its limit floors at par2's default",
			files: map[string]string{
				"proc/meminfo":                 meminfo(40),
				"proc/self/cgroup":             "0::/\n",
				"sys/fs/cgroup/memory.max":     strconv.Itoa(gibibyte),
				"sys/fs/cgroup/memory.current": strconv.Itoa(2 * gibibyte),
			},
			want: par2.MinMemoryLimit,
		},
		{
			name: "cgroup v1 container limit",
			files: map[string]string{
				"proc/meminfo":     meminfo(40),
				"proc/self/cgroup": "12:cpu,cpuacct:/docker/abc\n4:memory:/docker/abc\n1:name=systemd:/docker/abc\n",
				"sys/fs/cgroup/memory/memory.limit_in_bytes": strconv.Itoa(4 * gibibyte),
				"sys/fs/cgroup/memory/memory.usage_in_bytes": "0\n",
			},
			want: 2 * gibibyte,
		},
		{
			name: "cgroup v1 unlimited",
			files: map[string]string{
				"proc/meminfo":     meminfo(10),
				"proc/self/cgroup": "4:memory:/\n",
				"sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n",
				"sys/fs/cgroup/memory/memory.usage_in_bytes": strconv.Itoa(gibibyte),
			},
			want: 5 * gibibyte,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()

			for name, content := range test.files {
				path := filepath.Join(root, name)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			}

			assert.Equal(t, test.want, par2.MemoryBudgetFrom(root))
		})
	}
}

// TestGeneratePassesPlannedMemoryLimit proves Generate hands par2 the planned
// -m rather than leaving par2 at its 128 MiB default. A wrapper on PATH records
// the arguments and runs the real par2.
func TestGeneratePassesPlannedMemoryLimit(t *testing.T) {
	realPar2, err := exec.LookPath("par2")
	require.NoError(t, err)

	binDir := t.TempDir()
	argsLog := filepath.Join(t.TempDir(), "args")
	wrapper := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsLog + "'\nexec '" + realPar2 + "' \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "par2"), []byte(wrapper), 0o700)) //nolint:gosec // test wrapper must be executable

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	sizes := []int{1_000_000, 1_000_000, 1_000_000, 1_000_000}
	budget := int64(300 * mebibyte)

	dir := t.TempDir()
	paths, _ := writeSlices(t, dir, sizes)

	require.NoError(t, par2.Generate(t.Context(), filepath.Join(dir, "archive.par2"), paths, 30, par2.WithMemoryBudget(budget)))

	plan := par2.PlanMemory(sliceSizes(sizes), 30, budget)
	assert.Contains(t, strings.Split(string(readFile(t, argsLog)), "\n"), "-m"+strconv.FormatInt(plan.Limit/mebibyte, 10))
}

// BenchmarkGenerate compares par2's old 128 MiB default against the planned
// limit. Set TAPE_ARCHIVER_BENCH_PAR2_DIR to a directory of slices (e.g. on a
// spinning-disk pool; bench-433*.par2 is written there and removed) and TAPE_ARCHIVER_BENCH_DROP_CACHES=1 (needs passwordless
// sudo) to measure cold reads; the default temp slices are page-cache hot.
func BenchmarkGenerate(b *testing.B) {
	sources := benchmarkSlices(b)

	var totalSize int64

	for _, source := range sources {
		info, err := os.Stat(source)
		require.NoError(b, err)

		totalSize += info.Size()
	}

	for _, config := range []struct {
		name string
		opts []par2.Option
	}{
		{name: "par2 default memory", opts: []par2.Option{par2.WithMemoryBudget(par2.MinMemoryLimit)}},
		{name: "planned memory"},
	} {
		b.Run(config.name, func(b *testing.B) {
			b.SetBytes(totalSize)

			// par2 skips symlinks, so the set is written beside the slices and removed.
			recoverySetPath := filepath.Join(filepath.Dir(sources[0]), "bench-433.par2")

			removeSet := func() {
				outputs, err := filepath.Glob(filepath.Join(filepath.Dir(sources[0]), "bench-433*.par2"))
				require.NoError(b, err)

				for _, output := range outputs {
					require.NoError(b, os.Remove(output))
				}
			}

			b.Cleanup(removeSet)

			for b.Loop() {
				b.StopTimer()
				removeSet()
				dropCaches(b)
				b.StartTimer()

				require.NoError(b, par2.Generate(b.Context(), recoverySetPath, sources, 10, config.opts...))
			}
		})
	}
}

func benchmarkSlices(b *testing.B) []string {
	b.Helper()

	if dir := os.Getenv("TAPE_ARCHIVER_BENCH_PAR2_DIR"); dir != "" {
		entries, err := os.ReadDir(dir)
		require.NoError(b, err)

		var paths []string

		for _, entry := range entries {
			if entry.Type().IsRegular() && filepath.Ext(entry.Name()) != ".par2" {
				paths = append(paths, filepath.Join(dir, entry.Name()))
			}
		}

		require.NotEmpty(b, paths, "no slices in %s", dir)

		return paths
	}

	paths, _ := writeSlices(b, b.TempDir(), repeatSizes(4, 64*mebibyte))

	return paths
}

func dropCaches(b *testing.B) {
	b.Helper()

	if os.Getenv("TAPE_ARCHIVER_BENCH_DROP_CACHES") != "1" {
		return
	}

	output, err := exec.CommandContext(b.Context(), "sudo", "-n", "sh", "-c", "sync && echo 3 > /proc/sys/vm/drop_caches").CombinedOutput()
	require.NoError(b, err, "drop caches: %s", output)
}

func parseInt(t *testing.T, pattern *regexp.Regexp, output []byte) int64 {
	t.Helper()

	match := pattern.FindSubmatch(output)
	require.NotNil(t, match, "par2 output missing %s: %s", pattern, output)

	value, err := strconv.ParseInt(string(match[1]), 10, 64)
	require.NoError(t, err)

	return value
}

func repeatSizes64(count int, size int64) []int64 {
	sizes := make([]int64, count)
	for index := range sizes {
		sizes[index] = size
	}

	return sizes
}
