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
	"github.com/solidDoWant/tape-archiver/pkg/checksum"
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
			name:      "input exactly one slice",
			dataLen:   256,
			sliceSize: 256,
			wantSizes: []int64{256},
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

			slices, err := archive.Split(t.Context(), bytes.NewReader(data), test.sliceSize, dir, "archive")
			test.assertErr(t, err)

			if err != nil {
				assert.Nil(t, slices)

				return
			}

			require.Len(t, slices, len(test.wantSizes))

			var reconstructed []byte

			for index, slice := range slices {
				assert.Equal(t, filepath.Join(dir, "archive."+pad(index)), slice.Path)
				assert.Equal(t, test.wantSizes[index], slice.SizeBytes)
				assertDigestMatchesFile(t, slice)

				contents, readErr := os.ReadFile(slice.Path)
				require.NoError(t, readErr)
				assert.Len(t, contents, int(test.wantSizes[index]))

				reconstructed = append(reconstructed, contents...)
			}

			assert.True(t, bytes.Equal(data, reconstructed),
				"concatenated slices must reconstruct the input")

			remaining, readErr := os.ReadDir(dir)
			require.NoError(t, readErr)
			assert.Len(t, remaining, len(slices))
		})
	}
}

// TestSplitUniformWidthBeyond999 guards #138: mixed three/four-digit suffixes
// sort archive.1000 before archive.101, scrambling the recovery glob's
// reassembly.
func TestSplitUniformWidthBeyond999(t *testing.T) {
	t.Parallel()

	const sliceCount = 1005 // > 999, forcing a four-digit uniform suffix

	data := make([]byte, sliceCount)
	for index := range data {
		data[index] = byte(index % 251) // non-repeating pattern across slices
	}

	dir := t.TempDir()

	slices, err := archive.Split(t.Context(), bytes.NewReader(data), 1, dir, "archive")
	require.NoError(t, err)
	require.Len(t, slices, sliceCount)

	paths := make([]string, 0, len(slices))

	for index, slice := range slices {
		assert.Equal(t, filepath.Join(dir, fmt.Sprintf("archive.%04d", index)), slice.Path)
		assert.Equal(t, int64(1), slice.SizeBytes)
		// The digest must follow its slice through the width-normalizing rename.
		assertDigestMatchesFile(t, slice)

		paths = append(paths, slice.Path)
	}

	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	assert.Equal(t, paths, sorted, "lexical filename order must equal slice order")

	// Lexical glob order mirrors the recovery procedure's sorted reassembly.
	matches, err := filepath.Glob(filepath.Join(dir, "archive.[0-9]*"))
	require.NoError(t, err)
	require.Len(t, matches, sliceCount)

	var reconstructed []byte

	for _, match := range matches {
		contents, readErr := os.ReadFile(match)
		require.NoError(t, readErr)

		reconstructed = append(reconstructed, contents...)
	}

	assert.True(t, bytes.Equal(data, reconstructed),
		"documented sorted reassembly must reconstruct the input")
}

// TestSplitCancelledMidSlice: only per-read cancellation lets Split return
// before the source's blocking second read.
func TestSplitCancelledMidSlice(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	const sliceSize = 1 << 20

	src := &cancelThenBlockReader{cancel: cancel, chunk: make([]byte, 64<<10)}

	type result struct {
		slices []archive.Slice
		err    error
	}

	done := make(chan result, 1)

	go func() {
		slices, err := archive.Split(ctx, src, sliceSize, t.TempDir(), "archive")
		done <- result{slices: slices, err: err}
	}()

	select {
	case got := <-done:
		require.ErrorIs(t, got.err, context.Canceled)
		assert.Nil(t, got.slices)
	case <-time.After(5 * time.Second):
		t.Fatal("Split did not return after cancellation; per-read cancellation is not working")
	}
}

// cancelThenBlockReader yields one chunk and cancels, then blocks forever.
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

// assertDigestMatchesFile: Split's inline digest must equal an independent
// re-read of the slice on disk.
func assertDigestMatchesFile(t *testing.T, slice archive.Slice) {
	t.Helper()

	want, err := checksum.SHA256File(slice.Path)
	require.NoError(t, err)
	assert.Equal(t, want, slice.SHA256, "digest of %s", slice.Path)
}

func pad(index int) string {
	digits := []byte{
		byte('0' + index/100%10),
		byte('0' + index/10%10),
		byte('0' + index%10),
	}

	return string(digits)
}
