package archive_test

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/archive"
)

// entry is a normalized snapshot of one filesystem entry, used to compare the
// extracted tree against the source.
type entry struct {
	mode    os.FileMode // type bits and permission bits
	content []byte      // regular files only
	link    string      // symlink target only
}

func TestTar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, root string)
		assertErr require.ErrorAssertionFunc
	}{
		{
			name: "nested files and directories",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "top.txt"), []byte("top-level contents"))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "sub", "deeper"), 0o755))
				writeFile(t, filepath.Join(root, "sub", "b.bin"), bytes.Repeat([]byte{0x00, 0x01, 0x02}, 4096))
				writeFile(t, filepath.Join(root, "sub", "deeper", "c.txt"), []byte("deep"))
			},
		},
		{
			name: "includes symlinks",
			setup: func(t *testing.T, root string) {
				if runtime.GOOS == "windows" {
					t.Skip("symlink creation is privileged on Windows")
				}

				writeFile(t, filepath.Join(root, "target.txt"), []byte("real file"))
				require.NoError(t, os.Symlink("target.txt", filepath.Join(root, "link.txt")))
			},
		},
		{
			name:  "empty source directory",
			setup: func(t *testing.T, root string) {},
		},
		{
			name: "missing source directory",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.Remove(root))
			},
			assertErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			src := t.TempDir()
			test.setup(t, src)

			var buf bytes.Buffer

			err := archive.Tar(t.Context(), &buf, src)
			test.assertErr(t, err)

			if err != nil {
				return
			}

			dest := t.TempDir()
			extractTar(t, &buf, dest)

			assert.Equal(t, snapshot(t, src), snapshot(t, dest),
				"extracted tree must match the source byte-for-byte")
		})
	}
}

// writeFile writes content to path, creating parent directories as needed.
func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, content, 0o644))
}

// snapshot walks dir and returns a map of relative path to a normalized entry,
// so two trees can be compared for structure, contents, and modes.
func snapshot(t *testing.T, dir string) map[string]entry {
	t.Helper()

	result := make(map[string]entry)

	err := filepath.WalkDir(dir, func(path string, dirEntry os.DirEntry, err error) error {
		require.NoError(t, err)

		rel, relErr := filepath.Rel(dir, path)
		require.NoError(t, relErr)

		if rel == "." {
			return nil
		}

		info, infoErr := dirEntry.Info()
		require.NoError(t, infoErr)

		snap := entry{mode: info.Mode()}

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			snap.link, err = os.Readlink(path)
			require.NoError(t, err)
		case info.Mode().IsRegular():
			snap.content, err = os.ReadFile(path)
			require.NoError(t, err)
		}

		result[filepath.ToSlash(rel)] = snap

		return nil
	})
	require.NoError(t, err)

	return result
}

// extractTar reads a tar stream from r and writes its entries under dest.
func extractTar(t *testing.T, r io.Reader, dest string) {
	t.Helper()

	reader := tar.NewReader(r)

	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}

		require.NoError(t, err)

		target := filepath.Join(dest, filepath.FromSlash(header.Name))

		switch header.Typeflag {
		case tar.TypeDir:
			require.NoError(t, os.MkdirAll(target, os.FileMode(header.Mode)))
		case tar.TypeSymlink:
			require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
			require.NoError(t, os.Symlink(header.Linkname, target))
		case tar.TypeReg:
			require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
			extractRegular(t, reader, target, os.FileMode(header.Mode))
		default:
			t.Fatalf("unexpected tar entry type %q for %s", header.Typeflag, header.Name)
		}
	}
}

// extractRegular writes a single regular-file entry, copying its contents from
// the tar reader and applying its mode.
func extractRegular(t *testing.T, reader *tar.Reader, target string, mode os.FileMode) {
	t.Helper()

	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	require.NoError(t, err)

	_, copyErr := io.Copy(file, reader)

	require.NoError(t, file.Close())
	require.NoError(t, copyErr)
	require.NoError(t, os.Chmod(target, mode))
}
