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
	"time"
)

// Tar streams a tar of srcDir's contents (names relative to srcDir) to w.
//
// Hardlinks are stored once, sparse files as GNU sparse 1.0, and sockets,
// devices, and FIFOs are skipped with a warning. Xattrs, ACLs, and file
// capabilities are NOT captured (SPEC §6).
func Tar(ctx context.Context, w io.Writer, srcDir string) error {
	return tarWith(ctx, w, srcDir, defaultReadAhead)
}

func tarWith(ctx context.Context, w io.Writer, srcDir string, readAhead readAhead) error {
	writer := newTarWriter(w, readAhead)

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
	return tarMembersWith(ctx, w, members, defaultReadAhead)
}

func tarMembersWith(ctx context.Context, w io.Writer, members []Member, readAhead readAhead) error {
	// Validate before writing: tar extraction is last-entry-wins, so a duplicate
	// Subdir would silently drop a member at restore despite verifying cleanly.
	if err := validateMembers(members); err != nil {
		return err
	}

	writer := newTarWriter(w, readAhead)

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
	links     map[fileID]string
	readAhead readAhead
}

func newTarWriter(w io.Writer, readAhead readAhead) *tarWriter {
	return &tarWriter{
		tw:        tar.NewWriter(w),
		raw:       w,
		links:     make(map[fileID]string),
		readAhead: readAhead,
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
	if w.readAhead.workers > 0 {
		return w.treeReadAhead(ctx, srcDir, prefix)
	}

	return walkTree(ctx, srcDir, prefix, func(item walkEntry) error {
		return w.writeEntry(ctx, srcDir, prefix, item, nil)
	})
}

// walkEntry is one entry to archive, with its lstat result.
type walkEntry struct {
	path    string
	entry   fs.DirEntry
	info    fs.FileInfo
	infoErr error
}

// walkTree visits srcDir's entries in archive order, applying tree's rules.
// The root is visited first so its own metadata is archived.
func walkTree(ctx context.Context, srcDir, prefix string, visit func(walkEntry) error) error {
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
			info, infoErr := entry.Info()

			return visit(walkEntry{path: path, entry: entry, info: info, infoErr: infoErr})
		}

		if entry.Name() == zfsControlDir && filepath.Dir(path) == filepath.Clean(srcDir) {
			slog.DebugContext(ctx, "skipping ZFS control directory", "path", path)

			return fs.SkipDir
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		if err := visit(walkEntry{path: path, entry: entry, info: info}); err != nil {
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

// writeEntry writes one entry. The root becomes "./" (or the member directory
// under a prefix), so `tar -xp -C TARGET` restores its mode, owner, and mtime
// onto TARGET and empty members are preserved. pre, if set, is the file's
// read-ahead.
func (w *tarWriter) writeEntry(ctx context.Context, srcDir, prefix string, item walkEntry, pre *prefetch) error {
	path := item.path

	rel, err := filepath.Rel(srcDir, path)
	if err != nil {
		return err
	}

	if rel == "." && prefix != "" {
		rel = ""
	}

	if item.infoErr != nil {
		return item.infoErr
	}

	info := item.info
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

	// PAX keeps mtime to the nanosecond (an unset Format rounds it to the
	// second). atime and ctime are dropped: atime would make archives
	// nondeterministic and ctime cannot be restored.
	header.Format = tar.FormatPAX
	header.AccessTime = time.Time{}
	header.ChangeTime = time.Time{}

	name := filepath.ToSlash(rel)
	if prefix != "" {
		name = slashpath.Join(prefix, name)
	}

	header.Name = name
	if item.entry.IsDir() {
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

	return w.writeRegularFile(ctx, header, path, pre)
}

func (w *tarWriter) writeHeaderOnly(header *tar.Header) error {
	if err := w.tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write header %s: %w", header.Name, err)
	}

	return nil
}

func (w *tarWriter) writeRegularFile(ctx context.Context, header *tar.Header, path string, pre *prefetch) error {
	if pre != nil {
		return w.writePrefetched(ctx, header, path, pre)
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}

	regions, sparse, err := sparseDataRegions(file, header.Size)
	if err != nil {
		_ = file.Close()

		return fmt.Errorf("probe %s: %w", path, err)
	}

	return w.writeOpenFile(ctx, header, path, file, regions, sparse)
}

// writeOpenFile emits a GNU sparse 1.0 entry for sparse files, otherwise
// streams the contents, then closes file.
func (w *tarWriter) writeOpenFile(ctx context.Context, header *tar.Header, path string, file *os.File, regions []sparseRegion, sparse bool) error {
	var writeErr error
	if sparse {
		writeErr = w.writeSparseEntry(ctx, header, file, regions)
	} else {
		writeErr = w.writeStream(ctx, header, path, file)
	}

	closeErr := file.Close()

	if writeErr != nil {
		return writeErr
	}

	if closeErr != nil {
		return fmt.Errorf("close %s: %w", path, closeErr)
	}

	return nil
}

// writeStream writes header, then copies body with per-buffer cancellation.
func (w *tarWriter) writeStream(ctx context.Context, header *tar.Header, path string, body io.Reader) error {
	if err := w.writeHeaderOnly(header); err != nil {
		return err
	}

	if _, err := io.Copy(w.tw, ctxReader{ctx: ctx, r: body}); err != nil {
		return fmt.Errorf("copy %s: %w", path, err)
	}

	return nil
}
