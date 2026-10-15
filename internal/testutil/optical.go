package testutil

import (
	"os"
	"os/exec"
	"testing"
)

// OpticalBurnDevEnv names a real burner device for the write-once medium states a
// file-backed pseudo-disc cannot represent.
const OpticalBurnDevEnv = "OPTICAL_BURN_DEV"

// SkipIfXorrisoUnavailable skips the test when xorriso is not on PATH.
func SkipIfXorrisoUnavailable(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skipf("xorriso not available: not found on PATH"+
			" (run inside 'nix develop' so the xorriso package is present): %v", err)
	}
}

// SkipIfLosetupUnavailable skips the test unless mount/umount exist and the
// process is root, as mounting an ISO 9660 read-back requires.
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

// OpticalBurnDev returns OPTICAL_BURN_DEV, skipping the test when unset.
func OpticalBurnDev(t *testing.T) string {
	t.Helper()

	dev := os.Getenv(OpticalBurnDevEnv)
	if dev == "" {
		t.Skipf("%s not set: skipping real-hardware optical assertion"+
			" (a file-backed stdio disc cannot represent write-once media states)", OpticalBurnDevEnv)
	}

	return dev
}
