//go:build unix

package archive_test

import (
	"archive/tar"
	"bytes"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
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

// dataExtent is a run of real (non-hole) bytes written at a given offset when
// building a sparse test fixture.
type dataExtent struct {
	offset int64
	data   []byte
}

// writeSparseFixture creates a sparse file of the given logical size with the
// supplied data extents written into it (the gaps are left as holes). It returns
// the file's full logical contents so extractions can be compared byte-for-byte.
// It skips the test when the filesystem did not actually leave a hole, so the
// sparse encoder is genuinely exercised.
func writeSparseFixture(t *testing.T, path string, logical int64, extents []dataExtent) []byte {
	t.Helper()

	file, err := os.Create(path)
	require.NoError(t, err)

	require.NoError(t, file.Truncate(logical))

	want := make([]byte, logical)

	for _, extent := range extents {
		_, err = file.WriteAt(extent.data, extent.offset)
		require.NoError(t, err)
		copy(want[extent.offset:], extent.data)
	}

	require.NoError(t, file.Close())

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, logical, info.Size())

	stat := info.Sys().(*syscall.Stat_t)
	if stat.Blocks*512 >= logical {
		t.Skipf("filesystem did not leave a hole for %s (allocated %d bytes for a %d-byte file); "+
			"the sparse path is not exercised here", filepath.Base(path), stat.Blocks*512, logical)
	}

	return want
}

// TestTarSparseFile covers AC1–AC3: sparse files (including trailing-hole and
// all-hole layouts) do not grow the archive to their full logical size, and extract
// byte-identically at their full logical size through *both* a real GNU tar binary —
// the family the recovery disc ships, whose sparse 1.0 decoder does not honor
// GNU.sparse.realsize and so truncates a map that omits the terminal hole entry — and
// Go's archive/tar reader.
func TestTarSparseFile(t *testing.T) {
	t.Parallel()

	const logical = int64(8 << 20) // 8 MiB logical

	head := bytes.Repeat([]byte("H"), 64<<10)
	tail := bytes.Repeat([]byte("T"), 64<<10)

	testCases := []struct {
		name    string
		extents []dataExtent
	}{
		{
			// Data at the start, hole through to the logical end: the case GNU tar
			// silently truncated before the terminal {realsize, 0} map entry.
			name:    "trailing hole",
			extents: []dataExtent{{offset: 0, data: head}},
		},
		{
			// No data at all: an all-hole file of nonzero logical size.
			name:    "all hole",
			extents: nil,
		},
		{
			// Data at both ends, hole in the middle, ending in data: the pre-fix
			// layout, kept as a regression guard.
			name: "ends in data",
			extents: []dataExtent{
				{offset: 0, data: head},
				{offset: logical - int64(len(tail)), data: tail},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			src := t.TempDir()
			path := filepath.Join(src, "disk.img")

			want := writeSparseFixture(t, path, logical, testCase.extents)

			var buf bytes.Buffer

			require.NoError(t, archive.Tar(t.Context(), &buf, src))

			assert.Less(t, buf.Len(), int(logical),
				"the archive must not grow to the file's full logical size")

			archived := buf.Bytes()

			for _, extractor := range []struct {
				name    string
				extract func(t *testing.T, archived []byte, dest string)
			}{
				{"gnu tar", extractWithGNUTar},
				{"go reader", func(t *testing.T, archived []byte, dest string) {
					extractTar(t, bytes.NewReader(archived), dest)
				}},
			} {
				t.Run(extractor.name, func(t *testing.T) {
					dest := t.TempDir()
					extractor.extract(t, archived, dest)

					got, err := os.ReadFile(filepath.Join(dest, "disk.img"))
					require.NoError(t, err)

					require.Len(t, got, int(logical),
						"extraction must reproduce the full logical size")
					assert.Equal(t, want, got,
						"extracted contents must be byte-identical to the source, holes intact")
				})
			}
		})
	}
}

// extractWithGNUTar extracts archived into dest by invoking the bundled GNU tar
// binary — the family the recovery disc ships and that docs/recovery-procedure.md
// runs as `bin/tar -xf`. Unlike Go's archive/tar reader it does not extend sparse
// files to GNU.sparse.realsize, so it is the extractor that reveals a sparse map
// missing its terminal hole entry. The test skips when no tar binary is available.
func extractWithGNUTar(t *testing.T, archived []byte, dest string) {
	t.Helper()

	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no tar binary available to validate GNU sparse extraction")
	}

	cmd := exec.CommandContext(t.Context(), "tar", "-x", "-f", "-", "-C", dest)
	cmd.Stdin = bytes.NewReader(archived)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	require.NoError(t, cmd.Run(), "gnu tar extraction failed: %s", stderr.String())
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
