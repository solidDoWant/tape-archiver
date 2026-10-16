//go:build integration

package ltfs_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
)

// TestWrittenTapeHasUsableVCI: after the project's format/mount/write/unmount, a
// read-only ltfs mount must find usable volume coherency info in MAM and skip
// the full medium consistency check (#426).
func TestWrittenTapeHasUsableVCI(t *testing.T) {
	volume := loadFormattedVolume(t)

	mount, err := volume.Mount(t.Context(), filepath.Join(t.TempDir(), "mnt"), filepath.Join(t.TempDir(), "work"))
	require.NoError(t, err, "mount LTFS volume")

	writeFiles(t, mount.Mountpoint(), map[string]string{"manifest.txt": "vci\n"})

	require.NoError(t, mount.Unmount(t.Context()), "unmount LTFS volume")

	output := mountReadOnly(t, testutil.Drive0SgDev(t), filepath.Join(t.TempDir(), "ro"))

	// Anchor the negative checks below: without the mount's own success line in
	// the captured log they would pass vacuously on an empty or redirected log.
	require.Contains(t, output, "LTFS11031I", "ltfs log must record the volume mount; ltfs log:\n%s", output)

	for _, message := range []string{
		"LTFS12062W", // VCI signature unreadable
		"LTFS11016W", // index partition MAM unusable
		"LTFS11017W", // data partition MAM unusable
		"LTFS11026I", // full medium consistency check
	} {
		assert.NotContains(t, output, message, "read-only mount must find usable VCI in MAM; ltfs log:\n%s", output)
	}
}

// mountReadOnly mounts device read-only with the bundled ltfs, unmounts it,
// waits for ltfs to exit, and returns its log.
func mountReadOnly(t *testing.T, device, mountpoint string) string {
	t.Helper()

	output := &bytes.Buffer{}
	cmd := exec.Command("ltfs", "-f", mountpoint, "-o", "ro", "-o", "devname="+device)
	cmd.Stdout = output
	cmd.Stderr = output

	require.NoError(t, os.MkdirAll(mountpoint, 0o755), "create read-only mountpoint")
	require.NoError(t, cmd.Start(), "start read-only ltfs")

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "fusermount", "-u", mountpoint).Run()
	})

	deadline := time.Now().Add(2 * time.Minute)

	for !isMounted(mountpoint) {
		select {
		case err := <-done:
			require.FailNow(t, "ltfs exited before mounting", "%v\n%s", err, output)
		default:
		}

		require.True(t, time.Now().Before(deadline), "read-only ltfs mount timed out:\n%s", output)
		time.Sleep(100 * time.Millisecond)
	}

	out, err := exec.CommandContext(t.Context(), "fusermount", "-u", mountpoint).CombinedOutput()
	require.NoError(t, err, "fusermount -u: %s", out)

	require.NoError(t, <-done, "read-only ltfs exit:\n%s", output)

	return output.String()
}

// isMounted reports whether path's st_dev differs from its parent's.
func isMounted(path string) bool {
	var st, parent syscall.Stat_t
	if syscall.Stat(path, &st) != nil || syscall.Stat(filepath.Dir(path), &parent) != nil {
		return false
	}

	return st.Dev != parent.Dev
}
