package testutil

import (
	"os"
	"os/exec"
	"testing"
)

// OpticalBurnDevEnv names the environment variable that points the optical
// integration tests at a real burner/loop device for the medium-state
// transitions a file-backed (stdio) pseudo-disc cannot represent — a genuine
// write-once disc that is appendable or finalized, and Blank's refusal of it.
// When it is unset those real-hardware assertions are skipped.
const OpticalBurnDevEnv = "OPTICAL_BURN_DEV"

// SkipIfXorrisoUnavailable skips the test when the xorriso burn binary is not on
// PATH (run inside `nix develop` so the pinned xorriso is present). pkg/optical
// shells out to xorriso for every disc operation.
func SkipIfXorrisoUnavailable(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skipf("xorriso not available: not found on PATH"+
			" (run inside 'nix develop' so the xorriso package is present): %v", err)
	}
}

// SkipIfLosetupUnavailable skips the test when the mount/umount tooling needed to
// read a burned disc back is absent. The optical read-back mounts an ISO 9660
// filesystem, which needs mount/umount and privilege; the integration target
// runs under sudo (like the LTFS path), and an unprivileged run skips cleanly.
func SkipIfLosetupUnavailable(t *testing.T) {
	t.Helper()

	for _, bin := range []string{"mount", "umount"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("optical read-back not available: %q not found on PATH: %v", bin, err)
		}
	}

	if os.Geteuid() != 0 {
		t.Skip("optical read-back not available: mounting ISO 9660 requires root" +
			" (run via 'make test-integration', which uses sudo)")
	}
}

// OpticalBurnDev returns the real burner/loop device path for the hardware-only
// optical assertions, skipping the test when OPTICAL_BURN_DEV is unset.
func OpticalBurnDev(t *testing.T) string {
	t.Helper()

	dev := os.Getenv(OpticalBurnDevEnv)
	if dev == "" {
		t.Skipf("%s not set: skipping real-hardware optical assertion"+
			" (a file-backed stdio disc cannot represent write-once media states)", OpticalBurnDevEnv)
	}

	return dev
}
