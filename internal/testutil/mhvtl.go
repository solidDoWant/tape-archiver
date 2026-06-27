// Package testutil provides shared helpers for integration tests that require
// the mhvtl virtual tape library.
package testutil

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

const (
	EnvChangerDev = "MHVTL_CHANGER_DEV"
	EnvDrive0Dev  = "MHVTL_DRIVE0_DEV"
	EnvDrive1Dev  = "MHVTL_DRIVE1_DEV"

	defaultChangerDev = "/dev/sch0"
	defaultDrive0Dev  = "/dev/nst0"
	defaultDrive1Dev  = "/dev/nst1"
)

// SkipIfMhvtlUnavailable skips the test if the mhvtl virtual library is not
// accessible.  It checks MHVTL_CHANGER_DEV first, then falls back to probing
// /dev/sch0, and verifies that mtx can actually query the changer.
//
// Sending SCSI commands via mtx requires CAP_SYS_RAWIO; this helper uses
// sudo when the current process is not already running as root.
func SkipIfMhvtlUnavailable(t *testing.T) {
	t.Helper()

	dev := ChangerDev(t)
	if _, err := os.Stat(dev); err != nil {
		t.Skipf("mhvtl not available: %s not found"+
			" (run 'make mhvtl-up' or set %s)", dev, EnvChangerDev)
	}

	if err := MtxCommand(t.Context(), dev, "status").Run(); err != nil {
		t.Skipf("mhvtl not accessible: mtx -f %s status: %v"+
			" (run 'make mhvtl-up' to start the virtual library)", dev, err)
	}
}

// ChangerDev returns the changer device path, preferring MHVTL_CHANGER_DEV,
// falling back to /dev/sch0.
func ChangerDev(t *testing.T) string {
	t.Helper()

	if dev := os.Getenv(EnvChangerDev); dev != "" {
		return dev
	}

	return defaultChangerDev
}

// Drive0Dev returns the non-rewinding tape device path for drive 0, preferring
// MHVTL_DRIVE0_DEV, falling back to /dev/nst0.
func Drive0Dev(t *testing.T) string {
	t.Helper()

	if dev := os.Getenv(EnvDrive0Dev); dev != "" {
		return dev
	}

	return defaultDrive0Dev
}

// Drive1Dev returns the non-rewinding tape device path for drive 1, preferring
// MHVTL_DRIVE1_DEV, falling back to /dev/nst1.
func Drive1Dev(t *testing.T) string {
	t.Helper()

	if dev := os.Getenv(EnvDrive1Dev); dev != "" {
		return dev
	}

	return defaultDrive1Dev
}

// MtxCommand returns an exec.Cmd for "mtx -f <dev> <args...>".
// When the current process is not root, it prepends "sudo" so that
// SCSI_IOCTL_SEND_COMMAND (which requires CAP_SYS_RAWIO) succeeds.
func MtxCommand(ctx context.Context, dev string, args ...string) *exec.Cmd {
	cmdArgs := append([]string{"-f", dev}, args...)

	if os.Getuid() != 0 {
		//nolint:gosec // prepending sudo is intentional in the test harness
		return exec.CommandContext(ctx, "sudo", append([]string{"mtx"}, cmdArgs...)...)
	}

	return exec.CommandContext(ctx, "mtx", cmdArgs...)
}
