//go:build unix

package archive_test

import (
	"archive/tar"
	"bytes"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/solidDoWant/tape-archiver/pkg/archive"
)

// syncBuffer is a concurrency-safe buffer so a slog handler can be read after
// the archive walk finishes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// captureLogs swaps the global slog logger for one writing to the returned
// buffer for the duration of the test, restoring the original afterward.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()

	buf := &syncBuffer{}
	previous := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return buf
}

// TestTarSkipsIrregularFiles covers AC1: a source tree containing a unix socket
// (and a FIFO) is archived successfully — the run does not fail — every regular
// file, directory, and symlink is present, the irregular entries are absent, and
// each skip is surfaced as a warning naming the path.
func TestTarSkipsIrregularFiles(t *testing.T) {
	// Not parallel: it swaps the process-global slog logger.
	logs := captureLogs(t)

	// A short path root keeps the unix socket address under sun_path's 108-byte
	// limit regardless of the (possibly long) test temp dir.
	src, err := os.MkdirTemp("/tmp", "tar-irregular")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(src) })

	writeFile(t, filepath.Join(src, "top.txt"), []byte("regular contents"))
	require.NoError(t, os.MkdirAll(filepath.Join(src, "sub"), 0o755))
	writeFile(t, filepath.Join(src, "sub", "inner.bin"), []byte("nested"))
	require.NoError(t, os.Symlink("top.txt", filepath.Join(src, "link.txt")))

	sockPath := filepath.Join(src, "app.sock")
	listener, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	fifoPath := filepath.Join(src, "pipe.fifo")
	require.NoError(t, syscall.Mkfifo(fifoPath, 0o644))

	var buf bytes.Buffer

	require.NoError(t, archive.Tar(t.Context(), &buf, src),
		"an irregular file must not fail the run")

	dest := t.TempDir()
	extractTar(t, &buf, dest)

	got := snapshot(t, dest)

	assert.Contains(t, got, "top.txt")
	assert.Contains(t, got, "sub")
	assert.Contains(t, got, "sub/inner.bin")
	assert.Contains(t, got, "link.txt")
	assert.Equal(t, []byte("regular contents"), got["top.txt"].content)

	assert.NotContains(t, got, "app.sock", "the socket must be skipped, not archived")
	assert.NotContains(t, got, "pipe.fifo", "the FIFO must be skipped, not archived")

	output := logs.String()
	assert.Contains(t, output, "skipping non-archivable entry")
	assert.Contains(t, output, "app.sock", "the warning must name the skipped socket")
	assert.Contains(t, output, "pipe.fifo", "the warning must name the skipped FIFO")
}

// TestTarDeduplicatesHardlinks covers AC2: two paths hardlinked to one file are
// stored once (the stream is not enlarged by a second full copy) and extraction
// reproduces both paths with identical content.
func TestTarDeduplicatesHardlinks(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	content := bytes.Repeat([]byte("hardlinked payload block "), 8192) // ~200 KiB

	first := filepath.Join(src, "original.bin")
	second := filepath.Join(src, "linked.bin")

	writeFile(t, first, content)
	require.NoError(t, os.Link(first, second))

	var buf bytes.Buffer

	require.NoError(t, archive.Tar(t.Context(), &buf, src))

	raw := buf.Bytes()
	assert.Less(t, len(raw), len(content)+(64<<10),
		"the archive must hold one copy of the shared file, not two")

	// Exactly one file body and one hardlink entry are emitted.
	var (
		bodies int
		links  int
	)

	reader := tar.NewReader(bytes.NewReader(raw))

	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}

		require.NoError(t, err)

		switch header.Typeflag {
		case tar.TypeReg:
			bodies++
		case tar.TypeLink:
			links++

			assert.NotEmpty(t, header.Linkname, "a hardlink entry must reference its target")
		}
	}

	assert.Equal(t, 1, bodies, "the file body must be stored exactly once")
	assert.Equal(t, 1, links, "the second path must be a hardlink entry")

	dest := t.TempDir()
	extractTar(t, &buf, dest)

	assert.Equal(t, snapshot(t, src), snapshot(t, dest),
		"both hardlinked paths must extract with identical content")
}

// TestTarSparseFile covers AC3: a sparse file whose logical size greatly exceeds
// its allocated size does not grow the archive to the full logical size (with no
// compression in play — Tar writes a raw stream), and extraction reproduces the
// file's contents including its holes.
func TestTarSparseFile(t *testing.T) {
	t.Parallel()

	const (
		logical  = int64(16 << 20) // 16 MiB logical
		chunkLen = 64 << 10        // 64 KiB of real data at each end
	)

	src := t.TempDir()
	path := filepath.Join(src, "disk.img")

	head := bytes.Repeat([]byte("H"), chunkLen)
	tail := bytes.Repeat([]byte("T"), chunkLen)

	file, err := os.Create(path)
	require.NoError(t, err)

	_, err = file.WriteAt(head, 0)
	require.NoError(t, err)
	_, err = file.WriteAt(tail, logical-int64(len(tail)))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	// Precondition: the filesystem actually left a hole, otherwise the sparse
	// path is not being exercised.
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, logical, info.Size())

	stat := info.Sys().(*syscall.Stat_t)
	require.Less(t, stat.Blocks*512, logical,
		"test precondition: the file must be sparse on this filesystem")

	var buf bytes.Buffer

	require.NoError(t, archive.Tar(t.Context(), &buf, src))

	assert.Less(t, buf.Len(), 1<<20,
		"the archive must not grow to the file's full logical size")

	dest := t.TempDir()
	extractTar(t, &buf, dest)

	got, err := os.ReadFile(filepath.Join(dest, "disk.img"))
	require.NoError(t, err)

	require.Len(t, got, int(logical), "extraction must reproduce the full logical size")
	assert.Equal(t, head, got[:chunkLen], "leading data extent must match")
	assert.Equal(t, tail, got[logical-int64(len(tail)):], "trailing data extent must match")

	middle := got[chunkLen : logical-int64(len(tail))]
	assert.Equal(t, len(middle), bytes.Count(middle, []byte{0}),
		"the hole must extract as zeros")
}

// TestTarDropsExtendedAttributes covers AC4 (behavioral half): a user.* extended
// attribute set on a source file does not survive a tar round-trip, pinning the
// observed behavior that SPEC §6 and the recovery docs describe.
func TestTarDropsExtendedAttributes(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	path := filepath.Join(src, "tagged.txt")
	writeFile(t, path, []byte("payload"))

	const xattrName = "user.tape_archiver_test"

	if err := unix.Setxattr(path, xattrName, []byte("present"), 0); err != nil {
		t.Skipf("filesystem does not support user xattrs: %v", err)
	}

	// Confirm the attribute is really set on the source before archiving.
	require.NoError(t, unix.Setxattr(path, xattrName, []byte("present"), 0))

	var buf bytes.Buffer

	require.NoError(t, archive.Tar(t.Context(), &buf, src))

	dest := t.TempDir()
	extractTar(t, &buf, dest)

	value := make([]byte, 64)

	_, err := unix.Getxattr(filepath.Join(dest, "tagged.txt"), xattrName, value)
	require.Error(t, err, "the xattr must not survive extraction")
	assert.ErrorIs(t, err, unix.ENODATA)
}

// TestMetadataFidelityDocumented covers AC4 (documentation half): SPEC §6 and the
// recovery procedure state the fate of extended attributes, POSIX ACLs, and file
// capabilities, matching the observed drop.
func TestMetadataFidelityDocumented(t *testing.T) {
	t.Parallel()

	for _, docPath := range []string{"../../SPEC.md", "../../docs/recovery-procedure.md"} {
		content, err := os.ReadFile(docPath)
		require.NoError(t, err)

		text := strings.ToLower(string(content))

		assert.Contains(t, text, "extended attribute", "%s must state the xattr fate", docPath)
		assert.Contains(t, text, "acl", "%s must state the POSIX ACL fate", docPath)
		assert.Contains(t, text, "capabilit", "%s must state the file-capability fate", docPath)
	}
}
