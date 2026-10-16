package par2

import (
	"os"
	"path/filepath"
	"slices"
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
// smallest of /proc/meminfo's MemAvailable and the headroom (limit minus usage)
// of each memory-limited cgroup enclosing it, v2 or v1. When none is readable
// it is MinMemoryLimit, par2's own default.
func MemoryBudget() int64 {
	return MemoryBudgetFrom("/")
}

// MemoryBudgetFrom is MemoryBudget reading proc/ and sys/fs/cgroup/ under root
// in place of the live filesystem.
func MemoryBudgetFrom(root string) int64 {
	available, ok := memAvailable(root)

	if headroom, limited := cgroupHeadroom(root); limited && (!ok || headroom < available) {
		available, ok = headroom, true
	}

	if !ok {
		return MinMemoryLimit
	}

	return BudgetFor(available)
}

func memAvailable(root string) (int64, bool) {
	meminfo, err := os.ReadFile(filepath.Join(root, "proc", "meminfo"))
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

// headroomMin accumulates the smallest headroom seen; negative headroom (usage
// over the limit) counts as zero.
type headroomMin struct {
	value   int64
	limited bool
}

func (h *headroomMin) add(headroom int64, ok bool) {
	if ok && (!h.limited || headroom < h.value) {
		h.value, h.limited = max(headroom, 0), true
	}
}

// cgroupHeadroom returns the smallest limit-minus-usage across this process's
// memory-limited cgroups, if any has a limit.
func cgroupHeadroom(root string) (int64, bool) {
	membership, err := os.ReadFile(filepath.Join(root, "proc", "self", "cgroup"))
	if err != nil {
		return 0, false
	}

	var smallest headroomMin

	// Each line is "hierarchy-ID:controllers:path": cgroup v2's is "0::path";
	// a cgroup v1 memory hierarchy lists "memory" among its controllers.
	for line := range strings.Lines(string(membership)) {
		fields := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(fields) != 3 {
			continue
		}

		switch {
		case fields[0] == "0" && fields[1] == "":
			smallest.add(cgroupV2Headroom(filepath.Join(root, "sys", "fs", "cgroup"), fields[2]))
		case slices.Contains(strings.Split(fields[1], ","), "memory"):
			smallest.add(cgroupV1Headroom(filepath.Join(root, "sys", "fs", "cgroup", "memory"), fields[2]))
		}
	}

	return smallest.value, smallest.limited
}

// cgroupV2Headroom walks from the process's cgroup up to the mount root, since
// an ancestor's memory.max bounds it too. A container in its own cgroup
// namespace sees its cgroup as "/", the mount root.
func cgroupV2Headroom(mount, path string) (int64, bool) {
	var smallest headroomMin

	for dir := filepath.Join(mount, path); ; dir = filepath.Dir(dir) {
		if limit, ok := readCgroupInt(filepath.Join(dir, "memory.max")); ok {
			current, ok := readCgroupInt(filepath.Join(dir, "memory.current"))
			smallest.add(limit-current, ok)
		}

		if dir == mount || !strings.HasPrefix(dir, mount) {
			return smallest.value, smallest.limited
		}
	}
}

// cgroupV1Headroom reads the v1 memory controller at the process's cgroup path,
// else at the mount root, where a container sees its own cgroup bind-mounted.
// An unlimited v1 cgroup reports a huge limit, which never wins the minimum.
func cgroupV1Headroom(mount, path string) (int64, bool) {
	for _, dir := range []string{filepath.Join(mount, path), mount} {
		limit, ok := readCgroupInt(filepath.Join(dir, "memory.limit_in_bytes"))
		if !ok {
			continue
		}

		if usage, ok := readCgroupInt(filepath.Join(dir, "memory.usage_in_bytes")); ok {
			return limit - usage, true
		}
	}

	return 0, false
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
