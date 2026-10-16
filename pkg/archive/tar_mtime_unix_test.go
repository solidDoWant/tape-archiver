//go:build unix

package archive_test

import (
	"archive/tar"
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/solidDoWant/tape-archiver/pkg/archive"
)

// subsecondMtime has every nanosecond digit set (the #425 source file's mtime),
// so both rounding and truncation are detectable.
var subsecondMtime = time.Unix(1_739_941_425, 615_599_752)

// setTimes sets path's atime and mtime without following a symlink.
func setTimes(t *testing.T, path string, atime, mtime time.Time) {
	t.Helper()

	times := []unix.Timespec{unix.NsecToTimespec(atime.UnixNano()), unix.NsecToTimespec(mtime.UnixNano())}
	require.NoError(t, unix.UtimesNanoAt(unix.AT_FDCWD, path, times, unix.AT_SYMLINK_NOFOLLOW))
}

func lstatMtime(t *testing.T, path string) time.Time {
	t.Helper()

	var stat unix.Stat_t
	require.NoError(t, unix.Lstat(path, &stat))

	return time.Unix(stat.Mtim.Unix())
}

// TestTarPreservesSubsecondMtime: GNU tar, as the recovery procedure runs it,
// restores every entry kind's mtime to the nanosecond.
func TestTarPreservesSubsecondMtime(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("Skipping restore check: no tar binary on PATH to extract with")
	}

	tests := []struct {
		name string
		// build populates src and returns the entry (relative to src) to check.
		build func(t *testing.T, src string) string
	}{
		{
			name: "root",
			build: func(t *testing.T, src string) string {
				writeFile(t, filepath.Join(src, "data", "file.txt"), []byte("contents"))

				return "."
			},
		},
		{
			name: "regular file",
			build: func(t *testing.T, src string) string {
				writeFile(t, filepath.Join(src, "file.txt"), []byte("contents"))

				return "file.txt"
			},
		},
		{
			name: "directory",
			build: func(t *testing.T, src string) string {
				writeFile(t, filepath.Join(src, "dir", "inner.txt"), []byte("contents"))

				return "dir"
			},
		},
		{
			name: "symlink",
			build: func(t *testing.T, src string) string {
				writeFile(t, filepath.Join(src, "target.txt"), []byte("contents"))
				require.NoError(t, os.Symlink("target.txt", filepath.Join(src, "link")))

				return "link"
			},
		},
		{
			name: "hardlink",
			build: func(t *testing.T, src string) string {
				writeFile(t, filepath.Join(src, "original.txt"), []byte("contents"))
				require.NoError(t, os.Link(filepath.Join(src, "original.txt"), filepath.Join(src, "linked.txt")))

				return "linked.txt"
			},
		},
		{
			name: "sparse file",
			build: func(t *testing.T, src string) string {
				writeSparseFixture(t, filepath.Join(src, "disk.img"), 8<<20,
					[]dataExtent{{offset: 0, data: bytes.Repeat([]byte("H"), 64<<10)}})

				return "disk.img"
			},
		},
	}

	configs := []struct {
		name   string
		config archive.ReadAhead
	}{
		{name: "serial", config: serialReadAhead},
		{name: "read-ahead", config: archive.DefaultReadAhead},
	}

	for _, test := range tests {
		for _, config := range configs {
			t.Run(test.name+"/"+config.name, func(t *testing.T) {
				t.Parallel()

				src := t.TempDir()
				path := filepath.Join(src, test.build(t, src))

				setTimes(t, path, subsecondMtime, subsecondMtime)

				if got := lstatMtime(t, path); !got.Equal(subsecondMtime) {
					t.Skipf("Skipping: the test filesystem stored mtime %v for %v, so it cannot hold nanosecond mtimes", got, subsecondMtime)
				}

				archived := tarBytes(t, src, config.config)

				target := t.TempDir()

				cmd := exec.CommandContext(t.Context(), "tar", "-x", "-p", "-f", "-", "-C", target)
				cmd.Stdin = bytes.NewReader(archived)

				output, err := cmd.CombinedOutput()
				require.NoError(t, err, "gnu tar extraction failed: %s", output)

				rel, err := filepath.Rel(src, path)
				require.NoError(t, err)

				got := lstatMtime(t, filepath.Join(target, rel))
				assert.True(t, subsecondMtime.Equal(got), "restored mtime %v, want %v", got, subsecondMtime)
			})
		}
	}
}

// TestTarIgnoresAccessAndChangeTime: archives carry neither atime nor ctime, so
// re-archiving a tree whose atimes and ctimes moved yields identical bytes.
func TestTarIgnoresAccessAndChangeTime(t *testing.T) {
	t.Parallel()

	configs := []struct {
		name   string
		config archive.ReadAhead
	}{
		{name: "serial", config: serialReadAhead},
		{name: "read-ahead", config: archive.DefaultReadAhead},
	}

	for _, config := range configs {
		t.Run(config.name, func(t *testing.T) {
			t.Parallel()

			src := t.TempDir()
			buildMixedTree(t, src)

			first := tarBytes(t, src, config.config)

			reader := tar.NewReader(bytes.NewReader(first))

			for {
				header, err := reader.Next()
				if err == io.EOF {
					break
				}

				require.NoError(t, err)

				assert.True(t, header.AccessTime.IsZero(), "%s carries an atime", header.Name)
				assert.True(t, header.ChangeTime.IsZero(), "%s carries a ctime", header.Name)
			}

			// A new atime also moves every entry's ctime to now.
			later := time.Unix(1_900_000_000, 123_456_789)

			require.NoError(t, filepath.WalkDir(src, func(path string, _ fs.DirEntry, err error) error {
				require.NoError(t, err)
				setTimes(t, path, later, lstatMtime(t, path))

				return nil
			}))

			second := tarBytes(t, src, config.config)

			assert.Equal(t, first, second, "atime/ctime changes must not change the archive")
		})
	}
}
