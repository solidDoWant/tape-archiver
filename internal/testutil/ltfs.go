package testutil

import (
	"os"
	"os/exec"
	"testing"
)

// ltfsBinaries are the executables pkg/ltfs shells out to. fusermount is the
// FUSE userspace mount/unmount helper libltfs and Unmount rely on.
var ltfsBinaries = []string{"mkltfs", "ltfs", "fusermount"}

// SkipIfLTFSUnavailable skips the test when the LTFS toolchain or FUSE is not
// usable in this environment: the mkltfs/ltfs/fusermount binaries must be on
// PATH and /dev/fuse must exist. LTFS is a FUSE filesystem (SPEC.md §4.1), so
// without /dev/fuse a mount cannot succeed.
//
// As with the mhvtl helpers, the integration target runs the test binary with
// the privilege needed to drive the drive and mount FUSE ("make
// test-integration"); an unprivileged or under-provisioned run skips cleanly.
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
