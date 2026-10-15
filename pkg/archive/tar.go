// Package archive implements the streaming tar, zstd, and split stages of the
// per-archive prepare pipeline (SPEC §4.3 step 2).
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
	"strings"
)

// Tar streams a tar of srcDir's contents (names relative to srcDir) to w.
//
// Hardlinks are stored once, sparse files as GNU sparse 1.0, and sockets,
// devices, and FIFOs are skipped with a warning. Xattrs, ACLs, and file
// capabilities are NOT captured (SPEC §6).
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

// Member is one source tree packed by TarMembers.
type Member struct {
	// Subdir is the in-archive subdirectory; it must be a single path element.
	Subdir string
	// Dir is the source directory tarred under Subdir/.
	Dir string
}

// TarMembers streams one tar holding each member's tree under its own Subdir/,
// archiving a snapshot group consistently (SPEC §5). Empty members still get
// their directory entry.
func TarMembers(ctx context.Context, w io.Writer, members []Member) error {
	// Validate before writing: tar extraction is last-entry-wins, so a duplicate
	// Subdir would silently drop a member at restore despite verifying cleanly.
	if err := validateMembers(members); err != nil {
		return err
	}

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

// validateMembers requires each Subdir to be a unique, non-empty, single path
// element.
func validateMembers(members []Member) error {
	firstIndex := make(map[string]int, len(members))

	for index, member := range members {
		switch {
		case member.Subdir == "":
			return fmt.Errorf("tar members: members[%d] (%q) has an empty subdirectory; each member needs a single-path-element subdirectory", index, member.Dir)
		case strings.Contains(member.Subdir, "/"):
			return fmt.Errorf("tar members: members[%d] subdirectory %q (%q) must be a single path element, not a path", index, member.Subdir, member.Dir)
		}

		if prior, ok := firstIndex[member.Subdir]; ok {
			return fmt.Errorf("tar members: duplicate member subdirectory %q (members[%d] %q and members[%d] %q); members would collide on extraction",
				member.Subdir, prior, members[prior].Dir, index, member.Dir)
		}

		firstIndex[member.Subdir] = index
	}

	return nil
}

type tarWriter struct {
	tw  *tar.Writer
	raw io.Writer // for hand-rolled sparse members archive/tar cannot produce
	// links maps written inodes to their in-archive name. Distinct datasets have
	// distinct device numbers, so links never cross unrelated members.
	links map[fileID]string
}

func newTarWriter(w io.Writer) *tarWriter {
	return &tarWriter{
		tw:    tar.NewWriter(w),
		raw:   w,
		links: make(map[fileID]string),
	}
}

type fileID struct {
	dev uint64
	ino uint64
}

// sparseRegion is one allocated data extent; gaps between regions are holes.
type sparseRegion struct {
	offset int64
	length int64
}

// zfsControlDir is the ZFS control directory at a dataset's root; with
// snapdir=visible its snapshot/ holds every snapshot of the dataset.
const zfsControlDir = ".zfs"

// tree archives srcDir's own filesystem only, as a snapshot would: the root's
// .zfs control directory is skipped, and a filesystem mounted below srcDir (e.g.
// a child dataset) is archived as its empty mountpoint directory.
func (w *tarWriter) tree(ctx context.Context, srcDir, prefix string) error {
	rootDev, err := dirDevice(srcDir)
	if err != nil {
		return err
	}

	return filepath.WalkDir(srcDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		if !entry.IsDir() || path == srcDir {
			return w.writeEntry(ctx, srcDir, prefix, path, entry)
		}

		if entry.Name() == zfsControlDir && filepath.Dir(path) == filepath.Clean(srcDir) {
			slog.DebugContext(ctx, "skipping ZFS control directory", "path", path)

			return fs.SkipDir
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		if err := w.writeEntry(ctx, srcDir, prefix, path, entry); err != nil {
			return err
		}

		if dev, ok := deviceOf(info); ok && dev != rootDev {
			slog.InfoContext(ctx, "not descending into a separate filesystem mounted inside the source", "path", path)

			return fs.SkipDir
		}

		return nil
	})
}

// dirDevice returns the device of the filesystem holding dir's contents. It
// opens dir rather than stat'ing its path so an automounted directory (a ZFS
// snapshot) reports the mounted filesystem, not its placeholder.
func dirDevice(dir string) (uint64, error) {
	file, err := os.Open(dir)
	if err != nil {
		return 0, err
	}

	info, err := file.Stat()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		return 0, err
	}

	dev, _ := deviceOf(info)

	return dev, nil
}

// writeEntry writes one entry. Without a prefix the root is skipped; with one it
// becomes the member directory entry so empty members are preserved.
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

	// These hold no recoverable data; skip so e.g. a stale database socket does
	// not abort the run.
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

	// Later sightings of a multiply-linked inode become hardlink entries.
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

func (w *tarWriter) writeHeaderOnly(header *tar.Header) error {
	if err := w.tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write header %s: %w", header.Name, err)
	}

	return nil
}

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

// writeRegularBody emits a GNU sparse 1.0 entry for sparse files, otherwise
// streams the contents with per-buffer cancellation.
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
