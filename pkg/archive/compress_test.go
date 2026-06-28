package archive_test

import (
	"bytes"
	"context"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/archive"
)

// zstdMagic is the zstd frame magic number (RFC 8878), little-endian.
var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

func TestCompress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts []archive.CompressOption
	}{
		{name: "default level"},
		{name: "explicit low level", opts: []archive.CompressOption{archive.WithLevel(1)}},
		{name: "ultra level", opts: []archive.CompressOption{archive.WithLevel(20)}},
	}

	// A mix of compressible runs and varied bytes, large enough to exercise the
	// streaming path through the zstd subprocess.
	payload := make([]byte, 1<<16)
	for index := range payload {
		payload[index] = byte((index / 64) % 256)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var compressed bytes.Buffer

			err := archive.Compress(t.Context(), &compressed, bytes.NewReader(payload), test.opts...)
			require.NoError(t, err)

			assert.Equal(t, zstdMagic, compressed.Bytes()[:4], "output must be a zstd frame")

			// Decompress with the real zstd binary — the same tool a recoverer
			// uses — and confirm a byte-for-byte round trip.
			roundTripped := decompressWithZstd(t, compressed.Bytes())
			assert.Equal(t, payload, roundTripped)
		})
	}
}

func TestCompressContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := archive.Compress(ctx, &bytes.Buffer{}, bytes.NewReader([]byte("data")))
	require.Error(t, err)
}

// decompressWithZstd decompresses data by invoking the bundled zstd binary.
func decompressWithZstd(t *testing.T, data []byte) []byte {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "zstd", "-d", "-q", "-c")
	cmd.Stdin = bytes.NewReader(data)

	var out bytes.Buffer

	cmd.Stdout = &out

	require.NoError(t, cmd.Run())

	return out.Bytes()
}
