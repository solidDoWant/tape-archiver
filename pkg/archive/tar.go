// Package archive implements the streaming data-preparation stages of the
// per-archive prepare pipeline (SPEC §4.3 step 2): tar the source tree, an
// optional zstd compression pass, and splitting the resulting byte stream into
// fixed-size slice files staged on disk. Each stage is an io stream stage so
// they compose via io.Pipe; workflow wiring lives in a separate package.
package archive

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Tar writes a tar archive of the contents of srcDir to w. Entry names are
// relative to srcDir, so extracting the archive reproduces srcDir's contents
// without the srcDir path prefix. Regular files, directories, and symlinks are
// included, with their mode bits preserved.
//
// Tar streams: it never stages the archive to disk. Filesystem-level tar (not
// zfs send) is the deliberate, longest-lived source format (SPEC §6), and this
// is the first stage of the per-archive prepare pipeline (SPEC §4.3).
func Tar(ctx context.Context, w io.Writer, srcDir string) error {
	tw := tar.NewWriter(w)

	walkErr := filepath.WalkDir(srcDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		return writeTarEntry(tw, srcDir, path, entry)
	})
	if walkErr != nil {
		return fmt.Errorf("tar %s: %w", srcDir, walkErr)
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("tar %s: close: %w", srcDir, err)
	}

	return nil
}

// writeTarEntry writes a single filesystem entry to tw, with a name relative to
// srcDir. The root directory itself is skipped so the archive holds only its
// contents.
func writeTarEntry(tw *tar.Writer, srcDir, path string, entry fs.DirEntry) error {
	rel, err := filepath.Rel(srcDir, path)
	if err != nil {
		return err
	}

	if rel == "." {
		return nil
	}

	info, err := entry.Info()
	if err != nil {
		return err
	}

	link := ""
	if info.Mode()&fs.ModeSymlink != 0 {
		link, err = os.Readlink(path)
		if err != nil {
			return err
		}
	}

	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}

	// Use forward slashes (the tar convention) and mark directories so the
	// extracted layout matches the source on any platform.
	header.Name = filepath.ToSlash(rel)
	if entry.IsDir() {
		header.Name += "/"
	}

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write header %s: %w", header.Name, err)
	}

	if !info.Mode().IsRegular() {
		return nil
	}

	return copyFileContents(tw, path)
}

// copyFileContents streams the contents of the file at path into tw. It closes
// the file explicitly so a close error on the read side is not silently
// dropped.
func copyFileContents(tw *tar.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(tw, file)

	closeErr := file.Close()

	if copyErr != nil {
		return fmt.Errorf("copy %s: %w", path, copyErr)
	}

	if closeErr != nil {
		return fmt.Errorf("close %s: %w", path, closeErr)
	}

	return nil
}
