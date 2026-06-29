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
	slashpath "path"
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

	if err := tarTree(ctx, tw, srcDir, ""); err != nil {
		return fmt.Errorf("tar %s: %w", srcDir, err)
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("tar %s: close: %w", srcDir, err)
	}

	return nil
}

// Member is one source tree packed into a multi-member tar by TarMembers. Subdir
// is the subdirectory the member's contents appear under in the archive; Dir is
// the source directory whose contents are read.
type Member struct {
	// Subdir is the in-archive subdirectory for this member. It must be a single
	// path element (no slashes); callers derive it from the member's identity
	// (e.g. a PVC name).
	Subdir string
	// Dir is the source directory whose contents are tarred under Subdir/.
	Dir string
}

// TarMembers writes a single tar archive to w holding every member's tree, each
// under its own Name/ subdirectory. It is how a snapshot group is archived as one
// tar with one subdirectory per member volume, giving cross-volume consistency
// (SPEC §5). Each member's directory entry is emitted even when empty, so the
// extracted layout always reproduces the member subdirectories.
//
// Like Tar, it streams and never stages to disk; it is the multi-member form of
// the prepare pipeline's first stage (SPEC §4.3).
func TarMembers(ctx context.Context, w io.Writer, members []Member) error {
	tw := tar.NewWriter(w)

	for _, member := range members {
		if err := tarTree(ctx, tw, member.Dir, member.Subdir); err != nil {
			return fmt.Errorf("tar member %s: %w", member.Subdir, err)
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("tar members: close: %w", err)
	}

	return nil
}

// tarTree walks srcDir and writes each entry to tw with its name relative to
// srcDir, prefixed by prefix (the member subdirectory, empty for a single tree).
func tarTree(ctx context.Context, tw *tar.Writer, srcDir, prefix string) error {
	return filepath.WalkDir(srcDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		return writeTarEntry(ctx, tw, srcDir, prefix, path, entry)
	})
}

// writeTarEntry writes a single filesystem entry to tw, with a name relative to
// srcDir and prefixed by prefix. With no prefix the root directory itself is
// skipped so the archive holds only its contents (Tar); with a prefix the root
// maps to the member subdirectory entry so empty members are preserved
// (TarMembers).
func writeTarEntry(ctx context.Context, tw *tar.Writer, srcDir, prefix, path string, entry fs.DirEntry) error {
	rel, err := filepath.Rel(srcDir, path)
	if err != nil {
		return err
	}

	if rel == "." {
		if prefix == "" {
			return nil
		}

		rel = ""
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
	// extracted layout matches the source on any platform. A non-empty prefix
	// places the entry under the member subdirectory (TarMembers).
	name := filepath.ToSlash(rel)
	if prefix != "" {
		name = slashpath.Join(prefix, name)
	}

	header.Name = name
	if entry.IsDir() {
		header.Name += "/"
	}

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write header %s: %w", header.Name, err)
	}

	if !info.Mode().IsRegular() {
		return nil
	}

	return copyFileContents(ctx, tw, path)
}

// copyFileContents streams the contents of the file at path into tw. The copy
// honors ctx cancellation per read buffer so a single large file does not block
// cancellation. It closes the file explicitly so a close error on the read side
// is not silently dropped.
func copyFileContents(ctx context.Context, tw *tar.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(tw, ctxReader{ctx: ctx, r: file})

	closeErr := file.Close()

	if copyErr != nil {
		return fmt.Errorf("copy %s: %w", path, copyErr)
	}

	if closeErr != nil {
		return fmt.Errorf("close %s: %w", path, closeErr)
	}

	return nil
}
