package testutil

import (
	"os"
	"os/exec"
	"testing"
)

var ltfsBinaries = []string{"mkltfs", "ltfs", "fusermount"}

// SkipIfLTFSUnavailable skips the test unless the LTFS binaries are on PATH and
// /dev/fuse exists.
func SkipIfLTFSUnavailable(t *testing.T) {
	t.Helper()

	for _, bin := range ltfsBinaries {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("LTFS not available: %q not found on PATH"+
				" (run inside 'nix develop' so the ltfs package is present): %v", bin, err)
		}
	}

	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("LTFS not available: /dev/fuse not present (FUSE required for ltfs mounts): %v", err)
	}
}
