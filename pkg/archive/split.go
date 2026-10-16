package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// Slice is one file written by Split.
type Slice struct {
	Path string
	// SHA256 is the lowercase hex digest, computed from the bytes as they were
	// written so staging needs no second read pass (#448).
	SHA256    string
	SizeBytes int64
}

// Split writes src to dir as sliceSize-byte files "baseName.NNN" and returns
// them in order. All slices share one zero-padded width (at least three
// digits) so lexical order equals numeric order and the recovery procedure's
// sorted reassembly is correct. Slicing bounds a damaged tape region's
// blast radius to one slice (SPEC §8).
func Split(ctx context.Context, src io.Reader, sliceSize int64, dir, baseName string) ([]Slice, error) {
	if sliceSize <= 0 {
		return nil, fmt.Errorf("slice size must be positive, got %d", sliceSize)
	}

	var slices []Slice

	for index := 0; ; index++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		path := filepath.Join(dir, fmt.Sprintf("%s.%03d", baseName, index))

		written, digest, err := writeSlice(ctx, path, src, sliceSize)
		if err != nil {
			return nil, err
		}

		if written == 0 {
			// EOF: drop the empty trailing slice.
			if err := os.Remove(path); err != nil {
				return nil, fmt.Errorf("remove empty trailing slice %s: %w", path, err)
			}

			break
		}

		slices = append(slices, Slice{Path: path, SHA256: digest, SizeBytes: written})

		if written < sliceSize {
			break
		}
	}

	return normalizeWidth(slices, dir, baseName)
}

// normalizeWidth renames slices to a uniform pad once the count exceeds 1000.
func normalizeWidth(slices []Slice, dir, baseName string) ([]Slice, error) {
	if len(slices) == 0 {
		return slices, nil
	}

	width := len(strconv.Itoa(len(slices) - 1))
	if width <= 3 {
		return slices, nil
	}

	for index := range slices {
		oldPath := slices[index].Path

		newPath := filepath.Join(dir, fmt.Sprintf("%s.%0*d", baseName, width, index))
		if newPath == oldPath {
			continue
		}

		if err := os.Rename(oldPath, newPath); err != nil {
			return nil, fmt.Errorf("rename slice %s to uniform width %s: %w", oldPath, newPath, err)
		}

		slices[index].Path = newPath
	}

	return slices, nil
}

// writeSlice copies up to sliceSize bytes, hashing them as they are written,
// and returns the count and hex digest; a short count means EOF.
func writeSlice(ctx context.Context, path string, src io.Reader, sliceSize int64) (int64, string, error) {
	file, err := os.Create(path)
	if err != nil {
		return 0, "", fmt.Errorf("create slice %s: %w", path, err)
	}

	hasher := sha256.New()

	written, copyErr := io.CopyN(io.MultiWriter(file, hasher), ctxReader{ctx: ctx, r: src}, sliceSize)

	closeErr := file.Close()

	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		return written, "", fmt.Errorf("write slice %s: %w", path, copyErr)
	}

	if closeErr != nil {
		return written, "", fmt.Errorf("close slice %s: %w", path, closeErr)
	}

	return written, hex.EncodeToString(hasher.Sum(nil)), nil
}
