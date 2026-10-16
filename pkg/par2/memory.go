package par2

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	mebibyte = 1 << 20

	// maxChunkBytes mirrors par2cmdline-turbo's compiled-in MAX_CHUNK_SIZE: it
	// never processes more than 32 MiB of each block per pass.
	maxChunkBytes = 32 << 20

	// Beside the recovery blocks, par2cmdline-turbo holds 2 transfer buffers and
	// up to 24 ParPar staging buffers, each one chunk long (CalculateProcessBlockSize).
	transferBuffers   = 2
	maxStagingBuffers = 24

	// MinMemoryLimit is par2's effective default -m in the bundled build: its
	// physical-memory probe compiles out (commandline.cpp tests _SC_PHYS_PAGES
	// without including unistd.h), so "half of physical memory" is really 128 MiB.
	MinMemoryLimit = 128 << 20

	// MaxMemoryBudget bounds par2's working memory on the shared storage host.
	// It holds 32 MiB chunks for ~230 recovery blocks: an archive of any size at
	// the default ~2,000 source blocks and 10% redundancy streams in 32 MiB reads.
	MaxMemoryBudget = 8 << 30
)

// MemoryPlan is how par2 processes a recovery set under a memory limit.
//
// par2 holds every recovery block in memory, so when they do not fit it walks
// the input once per ChunkSize slice of the block size, reading ChunkSize
// bytes from every source block in turn. Small chunks turn the input into
// strided reads that seek on a spinning pool: at par2's 128 MiB default a
// 117 GB archive took ~100 passes of ~600 KB reads (issue #433).
type MemoryPlan struct {
	// BlockSize is the PAR2 block size (ComputeBlockSize).
	BlockSize int64
	// SourceBlocks and RecoveryBlocks are the counts par2 derives.
	SourceBlocks   int64
	RecoveryBlocks int64
	// Limit is the -m value in bytes, a whole number of MiB.
	Limit int64
	// ChunkSize is how much of each source block par2 reads per pass.
	ChunkSize int64
	// Passes is how many strided passes par2 makes over the input.
	Passes int64
	// InputReads is how many times par2 reads each input byte: 1 when it hashes
	// during a single pass, else 2 (a sequential hash pass, then the passes).
	InputReads int
}

// PlanMemory sizes par2's memory limit for files of fileSizes at
// redundancyPercent: just enough for the fewest passes (one, when the block
// fits in a chunk), capped at budget.
func PlanMemory(fileSizes []int64, redundancyPercent int, budget int64) MemoryPlan {
	var total int64
	for _, size := range fileSizes {
		total += size
	}

	blockSize := ComputeBlockSize(total, len(fileSizes))

	var sourceBlocks int64
	for _, size := range fileSizes {
		sourceBlocks += ceilDiv(size, blockSize)
	}

	// par2's own rounding for -r (commandline.cpp ComputeRecoveryBlockCount).
	recoveryBlocks := max((sourceBlocks*int64(redundancyPercent)+50)/100, 1)

	buffers := recoveryBlocks + transferBuffers + min(maxStagingBuffers, sourceBlocks+1)

	limit := ceilDiv(min(blockSize, maxChunkBytes)*buffers, mebibyte) * mebibyte
	limit = min(limit, budget/mebibyte*mebibyte)
	limit = max(limit, mebibyte) // -m takes whole MiB, at least one

	// Mirrors par2cmdline-turbo's CalculateProcessBlockSize.
	chunkSize := blockSize
	if blockSize*buffers > limit {
		chunkSize = limit / buffers &^ 3
	}

	chunkSize = min(chunkSize, maxChunkBytes)

	inputReads := 2
	if chunkSize == blockSize {
		inputReads = 1
	}

	return MemoryPlan{
		BlockSize:      blockSize,
		SourceBlocks:   sourceBlocks,
		RecoveryBlocks: recoveryBlocks,
		Limit:          limit,
		ChunkSize:      chunkSize,
		Passes:         ceilDiv(blockSize, chunkSize),
		InputReads:     inputReads,
	}
}

// BudgetFor returns the memory budget for par2 given available bytes: half of
// it, so the host and the rest of the worker keep the other half, clamped to
// [MinMemoryLimit, MaxMemoryBudget].
func BudgetFor(available int64) int64 {
	return min(max(available/2, MinMemoryLimit), MaxMemoryBudget)
}

// MemoryBudget is BudgetFor the memory available to this process now: the
// smaller of /proc/meminfo's MemAvailable and the cgroup v2 limit's headroom.
// When neither is readable it is MinMemoryLimit, par2's own default.
func MemoryBudget() int64 {
	available, ok := memAvailable()

	if headroom, limited := cgroupHeadroom(); limited && (!ok || headroom < available) {
		available, ok = headroom, true
	}

	if !ok {
		return MinMemoryLimit
	}

	return BudgetFor(available)
}

func memAvailable() (int64, bool) {
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}

	for line := range strings.Lines(string(meminfo)) {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemAvailable:" && fields[2] == "kB" {
			kib, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, false
			}

			return kib << 10, true
		}
	}

	return 0, false
}

// cgroupHeadroom returns memory.max minus memory.current for this process's
// cgroup v2, if it has a limit.
func cgroupHeadroom() (int64, bool) {
	membership, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0, false
	}

	// cgroup v2 has a single "0::/path" line.
	path, found := bytes.CutPrefix(bytes.TrimSpace(membership), []byte("0::"))
	if !found {
		return 0, false
	}

	dir := filepath.Join("/sys/fs/cgroup", string(path))

	limit, ok := readCgroupInt(filepath.Join(dir, "memory.max"))
	if !ok {
		return 0, false
	}

	current, ok := readCgroupInt(filepath.Join(dir, "memory.current"))
	if !ok {
		return 0, false
	}

	return max(limit-current, 0), true
}

// readCgroupInt parses a cgroup integer file; "max" (no limit) reports false.
func readCgroupInt(path string) (int64, bool) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	value, err := strconv.ParseInt(strings.TrimSpace(string(content)), 10, 64)
	if err != nil {
		return 0, false
	}

	return value, true
}
