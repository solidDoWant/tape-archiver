package checksum_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/checksum"
)

// Known SHA-256 vectors (lowercase hex).
const (
	emptyDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	abcDigest   = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
)

// writeFile creates a file under a fresh temp dir with the given contents and
// returns its path.
func writeFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	return path
}

func TestSHA256File(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		contents       string
		missing        bool
		expectedDigest string
		assertError    require.ErrorAssertionFunc
	}{
		{
			name:           "empty file",
			contents:       "",
			expectedDigest: emptyDigest,
		},
		{
			name:           "known string",
			contents:       "abc",
			expectedDigest: abcDigest,
		},
		{
			name:        "missing file",
			missing:     true,
			assertError: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assertError := test.assertError
			if assertError == nil {
				assertError = require.NoError
			}

			path := filepath.Join(t.TempDir(), "does-not-exist")
			if !test.missing {
				path = writeFile(t, test.contents)
			}

			digest, err := checksum.SHA256File(path)
			assertError(t, err)

			if test.assertError == nil {
				assert.Equal(t, test.expectedDigest, digest)
			}
		})
	}
}

func TestVerify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contents    string
		expected    string
		missing     bool
		assertError require.ErrorAssertionFunc
	}{
		{
			name:     "matching digest",
			contents: "abc",
			expected: abcDigest,
		},
		{
			name:        "mismatching digest",
			contents:    "abc",
			expected:    emptyDigest,
			assertError: require.Error,
		},
		{
			name:        "missing file",
			missing:     true,
			expected:    abcDigest,
			assertError: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assertError := test.assertError
			if assertError == nil {
				assertError = require.NoError
			}

			path := filepath.Join(t.TempDir(), "does-not-exist")
			if !test.missing {
				path = writeFile(t, test.contents)
			}

			assertError(t, checksum.Verify(path, test.expected))
		})
	}
}
