package archive_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
