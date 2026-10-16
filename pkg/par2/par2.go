// Package par2 generates per-archive PAR2 recovery sets by shelling out to the
// bundled par2cmdline-turbo binary (SPEC §8).
package par2

import (
	"context"
	"fmt"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// maxSourceBlocks is PAR2's hard limit on source blocks per recovery set.
const maxSourceBlocks = 32768

// targetSourceBlocks is the source-block count ComputeBlockSize aims for.
// Compute scales linearly with block count, and maxing it at the format limit
// made terabyte archives take weeks; contiguous-damage tolerance (how tape
// actually fails) is independent of block count (SPEC §8). 2,000 is par2's own
// default and sits where the compute curve flattens.
const targetSourceBlocks = 2000

// blockSizeMultiple: par2 rejects block sizes that are not a multiple of 4.
const blockSizeMultiple = 4

// ProgressFunc receives par2's completion fraction in [0, 1]. It is called from
// the output-reading goroutine and must not block.
type ProgressFunc func(fraction float64)

// Option configures a Generate call.
type Option func(*options)

type options struct {
	onProgress   ProgressFunc
	memoryBudget int64
}

// WithProgress reports par2's completion fraction to onProgress. par2 then runs
// at default verbosity, since it only prints progress there.
func WithProgress(onProgress ProgressFunc) Option {
	return func(o *options) { o.onProgress = onProgress }
}

// WithMemoryBudget caps par2's working memory at budget bytes in place of the
// host-derived MemoryBudget.
func WithMemoryBudget(budget int64) Option {
	return func(o *options) { o.memoryBudget = budget }
}

// Generate produces a PAR2 recovery set named after recoverySetPath covering
// dataFiles at redundancyPercent (1-100), using the same par2 binary that ships
// on the recovery disc (SPEC §8, §10).
//
// Every data file must share recoverySetPath's directory: par2 is given bare
// basenames so the set stays valid when restored into any directory.
//
// par2's memory limit is set explicitly (PlanMemory) so the input streams in
// as few, large reads as the memory budget allows. It changes only how par2
// computes the set, never the PAR2 2.0 output format.
func Generate(ctx context.Context, recoverySetPath string, dataFiles []string, redundancyPercent int, opts ...Option) error {
	if redundancyPercent < 1 || redundancyPercent > 100 {
		return fmt.Errorf("redundancy percentage must be in [1, 100], got %d", redundancyPercent)
	}

	if len(dataFiles) == 0 {
		return fmt.Errorf("no data files given")
	}

	var cfg options
	for _, opt := range opts {
		opt(&cfg)
	}

	dir := filepath.Dir(recoverySetPath)

	names := make([]string, len(dataFiles))
	sizes := make([]int64, len(dataFiles))

	for index, dataFile := range dataFiles {
		if fileDir := filepath.Dir(dataFile); fileDir != dir {
			return fmt.Errorf("data file %q is not in the recovery set directory %q", dataFile, dir)
		}

		info, err := os.Stat(dataFile)
		if err != nil {
			return fmt.Errorf("stat data file %q: %w", dataFile, err)
		}

		sizes[index] = info.Size()
		names[index] = filepath.Base(dataFile)
	}

	budget := cfg.memoryBudget
	if budget <= 0 {
		budget = MemoryBudget()
	}

	plan := PlanMemory(sizes, redundancyPercent, budget)

	args := []string{"create"}
	if cfg.onProgress == nil {
		args = append(args, "-qq")
	}

	// -t and -T stay at par2's defaults: every core for the GF16 compute, and two
	// files hashed at once in the separate hash pass.
	args = append(args,
		"-s"+strconv.FormatInt(plan.BlockSize, 10),
		"-m"+strconv.FormatInt(plan.Limit/mebibyte, 10),
		"-r"+strconv.Itoa(redundancyPercent),
		"-a", filepath.Base(recoverySetPath),
		"--",
	)
	args = append(args, names...)

	cmd := exec.CommandContext(ctx, "par2", args...)
	cmd.Dir = dir

	// One sink for both streams, so os/exec drives it from a single goroutine.
	var output strings.Builder

	sink := &par2OutputSink{full: &output, onProgress: cfg.onProgress}
	cmd.Stdout = sink
	cmd.Stderr = sink

	err := cmd.Run()

	sink.flush()

	if err != nil {
		if msg := strings.TrimSpace(output.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return fmt.Errorf("%s: %w", cmd, err)
	}

	return nil
}

// par2OutputSink splits par2's output on '\r' or '\n' (progress tokens are
// '\r'-separated), turning progress into callbacks and keeping other lines for
// error messages.
type par2OutputSink struct {
	full       *strings.Builder
	onProgress ProgressFunc
	partial    []byte // bytes since the last separator, possibly split across writes
}

func (s *par2OutputSink) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\r' || b == '\n' {
			s.emit()

			continue
		}

		s.partial = append(s.partial, b)
	}

	return len(p), nil
}

func (s *par2OutputSink) flush() { s.emit() }

func (s *par2OutputSink) emit() {
	token := strings.TrimSpace(string(s.partial))
	s.partial = s.partial[:0]

	if token == "" {
		return
	}

	if s.onProgress != nil {
		if fraction, ok := parseProgress(token); ok {
			s.onProgress(fraction)

			return
		}
	}

	s.full.WriteString(token)
	s.full.WriteByte('\n')
}

// parseProgress parses a token like "Processing: 42.3%".
func parseProgress(token string) (float64, bool) {
	const prefix = "Processing:"

	if !strings.HasPrefix(token, prefix) || !strings.HasSuffix(token, "%") {
		return 0, false
	}

	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(token, prefix), "%"))

	percent, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}

	return percent / 100, true
}

// ComputeBlockSize returns the PAR2 block size for totalSize bytes across
// fileCount files, targeting about targetSourceBlocks blocks.
//
// Blocks never span files, so per-file rounding adds up to fileCount-1 blocks;
// with few files that headroom is subtracted from the target, and with many
// files the size aims for one block per file. The result never violates the
// 32,768-block limit and is a multiple of 4.
func ComputeBlockSize(totalSize int64, fileCount int) int64 {
	var blockSize int64
	if fileCount < targetSourceBlocks/2 {
		blockSize = ceilDiv(totalSize, int64(targetSourceBlocks-fileCount))
	} else {
		blockSize = ceilDiv(totalSize, int64(fileCount-1))
	}

	capacity := int64(maxSourceBlocks - fileCount)
	if capacity < 1 {
		capacity = 1
	}

	if minimum := ceilDiv(totalSize, capacity); blockSize < minimum {
		blockSize = minimum
	}

	// Rounding up only lowers the block count, so the limit still holds.
	blockSize = ceilDiv(blockSize, blockSizeMultiple) * blockSizeMultiple
	if blockSize < blockSizeMultiple {
		blockSize = blockSizeMultiple
	}

	return blockSize
}

// MaxOutputBytes returns a conservative upper bound on the on-disk size of the
// recovery set Generate produces, so Pack can reserve honest tape space: par2's
// output exceeds the naive dataBytes×percent/100, and reserving that let a full
// tape overflow in Verify after hours of PAR2 compute (issue #148).
//
// The bound is recovery packets (header plus block each) plus the critical
// packet set, which par2 replicates up to (bitLen+2)(bitLen+1) times across its
// exponentially sized volume files. TestMaxOutputBytes validates it against the
// shipped par2cmdline-turbo.
func MaxOutputBytes(dataBytes int64, fileCount, redundancyPercent int) int64 {
	if fileCount < 1 {
		fileCount = 1
	}

	blockSize := ComputeBlockSize(dataBytes, fileCount)

	// Per-file rounding adds at most fileCount-1 blocks.
	sourceBlocks := ceilDiv(dataBytes, blockSize) + int64(fileCount) - 1
	if sourceBlocks < 1 {
		sourceBlocks = 1
	}

	// +1 so the bound never dips below par2's own rounding.
	recoveryBlocks := ceilDiv(sourceBlocks*int64(redundancyPercent), 100) + 1

	recoveryBytes := recoveryBlocks * (blockSize + recoveryPacketOverhead)

	criticalPerCopy := sliceChecksumBytesPerBlock*sourceBlocks +
		criticalPerFileBytes*int64(fileCount) + criticalFixedBytes

	fileBits := int64(bits.Len64(uint64(recoveryBlocks)))
	criticalCopies := (fileBits + 2) * (fileBits + 1)

	return recoveryBytes + criticalPerCopy*criticalCopies
}

const (
	// 64-byte PAR2 packet header plus alignment slack.
	recoveryPacketOverhead = 68
	// 16-byte MD5 plus 4-byte CRC-32 per source block.
	sliceChecksumBytesPerBlock = 20
	// File description packet allowance.
	criticalPerFileBytes = 512
	// Main and creator packets.
	criticalFixedBytes = 4096
)

func ceilDiv(numerator, denominator int64) int64 {
	return (numerator + denominator - 1) / denominator
}
