package archive_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/archive"
)

func TestSplit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		dataLen   int
		sliceSize int64
		wantSizes []int64 // expected size of each slice, in order
		assertErr require.ErrorAssertionFunc
	}{
		{
			name:      "remainder slice",
			dataLen:   1000,
			sliceSize: 256,
			wantSizes: []int64{256, 256, 256, 232},
		},
		{
			name:      "exact multiple leaves no trailing empty slice",
			dataLen:   512,
			sliceSize: 256,
			wantSizes: []int64{256, 256},
		},
		{
			name:      "input smaller than slice size",
			dataLen:   100,
			sliceSize: 256,
			wantSizes: []int64{100},
		},
		{
			name:      "empty input produces no slices",
			dataLen:   0,
			sliceSize: 256,
			wantSizes: nil,
		},
		{
			name:      "zero slice size is rejected",
			dataLen:   10,
			sliceSize: 0,
			assertErr: require.Error,
		},
		{
			name:      "negative slice size is rejected",
			dataLen:   10,
			sliceSize: -1,
			assertErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			data := make([]byte, test.dataLen)
			for index := range data {
				data[index] = byte(index % 251) // non-repeating pattern across slices
			}

			dir := t.TempDir()

			paths, err := archive.Split(t.Context(), bytes.NewReader(data), test.sliceSize, dir, "archive")
			test.assertErr(t, err)

			if err != nil {
				assert.Nil(t, paths)

				return
			}

			require.Len(t, paths, len(test.wantSizes))

			var reconstructed []byte

			for index, path := range paths {
				assert.Equal(t, filepath.Join(dir, "archive."+pad(index)), path)

				slice, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				assert.Len(t, slice, int(test.wantSizes[index]))

				reconstructed = append(reconstructed, slice...)
			}

			assert.True(t, bytes.Equal(data, reconstructed),
				"concatenated slices must reconstruct the input")

			// No stray files beyond the returned slices.
			remaining, readErr := os.ReadDir(dir)
			require.NoError(t, readErr)
			assert.Len(t, remaining, len(paths))
		})
	}
}

// TestSplitUniformWidthBeyond999 exercises Split past the 999-slice boundary
// where the suffix width rolls from three to four digits. It proves the fix for
// issue #138: every slice in the archive shares a single uniform suffix width,
// so lexical filename order equals numeric slice order and the recovery
// procedure's documented reassembly glob (archive.[0-9]*) reconstructs the input
// byte-for-byte. Slice size 1 over 1005 bytes yields 1005 slices (indices
// 0…1004), forcing a four-digit uniform pad. Without the fix, mixed three/four
// digit names (archive.999, archive.1000) sort archive.1000 before archive.101,
// scrambling the stream.
func TestSplitUniformWidthBeyond999(t *testing.T) {
	t.Parallel()

	const sliceCount = 1005 // > 999, forcing a four-digit uniform suffix

	data := make([]byte, sliceCount)
	for index := range data {
		data[index] = byte(index % 251) // non-repeating pattern across slices
	}

	dir := t.TempDir()

	paths, err := archive.Split(t.Context(), bytes.NewReader(data), 1, dir, "archive")
	require.NoError(t, err)
	require.Len(t, paths, sliceCount)

	// Every returned path carries a uniform four-digit suffix.
	for index, path := range paths {
		assert.Equal(t, filepath.Join(dir, fmt.Sprintf("archive.%04d", index)), path)
	}

	// AC2: sorting the filenames lexically yields the same order Split returns.
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	assert.Equal(t, paths, sorted, "lexical filename order must equal slice order")

	// AC1: the documented recovery reassembly — the shell's lexical expansion of
	// `cat archive.[0-9]*`, mirrored by filepath.Glob's lexical match order —
	// reconstructs the input byte-for-byte.
	matches, err := filepath.Glob(filepath.Join(dir, "archive.[0-9]*"))
	require.NoError(t, err)
	require.Len(t, matches, sliceCount)

	var reconstructed []byte

	for _, match := range matches {
		slice, readErr := os.ReadFile(match)
		require.NoError(t, readErr)

		reconstructed = append(reconstructed, slice...)
	}

	assert.True(t, bytes.Equal(data, reconstructed),
		"documented archive.[0-9]* reassembly must reconstruct the input")
}

// TestSplitCancelledMidSlice confirms Split stops when the context is cancelled
// partway through copying a single slice, rather than running the copy to
// completion. The source yields one chunk, cancels, then blocks forever on any
// further read — modeling a large or slow source. Only per-read cancellation
// lets Split return without reaching that blocking read; without it the copy
// would hang past the timeout.
func TestSplitCancelledMidSlice(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	// sliceSize exceeds the first chunk, so the copy needs a second read.
	const sliceSize = 1 << 20

	src := &cancelThenBlockReader{cancel: cancel, chunk: make([]byte, 64<<10)}

	type result struct {
		paths []string
		err   error
	}

	done := make(chan result, 1)

	go func() {
		paths, err := archive.Split(ctx, src, sliceSize, t.TempDir(), "archive")
		done <- result{paths: paths, err: err}
	}()

	select {
	case got := <-done:
		require.ErrorIs(t, got.err, context.Canceled)
		assert.Nil(t, got.paths)
	case <-time.After(5 * time.Second):
		t.Fatal("Split did not return after cancellation; per-read cancellation is not working")
	}
}

// cancelThenBlockReader yields one chunk and cancels its context on the first
// read, then blocks forever on every subsequent read. A reader of this shape is
// only escaped by checking the context before each read.
type cancelThenBlockReader struct {
	cancel  context.CancelFunc
	chunk   []byte
	yielded bool
}

func (r *cancelThenBlockReader) Read(p []byte) (int, error) {
	if !r.yielded {
		r.yielded = true
		n := copy(p, r.chunk)
		r.cancel()

		return n, nil
	}

	select {} // unreachable unless the context is not checked before reading
}

// pad renders a slice index as the three-digit suffix Split uses.
func pad(index int) string {
	digits := []byte{
		byte('0' + index/100%10),
		byte('0' + index/10%10),
		byte('0' + index%10),
	}

	return string(digits)
}
