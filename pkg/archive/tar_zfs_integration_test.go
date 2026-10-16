//go:build integration

package archive_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/archive"
)

// TestTarLiveDatasetStaysInDataset archives a live dataset with
// snapdir=visible, a snapshot, and a mounted child dataset: only the dataset's
// own live files are archived, and the child's mountpoint is an empty
// directory, as it is in a snapshot of the parent.
func TestTarLiveDatasetStaysInDataset(t *testing.T) {
	testutil.SkipIfZFSUnavailable(t)
	testutil.SkipIfPoolUnavailable(t)

	parent := testutil.CreateEphemeralDataset(t, "tape-archiver-boundary-test")
	child := parent + "/child"

	runZFS(t, "create", child)
	runZFS(t, "set", "snapdir=visible", parent)

	mount := strings.TrimSpace(runZFS(t, "get", "-H", "-o", "value", "mountpoint", parent))

	writeTreeFile(t, mount, "live.txt", "live")
	writeTreeFile(t, mount, "child/inner.txt", "child data")
	runZFS(t, "snapshot", parent+"@daily-1")

	_, err := os.Stat(filepath.Join(mount, ".zfs", "snapshot", "daily-1", "live.txt"))
	require.NoError(t, err, "fixture: the snapshot must be reachable through the visible .zfs directory")

	var buf bytes.Buffer
	require.NoError(t, archive.Tar(t.Context(), &buf, mount))

	assert.Equal(t, []string{"./", "child/", "live.txt"}, tarEntryNames(t, &buf))
}

func runZFS(t *testing.T, args ...string) string {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "zfs", args...).CombinedOutput()
	require.NoErrorf(t, err, "zfs %v: %s", args, out)

	return string(out)
}
