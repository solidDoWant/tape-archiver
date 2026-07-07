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
	"log/slog"
	"os"
	slashpath "path"
	"path/filepath"
)

// Tar writes a tar archive of the contents of srcDir to w. Entry names are
// relative to srcDir, so extracting the archive reproduces srcDir's contents
// without the srcDir path prefix.
//
// The archive captures regular files, directories, and symlinks, preserving
// their mode bits, ownership, and modification time. Hardlinked regular files
// are stored once (the first sighting) and reproduced as tar hardlink entries;
// sparse files are stored in GNU sparse 1.0 form so their holes are not written
// out. File types that a portable file-by-file tar cannot carry and that hold no
// recoverable data — sockets, devices, and named pipes (FIFOs) — are skipped
// with a warning rather than failing the run. Extended attributes, POSIX ACLs,
// and file capabilities are NOT captured (SPEC §6); a tar-level restore does not
// reproduce them.
//
// Tar streams: it never stages the archive to disk. Filesystem-level tar (not
// zfs send) is the deliberate, longest-lived source format (SPEC §6), and this
// is the first stage of the per-archive prepare pipeline (SPEC §4.3).
func Tar(ctx context.Context, w io.Writer, srcDir string) error {
	writer := newTarWriter(w)

	if err := writer.tree(ctx, srcDir, ""); err != nil {
		return fmt.Errorf("tar %s: %w", srcDir, err)
	}

	if err := writer.tw.Close(); err != nil {
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
	writer := newTarWriter(w)

	for _, member := range members {
		if err := writer.tree(ctx, member.Dir, member.Subdir); err != nil {
			return fmt.Errorf("tar member %s: %w", member.Subdir, err)
		}
	}

	if err := writer.tw.Close(); err != nil {
		return fmt.Errorf("tar members: close: %w", err)
	}

	return nil
}

// tarWriter carries the shared state for writing one archive: the archive/tar
// writer, the underlying stream (used to emit hand-rolled sparse members that
// the standard writer cannot produce), and the archive-wide hardlink index.
type tarWriter struct {
	tw  *tar.Writer
	raw io.Writer
	// links maps an already-written file's {device, inode} identity to its
	// in-archive name so later sightings become hardlink entries. It spans the
	// whole archive; distinct source datasets have distinct device numbers, so
	// links never falsely cross unrelated members.
	links map[fileID]string
}

func newTarWriter(w io.Writer) *tarWriter {
	return &tarWriter{
		tw:    tar.NewWriter(w),
		raw:   w,
		links: make(map[fileID]string),
	}
}

// fileID identifies an inode within a device. It is the key used to detect
// hardlinks: two paths with the same fileID name the same on-disk file.
type fileID struct {
	dev uint64
	ino uint64
}

// sparseRegion is one allocated data extent of a sparse file: length bytes of
// real data starting at offset. The gaps between regions (and any tail up to the
// logical size) are holes that read back as zeros.
type sparseRegion struct {
	offset int64
	length int64
}

// tree walks srcDir and writes each entry with its name relative to srcDir,
// prefixed by prefix (the member subdirectory, empty for a single tree).
func (w *tarWriter) tree(ctx context.Context, srcDir, prefix string) error {
	return filepath.WalkDir(srcDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		return w.writeEntry(ctx, srcDir, prefix, path, entry)
	})
}

// writeEntry writes a single filesystem entry, with a name relative to srcDir
// and prefixed by prefix. With no prefix the root directory itself is skipped so
// the archive holds only its contents (Tar); with a prefix the root maps to the
// member subdirectory entry so empty members are preserved (TarMembers).
func (w *tarWriter) writeEntry(ctx context.Context, srcDir, prefix, path string, entry fs.DirEntry) error {
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

	mode := info.Mode()

	// Sockets, devices, and named pipes (FIFOs) cannot be represented by a
	// portable file-by-file tar and carry no recoverable data. Skip them with a
	// warning so a stale socket in a snapshot (e.g. a database datadir) does not
	// abort the whole run.
	if mode&(fs.ModeSocket|fs.ModeDevice|fs.ModeCharDevice|fs.ModeNamedPipe) != 0 {
		slog.WarnContext(ctx, "skipping non-archivable entry",
			"path", path, "type", mode.Type().String())

		return nil
	}

	link := ""
	if mode&fs.ModeSymlink != 0 {
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

	if !mode.IsRegular() {
		return w.writeHeaderOnly(header)
	}

	// A regular file with more than one link is stored once. The first sighting
	// records the in-archive name and writes the body; later sightings of the
	// same inode become hardlink entries pointing at it, so the archive is not
	// enlarged by a second full copy.
	if id, ok := hardlinkID(info); ok {
		if target, seen := w.links[id]; seen {
			header.Typeflag = tar.TypeLink
			header.Linkname = target
			header.Size = 0

			return w.writeHeaderOnly(header)
		}

		w.links[id] = header.Name
	}

	return w.writeRegularFile(ctx, header, path)
}

// writeHeaderOnly writes a header that carries no file body (directory, symlink,
// or hardlink entry).
func (w *tarWriter) writeHeaderOnly(header *tar.Header) error {
	if err := w.tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write header %s: %w", header.Name, err)
	}

	return nil
}

// writeRegularFile writes the header and body for a regular file at path. Sparse
// files are encoded so their holes are not written out; all other files stream
// contiguously. It closes the file explicitly so a close error on the read side
// is not silently dropped.
func (w *tarWriter) writeRegularFile(ctx context.Context, header *tar.Header, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}

	writeErr := w.writeRegularBody(ctx, header, file, path)
	closeErr := file.Close()

	if writeErr != nil {
		return writeErr
	}

	if closeErr != nil {
		return fmt.Errorf("close %s: %w", path, closeErr)
	}

	return nil
}

// writeRegularBody writes header then copies file's contents. When the file is
// sparse it is emitted as a GNU sparse 1.0 entry; otherwise the contents stream
// contiguously. The copy honors ctx cancellation per read buffer so a single
// large file does not block cancellation.
func (w *tarWriter) writeRegularBody(ctx context.Context, header *tar.Header, file *os.File, path string) error {
	regions, sparse, err := sparseDataRegions(file, header.Size)
	if err != nil {
		return fmt.Errorf("probe %s: %w", path, err)
	}

	if sparse {
		return w.writeSparseEntry(ctx, header, file, regions)
	}

	if err := w.writeHeaderOnly(header); err != nil {
		return err
	}

	if _, err := io.Copy(w.tw, ctxReader{ctx: ctx, r: file}); err != nil {
		return fmt.Errorf("copy %s: %w", path, err)
	}

	return nil
}
