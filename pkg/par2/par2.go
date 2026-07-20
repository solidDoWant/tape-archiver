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

// maxSourceBlocks is PAR2's hard limit on the number of source blocks in a
// recovery set (SPEC §8). The block size is derived from the data size so the
// real block count stays at or below this; par2 itself rejects any larger
// count.
const maxSourceBlocks = 32768

// blockSizeMultiple is the alignment par2 requires of every block size — a
// block size that is not a multiple of 4 is rejected outright.
const blockSizeMultiple = 4

// ProgressFunc receives par2's completion fraction for the current run, in the
// range [0, 1], as par2 reports it. Generate calls it from the goroutine that
// reads par2's output, so it must return quickly and must not block.
type ProgressFunc func(fraction float64)

// Option configures a Generate call.
type Option func(*options)

type options struct {
	onProgress ProgressFunc
}

// WithProgress reports par2's completion fraction to onProgress as generation
// runs. Supplying it makes Generate run par2 at its default verbosity rather than
// fully silenced, because par2 emits its "Processing: N%" progress only at that
// verbosity; without it Generate stays quiet and reports no progress.
func WithProgress(onProgress ProgressFunc) Option {
	return func(o *options) { o.onProgress = onProgress }
}

// Generate produces a PAR2 recovery set covering dataFiles at the given
// redundancy percentage, writing the recovery files alongside them. It shells
// out to the bundled par2 binary — the exact tool whose binary and source ship
// on the recovery disc (SPEC §8, §10) — so the recovery set is produced by the
// same implementation a future recoverer uses to repair with it.
//
// The recovery set is named after recoverySetPath; every data file must reside
// in the same directory as recoverySetPath. par2 is invoked in that directory
// with bare basenames, so the set references its data files by name and remains
// valid when the slices and recovery files are read back from tape into any
// directory during recovery.
//
// redundancyPercent must be in [1, 100]: the recovery set can repair damage up
// to that fraction of the data. The PAR2 block size is computed from the total
// data size so the source block count stays within PAR2's 32,768-block hard
// limit (SPEC §8). Pass WithProgress to observe par2's completion fraction as it
// runs.
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

	// par2 runs in dir and is handed basenames; enforce the co-location the
	// basename references depend on, and total the data size in one pass.
	var totalSize int64

	names := make([]string, len(dataFiles))

	for index, dataFile := range dataFiles {
		if fileDir := filepath.Dir(dataFile); fileDir != dir {
			return fmt.Errorf("data file %q is not in the recovery set directory %q", dataFile, dir)
		}

		info, err := os.Stat(dataFile)
		if err != nil {
			return fmt.Errorf("stat data file %q: %w", dataFile, err)
		}

		totalSize += info.Size()
		names[index] = filepath.Base(dataFile)
	}

	blockSize := ComputeBlockSize(totalSize, len(dataFiles))

	// -s sets the block size, -r the redundancy percentage, -a the recovery set
	// name. -- ends option parsing so data file names are never mistaken for
	// flags. -qq fully silences par2, including its "Processing: N%" progress; it
	// is dropped when a progress callback is set so that output exists to parse.
	args := []string{"create"}
	if cfg.onProgress == nil {
		args = append(args, "-qq")
	}

	args = append(args,
		"-s"+strconv.FormatInt(blockSize, 10),
		"-r"+strconv.Itoa(redundancyPercent),
		"-a", filepath.Base(recoverySetPath),
		"--",
	)
	args = append(args, names...)

	cmd := exec.CommandContext(ctx, "par2", args...)
	cmd.Dir = dir

	// Merge stdout and stderr into one sink: it retains the non-progress lines for
	// a failure message and parses progress tokens into fraction callbacks. Both
	// streams share the one sink, so os/exec drives it from a single goroutine.
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

// par2OutputSink consumes par2's merged stdout/stderr. par2 reports progress as
// "Processing: N%" tokens separated by carriage returns, interleaved with
// ordinary newline-separated lines. The sink splits the stream on either
// separator: progress tokens are parsed into fraction callbacks (when a callback
// is set) and dropped, and every other line is retained in full for a failure
// message. os/exec drives a single writer goroutine here — stdout and stderr
// share one sink — so Write needs no synchronisation.
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

// flush emits any trailing token par2 left without a final separator; call it
// once par2 has exited and no further writes can arrive.
func (s *par2OutputSink) flush() { s.emit() }

// emit consumes the token accumulated since the last separator: a progress token
// becomes a fraction callback, anything else is retained for the failure message.
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

// parseProgress extracts the completion fraction from a par2 progress token like
// "Processing: 42.3%", returning false for any other token.
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

// ComputeBlockSize returns the PAR2 block size to use for totalSize bytes spread
// across fileCount files.
//
// PAR2 counts source blocks per file — sum(ceil(size_i / blockSize)) — so
// per-file rounding can push the count up to fileCount-1 blocks above
// ceil(totalSize / blockSize). Dividing by the limit less fileCount reserves
// that headroom, keeping the real count within PAR2's 32,768-block limit
// (SPEC §8). The result is rounded up to a multiple of 4 (which par2 requires)
// and never falls below one such block.
//
// It is exported so the Pack phase can reserve a conservative upper bound on the
// recovery-set size (MaxOutputBytes) from the same block size Generate will use.
func ComputeBlockSize(totalSize int64, fileCount int) int64 {
	capacity := int64(maxSourceBlocks - fileCount)
	if capacity < 1 {
		capacity = 1
	}

	blockSize := ceilDiv(totalSize, capacity)

	// Round up to the next multiple of blockSizeMultiple; rounding up only ever
	// lowers the block count, so the limit still holds. Enforce a one-block
	// floor for tiny or empty inputs.
	blockSize = ceilDiv(blockSize, blockSizeMultiple) * blockSizeMultiple
	if blockSize < blockSizeMultiple {
		blockSize = blockSizeMultiple
	}

	return blockSize
}

// MaxOutputBytes returns a conservative upper bound on the total on-disk size of
// the PAR2 recovery set Generate produces for dataBytes spread across fileCount
// files at redundancyPercent. It exists so the Pack phase can reserve honest tape
// space for parity: par2's real output is measurably larger than the naive
// dataBytes×percent/100, so reserving that naive figure lets a fill-to-capacity
// tape pass Pack and then overflow its usable capacity in Verify after the
// multi-hour PAR2 compute (issue #148).
//
// The bound decomposes par2cmdline-turbo's output (validated against the shipped
// 1.4.0 binary by TestMaxOutputBytes) into two parts:
//
//   - Recovery packets: recoveryBlocks packets, each a header plus one block. The
//     source-block count is bounded above by ceil(dataBytes/B)+fileCount-1 (the
//     same per-file rounding ComputeBlockSize budgets for), and the recovery-block
//     count by ceil(sourceBlocks×percent/100)+1.
//   - Critical packets (the main, creator, file-description and input-file
//     slice-checksum packets): par2 replicates the whole critical set into the
//     index file and every recovery volume file, more copies in the larger files.
//     One copy is bounded by 20 bytes per source block (the slice checksums) plus
//     a per-file and fixed allowance; par2's default exponential volume grouping
//     yields at most bitLen(recoveryBlocks)+2 files, each holding at most
//     bitLen(recoveryBlocks)+1 copies, so (bitLen+2)(bitLen+1) copies bound the total.
//
// At LTO (terabyte) scale the recovery packets dominate and the bound is tight
// (~percent% + a fraction of a percent); at tiny sizes the replicated critical
// packets dominate and the bound runs a small multiple above the real output —
// always conservative, which is the safe direction for a capacity reserve
// (principles 1 and 2: recoverability and never overrunning a tape).
//
// fileCount below 1 is treated as 1; redundancyPercent is used as given (callers
// clamp it to par2's [1, 100]).
func MaxOutputBytes(dataBytes int64, fileCount, redundancyPercent int) int64 {
	if fileCount < 1 {
		fileCount = 1
	}

	blockSize := ComputeBlockSize(dataBytes, fileCount)

	// Upper bound on source blocks: per-file rounding adds at most fileCount-1
	// blocks over ceil(dataBytes/blockSize) — the headroom ComputeBlockSize budgets.
	sourceBlocks := ceilDiv(dataBytes, blockSize) + int64(fileCount) - 1
	if sourceBlocks < 1 {
		sourceBlocks = 1
	}

	// Upper bound on recovery blocks: round the percentage up and add one so the
	// bound never dips below par2's chosen count.
	recoveryBlocks := ceilDiv(sourceBlocks*int64(redundancyPercent), 100) + 1

	// Recovery packets: one header (bounded generously at 68 bytes with alignment)
	// plus one block each.
	recoveryBytes := recoveryBlocks * (blockSize + recoveryPacketOverhead)

	// One copy of the critical packet set: the input-file slice checksums dominate
	// at 20 bytes (MD5 + CRC-32) per source block, plus a per-file allowance (file
	// description packets) and a fixed allowance (main and creator packets).
	criticalPerCopy := sliceChecksumBytesPerBlock*sourceBlocks +
		criticalPerFileBytes*int64(fileCount) + criticalFixedBytes

	// par2 replicates the critical set across the index and volume files, more
	// copies in larger files; bound the total copies by (bitLen+2)(bitLen+1).
	fileBits := int64(bits.Len64(uint64(recoveryBlocks)))
	criticalCopies := (fileBits + 2) * (fileBits + 1)

	return recoveryBytes + criticalPerCopy*criticalCopies
}

const (
	// recoveryPacketOverhead bounds a single recovery-slice packet's non-payload
	// bytes (the 64-byte PAR2 packet header plus block alignment slack).
	recoveryPacketOverhead = 68
	// sliceChecksumBytesPerBlock is the per-source-block cost of the input-file
	// slice-checksum packets: a 16-byte MD5 plus a 4-byte CRC-32 per block.
	sliceChecksumBytesPerBlock = 20
	// criticalPerFileBytes bounds the per-file critical-packet cost (the file
	// description packet's header, IDs, hashes, length and file name).
	criticalPerFileBytes = 512
	// criticalFixedBytes bounds the file-count-independent critical packets (the
	// main packet and the creator packet) in one copy of the critical set.
	criticalFixedBytes = 4096
)

// ceilDiv returns ceil(numerator / denominator) for non-negative numerator and
// positive denominator.
func ceilDiv(numerator, denominator int64) int64 {
	return (numerator + denominator - 1) / denominator
}
