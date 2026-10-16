//go:build unix

package archive_test

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/archive"
)

// rootFixture is a source root with distinctive mode, mtime and (as root)
// owner, so a restore that drops them is detectable.
type rootFixture struct {
	mode  os.FileMode
	mtime time.Time
	uid   int
	gid   int
}

const fixtureOwner = 2001

func newRootFixture(t *testing.T) (string, rootFixture) {
	t.Helper()

	src := t.TempDir()
	writeFile(t, filepath.Join(src, "data", "file.txt"), []byte("contents"))

	fixture := rootFixture{mode: 0o770, mtime: time.Unix(1_600_000_000, 0), uid: os.Getuid(), gid: os.Getgid()}

	if os.Geteuid() == 0 {
		require.NoError(t, os.Lchown(src, fixtureOwner, fixtureOwner))
		fixture.uid, fixture.gid = fixtureOwner, fixtureOwner
	}

	require.NoError(t, os.Chmod(src, fixture.mode))
	require.NoError(t, os.Chtimes(src, fixture.mtime, fixture.mtime))

	return src, fixture
}

// TestTarArchivesRootEntry: the source root is archived first, with its own
// mode, owner and mtime, on both the serial and read-ahead paths.
func TestTarArchivesRootEntry(t *testing.T) {
	t.Parallel()

	serial := archive.ReadAhead{}

	tests := []struct {
		name     string
		wantName string
		write    func(t *testing.T, w io.Writer, src string) error
	}{
		{
			name:     "tar serial",
			wantName: "./",
			write: func(t *testing.T, w io.Writer, src string) error {
				return archive.TarWith(t.Context(), w, src, serial)
			},
		},
		{
			name:     "tar read-ahead",
			wantName: "./",
			write: func(t *testing.T, w io.Writer, src string) error {
				return archive.TarWith(t.Context(), w, src, archive.DefaultReadAhead)
			},
		},
		{
			name:     "members serial",
			wantName: "vol/",
			write: func(t *testing.T, w io.Writer, src string) error {
				return archive.TarMembersWith(t.Context(), w, []archive.Member{{Subdir: "vol", Dir: src}}, serial)
			},
		},
		{
			name:     "members read-ahead",
			wantName: "vol/",
			write: func(t *testing.T, w io.Writer, src string) error {
				return archive.TarMembersWith(t.Context(), w, []archive.Member{{Subdir: "vol", Dir: src}}, archive.DefaultReadAhead)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			src, fixture := newRootFixture(t)

			var buf bytes.Buffer
			require.NoError(t, test.write(t, &buf, src))

			header, err := tar.NewReader(&buf).Next()
			require.NoError(t, err)

			assert.Equal(t, test.wantName, header.Name)
			assert.Equal(t, byte(tar.TypeDir), header.Typeflag)
			assert.Equal(t, int64(fixture.mode), header.Mode&0o7777)
			assert.Equal(t, fixture.uid, header.Uid)
			assert.Equal(t, fixture.gid, header.Gid)
			assert.True(t, fixture.mtime.Equal(header.ModTime), "mtime %v, want %v", header.ModTime, fixture.mtime)
		})
	}
}

// TestTarRestoresRootMetadata: GNU tar -xp -C TARGET, as the recovery
// procedure runs it, applies the source root's metadata to TARGET.
func TestTarRestoresRootMetadata(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("Skipping restore check: no tar binary on PATH to extract with")
	}

	tests := []struct {
		name   string
		config archive.ReadAhead
	}{
		{name: "serial", config: archive.ReadAhead{}},
		{name: "read-ahead", config: archive.DefaultReadAhead},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			src, fixture := newRootFixture(t)

			var buf bytes.Buffer
			require.NoError(t, archive.TarWith(t.Context(), &buf, src, test.config))

			target := t.TempDir()

			cmd := exec.CommandContext(t.Context(), "tar", "-x", "-p", "-f", "-", "-C", target)
			cmd.Stdin = &buf

			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "gnu tar extraction failed: %s", output)

			info, err := os.Stat(target)
			require.NoError(t, err)

			assert.Equal(t, fixture.mode, info.Mode().Perm())
			assert.True(t, fixture.mtime.Equal(info.ModTime()), "mtime %v, want %v", info.ModTime(), fixture.mtime)

			stat, ok := info.Sys().(*syscall.Stat_t)
			require.True(t, ok)

			// Non-root tar cannot chown; ownership is restored only as root.
			if os.Geteuid() != 0 {
				t.Log("Skipping root owner check: restoring ownership needs root (run this test as root to cover it)")

				return
			}

			assert.Equal(t, fixtureOwner, int(stat.Uid))
			assert.Equal(t, fixtureOwner, int(stat.Gid))
		})
	}
}
