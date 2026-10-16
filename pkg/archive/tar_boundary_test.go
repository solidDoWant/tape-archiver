package archive_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/archive"
)

// TestTarSkipsRootZFSControlDir: a dataset root with snapdir=visible lists
// .zfs, whose snapshot/ holds every snapshot; only the live tree is archived.
// A .zfs below the root is ordinary data and is kept.
func TestTarSkipsRootZFSControlDir(t *testing.T) {
	src := t.TempDir()
	writeTreeFile(t, src, "data/file.txt", "live")
	writeTreeFile(t, src, ".zfs/snapshot/daily-1/data/file.txt", "old")
	writeTreeFile(t, src, "data/.zfs/kept.txt", "nested")

	var buf bytes.Buffer
	require.NoError(t, archive.Tar(t.Context(), &buf, src))

	assert.Equal(t, []string{"./", "data/", "data/.zfs/", "data/.zfs/kept.txt", "data/file.txt"}, tarEntryNames(t, &buf))
}

// TestTarMembersSkipsRootZFSControlDir: the same holds for each member root.
func TestTarMembersSkipsRootZFSControlDir(t *testing.T) {
	src := t.TempDir()
	writeTreeFile(t, src, "file.txt", "live")
	writeTreeFile(t, src, ".zfs/snapshot/daily-1/file.txt", "old")

	var buf bytes.Buffer
	require.NoError(t, archive.TarMembers(t.Context(), &buf, []archive.Member{{Subdir: "vol", Dir: src}}))

	assert.Equal(t, []string{"vol/", "vol/file.txt"}, tarEntryNames(t, &buf))
}

func writeTreeFile(t *testing.T, root, name, content string) {
	t.Helper()

	path := filepath.Join(root, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func tarEntryNames(t *testing.T, r io.Reader) []string {
	t.Helper()

	var names []string

	reader := tar.NewReader(r)

	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return names
		}

		require.NoError(t, err)

		names = append(names, header.Name)
	}
}
