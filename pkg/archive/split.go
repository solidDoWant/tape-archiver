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

// Split reads from src and writes it to dir as consecutive fixed-size slice
// files of sliceSize bytes each; the final slice holds the remainder. Slice
// files are named "<baseName>.NNN" starting at .000. Every slice in one archive
// shares a single zero-padded suffix width — the minimum of three, widened to
// the digit count the final slice total needs (e.g. 1001 slices -> four digits,
// .0000 … .1000). A uniform width guarantees lexical filename order equals
// numeric slice order for any slice count, so the documented recovery glob
// (archive.[0-9]*) reassembles the stream correctly. Split returns the slice
// paths in order — the authoritative order whose concatenation reconstructs the
// stream exactly.
//
// Slicing bounds the blast radius of a damaged tape region to a single slice,
// recoverable via PAR2 or the redundant copy (SPEC §8); it is the final prepare
// stage before checksumming (SPEC §4.3). An empty stream produces no slices.
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
			// EOF: the slice just created is empty. Remove it so an input whose
			// length is an exact multiple of sliceSize (or an empty input) leaves
			// no trailing empty file behind.
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

// normalizeWidth ensures every slice in the archive shares one zero-padded
// suffix width. The write loop names slices with the provisional three-digit
// "%03d" pad; once the count is known, the final width is max(3, digits of the
// highest index). When that exceeds three the slices are renamed to the wider
// uniform pad and the returned paths updated to match. Fewer than 1001 slices
// need no rename (highest index <= 999 fits three digits), so the common case
// is untouched. This runs in the prepare phase on staging disk before any tape
// write (SPEC §4.3), so the renames never touch the write window.
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

// writeSlice creates the file at path and copies up to sliceSize bytes into it
// from src, returning the number of bytes written. A return below sliceSize
// means src reached EOF. The copy honors ctx cancellation per read buffer so a
// single large slice does not block cancellation.
func writeSlice(ctx context.Context, path string, src io.Reader, sliceSize int64) (int64, error) {
	file, err := os.Create(path)
	if err != nil {
		return 0, fmt.Errorf("create slice %s: %w", path, err)
	}

	written, copyErr := io.CopyN(file, ctxReader{ctx: ctx, r: src}, sliceSize)

	closeErr := file.Close()

	// io.CopyN reports io.EOF when src ends before sliceSize bytes; that is the
	// normal end-of-stream signal, not a failure.
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		return written, fmt.Errorf("write slice %s: %w", path, copyErr)
	}

	if closeErr != nil {
		return written, fmt.Errorf("close slice %s: %w", path, closeErr)
	}

	return written, nil
}
