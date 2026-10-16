//go:build unix

package archive_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/archive"
)

var serialReadAhead = archive.ReadAhead{}

// readAheadConfigs spans tight and loose bounds, so the memory budget and
// worker limit both block during a run.
var readAheadConfigs = []struct {
	name   string
	config archive.ReadAhead
}{
	{name: "default", config: archive.DefaultReadAhead},
	{name: "one worker", config: archive.ReadAhead{Workers: 1, MaxFile: 64 << 10, Memory: 64 << 10, Window: 4}},
	{name: "tight memory", config: archive.ReadAhead{Workers: 8, MaxFile: 16 << 10, Memory: 20 << 10, Window: 16}},
	{name: "many workers", config: archive.ReadAhead{Workers: 64, MaxFile: 256 << 10, Memory: 1 << 20, Window: 1}},
	{name: "memory below max file", config: archive.ReadAhead{Workers: 4, MaxFile: 1 << 20, Memory: 8 << 10, Window: 64}},
}

// buildMixedTree writes small files of varying sizes, files at and above the
// read-ahead caps, empty files and directories, symlinks, hardlinks, and
// sparse files.
func buildMixedTree(t *testing.T, root string) {
	t.Helper()

	random := rand.New(rand.NewPCG(428, 1))

	sizes := []int{0, 1, 511, 512, 513, 16<<10 - 1, 16 << 10, 16<<10 + 1, 64<<10 - 1, 64 << 10, 256 << 10, 1<<20 - 1, 1 << 20, 3 << 20}
	for range 300 {
		sizes = append(sizes, random.IntN(40<<10))
	}

	for index, size := range sizes {
		dir := filepath.Join(root, fmt.Sprintf("dir%02d", index%7), fmt.Sprintf("sub%d", index%3))
		require.NoError(t, os.MkdirAll(dir, 0o755))

		content := make([]byte, size)
		for offset := range content {
			content[offset] = byte(random.UintN(256))
		}

		writeFile(t, filepath.Join(dir, fmt.Sprintf("file%04d.bin", index)), content)
	}

	require.NoError(t, os.MkdirAll(filepath.Join(root, "empty", "nested"), 0o755))
	require.NoError(t, os.Symlink("../dir00/sub0/file0000.bin", filepath.Join(root, "dir01", "link")))
	require.NoError(t, os.Symlink("missing", filepath.Join(root, "dangling")))

	writeFile(t, filepath.Join(root, "linked-small"), []byte("hardlinked small file"))
	require.NoError(t, os.Link(filepath.Join(root, "linked-small"), filepath.Join(root, "dir02", "linked-small-2")))
	writeFile(t, filepath.Join(root, "linked-large"), bytes.Repeat([]byte("L"), 2<<20))
	require.NoError(t, os.Link(filepath.Join(root, "linked-large"), filepath.Join(root, "dir03", "linked-large-2")))

	// Sparse files below and above the read-ahead caps.
	writeSparse(t, filepath.Join(root, "sparse-small"), 48<<10, []dataExtent{{offset: 0, data: []byte("head")}})
	writeSparse(t, filepath.Join(root, "sparse-large"), 8<<20, []dataExtent{
		{offset: 1 << 20, data: bytes.Repeat([]byte("D"), 4096)},
		{offset: 5 << 20, data: []byte("tail")},
	})
}

// writeSparse writes a file with holes; unlike writeSparseFixture it does not
// skip where the filesystem fills them, as both paths then see a dense file.
func writeSparse(t *testing.T, path string, logical int64, extents []dataExtent) {
	t.Helper()

	file, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(logical))

	for _, extent := range extents {
		_, err = file.WriteAt(extent.data, extent.offset)
		require.NoError(t, err)
	}

	require.NoError(t, file.Close())
}

func tarBytes(t *testing.T, src string, config archive.ReadAhead) []byte {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, archive.TarWith(t.Context(), &buf, src, config))

	return buf.Bytes()
}

// TestTarReadAheadByteIdentical checks the read-ahead path emits exactly the
// serial path's bytes.
func TestTarReadAheadByteIdentical(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	buildMixedTree(t, src)

	// The first read can bump atimes (relatime), which sparse PAX headers
	// record; settle them before taking the reference.
	tarBytes(t, src, serialReadAhead)
	want := tarBytes(t, src, serialReadAhead)

	for _, readAhead := range readAheadConfigs {
		t.Run(readAhead.name, func(t *testing.T) {
			got := tarBytes(t, src, readAhead.config)
			require.Equal(t, len(want), len(got))
			assert.True(t, bytes.Equal(want, got), "read-ahead tar differs from the serial tar")
		})
	}
}

func TestTarMembersReadAheadByteIdentical(t *testing.T) {
	t.Parallel()

	first, second := t.TempDir(), t.TempDir()
	buildMixedTree(t, first)
	writeFile(t, filepath.Join(second, "only.txt"), []byte("second member"))

	members := []archive.Member{{Subdir: "a", Dir: first}, {Subdir: "b", Dir: second}, {Subdir: "c", Dir: t.TempDir()}}

	tarMembers := func(config archive.ReadAhead) []byte {
		var buf bytes.Buffer
		require.NoError(t, archive.TarMembersWith(t.Context(), &buf, members, config))

		return buf.Bytes()
	}

	tarMembers(serialReadAhead)
	want := tarMembers(serialReadAhead)

	for _, readAhead := range readAheadConfigs {
		t.Run(readAhead.name, func(t *testing.T) {
			assert.True(t, bytes.Equal(want, tarMembers(readAhead.config)), "read-ahead tar differs from the serial tar")
		})
	}
}

// failingWriter fails once limit bytes have been written.
type failingWriter struct {
	buf   bytes.Buffer
	limit int
}

var errWriterFull = errors.New("writer full")

func (w *failingWriter) Write(p []byte) (int, error) {
	room := w.limit - w.buf.Len()
	if room >= len(p) {
		return w.buf.Write(p)
	}

	w.buf.Write(p[:max(room, 0)])

	return max(room, 0), errWriterFull
}

// TestTarReadAheadErrorsMatchSerial checks a failure part-way through the
// stream (an unreadable source file, or the output failing) surfaces the same
// error after the same output as the serial path.
func TestTarReadAheadErrorsMatchSerial(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions, so the unreadable-file case cannot fail")
	}

	src := t.TempDir()
	buildMixedTree(t, src)

	unreadable := filepath.Join(src, "dir04", "sub1", "unreadable.bin")
	writeFile(t, unreadable, []byte("secret"))
	require.NoError(t, os.Chmod(unreadable, 0o000))

	tests := []struct {
		name  string
		limit int
	}{
		{name: "unreadable file", limit: 1 << 30},
		{name: "output fails in a header", limit: 512*3 + 100},
		{name: "output fails in a body", limit: 200_000},
		{name: "output fails late", limit: 3_000_000},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serial := &failingWriter{limit: test.limit}
			wantErr := archive.TarWith(t.Context(), serial, src, serialReadAhead)
			require.Error(t, wantErr)

			for _, readAhead := range readAheadConfigs {
				readAheadOut := &failingWriter{limit: test.limit}
				gotErr := archive.TarWith(t.Context(), readAheadOut, src, readAhead.config)
				require.Error(t, gotErr, readAhead.name)
				assert.Equal(t, wantErr.Error(), gotErr.Error(), readAhead.name)
				assert.True(t, bytes.Equal(serial.buf.Bytes(), readAheadOut.buf.Bytes()), "%s: output before the error differs", readAhead.name)
			}
		})
	}
}

// cancellingWriter cancels the run once limit bytes have been written.
type cancellingWriter struct {
	mu      sync.Mutex
	written int
	limit   int
	cancel  context.CancelFunc
}

func (w *cancellingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.written += len(p)
	if w.written >= w.limit {
		w.cancel()
	}

	return len(p), nil
}

// TestTarReadAheadCancelled checks cancellation mid-stream stops the run and
// leaves no read-ahead goroutines or open files behind. Not parallel: it
// counts goroutines and fds.
func TestTarReadAheadCancelled(t *testing.T) {
	src := t.TempDir()
	buildMixedTree(t, src)

	baseline := runtime.NumGoroutine()
	baselineFDs := openFDs(t)

	for _, readAhead := range readAheadConfigs {
		for _, limit := range []int{1, 100_000, 2_000_000} {
			ctx, cancel := context.WithCancel(t.Context())
			writer := &cancellingWriter{limit: limit, cancel: cancel}

			err := archive.TarWith(ctx, writer, src, readAhead.config)
			require.ErrorIs(t, err, context.Canceled, "%s, limit %d", readAhead.name, limit)
			assert.Equal(t, baselineFDs, openFDs(t), "%s, limit %d: files left open", readAhead.name, limit)

			cancel()
		}
	}

	// Polled inline: assert.Eventually's own goroutine would skew the count.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	assert.LessOrEqual(t, runtime.NumGoroutine(), baseline, "read-ahead goroutines leaked")
}

// fdCountingWriter records the most fds open during any write, pausing on the
// first so the read-ahead runs as far ahead as it can.
type fdCountingWriter struct {
	t       *testing.T
	writes  int
	maxFDs  int
	pauseOn int
}

func (w *fdCountingWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.pauseOn {
		time.Sleep(200 * time.Millisecond)
	}

	w.maxFDs = max(w.maxFDs, openFDs(w.t))

	return len(p), nil
}

// TestTarReadAheadBoundsOpenFiles checks handed-back sparse files count against
// the worker limit, so read-ahead never holds more than Workers files open.
// Not parallel: it counts fds.
func TestTarReadAheadBoundsOpenFiles(t *testing.T) {
	src := t.TempDir()
	writeSparseFixture(t, filepath.Join(src, "sparse0000"), 48<<10, []dataExtent{{offset: 0, data: []byte("head")}})

	for index := 1; index < 200; index++ {
		writeSparse(t, filepath.Join(src, fmt.Sprintf("sparse%04d", index)), 48<<10, []dataExtent{{offset: 0, data: []byte("head")}})
	}

	config := archive.ReadAhead{Workers: 4, MaxFile: 1 << 20, Memory: 64 << 20, Window: 1024}
	baseline := openFDs(t)
	writer := &fdCountingWriter{t: t, pauseOn: 1}

	require.NoError(t, archive.TarWith(t.Context(), writer, src, config))
	// Workers' files plus one directory or file the walker may hold open.
	assert.LessOrEqual(t, writer.maxFDs, baseline+config.Workers+1)
}

func openFDs(t *testing.T) int {
	t.Helper()

	entries, err := os.ReadDir("/dev/fd")
	require.NoError(t, err)

	return len(entries)
}

func TestTarReadAheadCancelledBeforeStart(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	buildMixedTree(t, src)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := archive.TarWith(ctx, io.Discard, src, archive.DefaultReadAhead)
	require.ErrorIs(t, err, context.Canceled)
}

// BenchmarkTarSmallFiles compares the serial and read-ahead paths on a tree of
// small files. Set TAPE_ARCHIVER_BENCH_TREE to an existing tree (e.g. on a
// spinning-disk pool, with caches dropped between runs) to measure real seek
// cost; the default temp tree is page-cache hot and shows only overhead.
func BenchmarkTarSmallFiles(b *testing.B) {
	src := os.Getenv("TAPE_ARCHIVER_BENCH_TREE")
	if src == "" {
		src = b.TempDir()
		random := rand.New(rand.NewPCG(428, 2))

		for index := range 4000 {
			dir := filepath.Join(src, fmt.Sprintf("dir%02d", index%40))
			require.NoError(b, os.MkdirAll(dir, 0o755))

			content := make([]byte, 8<<10+random.IntN(56<<10))
			for offset := range content {
				content[offset] = byte(random.UintN(256))
			}

			require.NoError(b, os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%05d", index)), content, 0o644))
		}
	}

	for _, readAhead := range []struct {
		name   string
		config archive.ReadAhead
	}{
		{name: "serial", config: serialReadAhead},
		{name: "readahead", config: archive.DefaultReadAhead},
	} {
		b.Run(readAhead.name, func(b *testing.B) {
			var counter countingWriter

			for b.Loop() {
				require.NoError(b, archive.TarWith(b.Context(), &counter, src, readAhead.config))
			}

			b.SetBytes(counter.n / int64(b.N))
		})
	}
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))

	return len(p), nil
}
