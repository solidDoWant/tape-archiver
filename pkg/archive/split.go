package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// Split writes src to dir as sliceSize-byte files "baseName.NNN" and returns
// their paths in order. All slices share one zero-padded width (at least three
// digits) so lexical order equals numeric order and the recovery procedure's
// sorted reassembly is correct. Slicing bounds a damaged tape region's
// blast radius to one slice (SPEC §8).
func Split(ctx context.Context, src io.Reader, sliceSize int64, dir, baseName string) ([]string, error) {
	if sliceSize <= 0 {
		return nil, fmt.Errorf("slice size must be positive, got %d", sliceSize)
	}

	var paths []string

	for index := 0; ; index++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		path := filepath.Join(dir, fmt.Sprintf("%s.%03d", baseName, index))

		written, err := writeSlice(ctx, path, src, sliceSize)
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

		paths = append(paths, path)

		if written < sliceSize {
			break
		}
	}

	return normalizeWidth(paths, dir, baseName)
}

// normalizeWidth renames slices to a uniform pad once the count exceeds 1000.
func normalizeWidth(paths []string, dir, baseName string) ([]string, error) {
	if len(paths) == 0 {
		return paths, nil
	}

	width := len(strconv.Itoa(len(paths) - 1))
	if width <= 3 {
		return paths, nil
	}

	for index, oldPath := range paths {
		newPath := filepath.Join(dir, fmt.Sprintf("%s.%0*d", baseName, width, index))
		if newPath == oldPath {
			continue
		}

		if err := os.Rename(oldPath, newPath); err != nil {
			return nil, fmt.Errorf("rename slice %s to uniform width %s: %w", oldPath, newPath, err)
		}

		paths[index] = newPath
	}

	return paths, nil
}

// writeSlice copies up to sliceSize bytes; a short count means EOF.
func writeSlice(ctx context.Context, path string, src io.Reader, sliceSize int64) (int64, error) {
	file, err := os.Create(path)
	if err != nil {
		return 0, fmt.Errorf("create slice %s: %w", path, err)
	}

	written, copyErr := io.CopyN(file, ctxReader{ctx: ctx, r: src}, sliceSize)

	closeErr := file.Close()

	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		return written, fmt.Errorf("write slice %s: %w", path, copyErr)
	}

	if closeErr != nil {
		return written, fmt.Errorf("close slice %s: %w", path, closeErr)
	}

	return written, nil
}
