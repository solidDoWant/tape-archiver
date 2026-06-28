// Package par2 generates per-archive PAR2 recovery sets by shelling out to the
// bundled par2cmdline-turbo binary (SPEC §8).
package par2

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// maxRecoveryBlocks is PAR2's hard limit on the number of source blocks in a
// recovery set (SPEC §8). The block size is derived from the data size so the
// real block count stays at or below this; par2 itself rejects any larger
// count.
const maxRecoveryBlocks = 32768

// blockSizeMultiple is the alignment par2 requires of every block size — a
// block size that is not a multiple of 4 is rejected outright.
const blockSizeMultiple = 4

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
// limit (SPEC §8).
func Generate(ctx context.Context, recoverySetPath string, dataFiles []string, redundancyPercent int) error {
	if redundancyPercent < 1 || redundancyPercent > 100 {
		return fmt.Errorf("redundancy percentage must be in [1, 100], got %d", redundancyPercent)
	}

	if len(dataFiles) == 0 {
		return fmt.Errorf("no data files given")
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

	blockSize := computeBlockSize(totalSize, len(dataFiles))

	// -qq fully silences par2's progress output; -s sets the block size, -r the
	// redundancy percentage, -a the recovery set name. -- ends option parsing so
	// data file names are never mistaken for flags.
	args := []string{
		"create", "-qq",
		"-s" + strconv.FormatInt(blockSize, 10),
		"-r" + strconv.Itoa(redundancyPercent),
		"-a", filepath.Base(recoverySetPath),
		"--",
	}
	args = append(args, names...)

	cmd := exec.CommandContext(ctx, "par2", args...)
	cmd.Dir = dir

	var output strings.Builder

	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(output.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return fmt.Errorf("%s: %w", cmd, err)
	}

	return nil
}

// computeBlockSize returns the PAR2 block size to use for totalSize bytes spread
// across fileCount files.
//
// PAR2 counts source blocks per file — sum(ceil(size_i / blockSize)) — so
// per-file rounding can push the count up to fileCount-1 blocks above
// ceil(totalSize / blockSize). Dividing by the limit less fileCount reserves
// that headroom, keeping the real count within PAR2's 32,768-block limit
// (SPEC §8). The result is rounded up to a multiple of 4 (which par2 requires)
// and never falls below one such block.
func computeBlockSize(totalSize int64, fileCount int) int64 {
	capacity := int64(maxRecoveryBlocks - fileCount)
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

// ceilDiv returns ceil(numerator / denominator) for non-negative numerator and
// positive denominator.
func ceilDiv(numerator, denominator int64) int64 {
	return (numerator + denominator - 1) / denominator
}
