// Package testutil provides shared helpers for integration tests that require
// external resources: the mhvtl virtual tape library and a mounted ZFS pool.
package testutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	EnvChangerDev  = "MHVTL_CHANGER_DEV"
	EnvDrive0Dev   = "MHVTL_DRIVE0_DEV"
	EnvDrive1Dev   = "MHVTL_DRIVE1_DEV"
	EnvDrive0SgDev = "MHVTL_DRIVE0_SG_DEV"
	EnvDrive1SgDev = "MHVTL_DRIVE1_SG_DEV"

	defaultChangerDev  = "/dev/sch0"
	defaultDrive0Dev   = "/dev/nst0"
	defaultDrive1Dev   = "/dev/nst1"
	defaultDrive0SgDev = "/dev/sg1"
	defaultDrive1SgDev = "/dev/sg2"
)

// SkipIfMhvtlUnavailable skips the test if the mhvtl virtual library is not
// accessible.  It checks MHVTL_CHANGER_DEV first, then falls back to probing
// /dev/sch0, and verifies that mtx can actually query the changer.
//
// Sending SCSI commands via mtx requires CAP_SYS_RAWIO plus device access; when
// the test process lacks it the status query fails and the test is skipped
// rather than failed. Run "make test-integration" (which elevates the test
// binary) to exercise these tests.
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
// MHVTL_DRIVE0_DEV, then resolving by SCSI address, then /dev/nst0.
func Drive0Dev(t *testing.T) string {
	t.Helper()

	return resolveDev(t, EnvDrive0Dev, scsiTapeClass, baseTapeNodeRe, 0, defaultDrive0Dev)
}

// Drive1Dev returns the non-rewinding tape device path for drive 1, preferring
// MHVTL_DRIVE1_DEV, then resolving by SCSI address, then /dev/nst1.
func Drive1Dev(t *testing.T) string {
	t.Helper()

	return resolveDev(t, EnvDrive1Dev, scsiTapeClass, baseTapeNodeRe, 1, defaultDrive1Dev)
}

// Drive0SgDev returns the SCSI generic device path for drive 0 (used by
// sg_logs), preferring MHVTL_DRIVE0_SG_DEV, then resolving by SCSI address,
// then /dev/sg1.
func Drive0SgDev(t *testing.T) string {
	t.Helper()

	return resolveDev(t, EnvDrive0SgDev, scsiGenericClass, baseSgNodeRe, 0, defaultDrive0SgDev)
}

// Drive1SgDev returns the SCSI generic device path for drive 1 (used by
// sg_logs), preferring MHVTL_DRIVE1_SG_DEV, then resolving by SCSI address,
// then /dev/sg2.
func Drive1SgDev(t *testing.T) string {
	t.Helper()

	return resolveDev(t, EnvDrive1SgDev, scsiGenericClass, baseSgNodeRe, 1, defaultDrive1SgDev)
}

// SCSI sysfs classes and the base (non-partition) device-node name patterns.
const (
	scsiTapeClass    = "scsi_tape"
	scsiGenericClass = "scsi_generic"
)

var (
	// baseTapeNodeRe matches the base non-rewinding st node (e.g. nst0) and
	// excludes the per-density/partition aliases (nst0a, nst0l, nst0m, st0).
	baseTapeNodeRe = regexp.MustCompile(`^nst[0-9]+$`)
	// baseSgNodeRe matches an sg node (e.g. sg1).
	baseSgNodeRe = regexp.MustCompile(`^sg[0-9]+$`)
)

// resolveDev returns a device path using, in order of precedence: the env-var
// override, deterministic resolution by SCSI address, then the static fallback.
//
// The kernel assigns st/sg minor numbers (nst0, sg1, …) in probe order, which
// is not guaranteed to match mhvtl's drive numbering. mhvtl's device.conf maps
// the changer to SCSI target 0 and drive N to target N+1 (all on channel 0,
// LUN 0), so we resolve the node whose SCSI address matches rather than
// trusting the minor-number ordering.
func resolveDev(t *testing.T, env, class string, nodeRe *regexp.Regexp, driveIndex int, fallback string) string {
	t.Helper()

	if dev := os.Getenv(env); dev != "" {
		return dev
	}

	if dev, ok := resolveByTarget(class, nodeRe, driveSCSITarget(driveIndex)); ok {
		return dev
	}

	return fallback
}

// driveSCSITarget maps a 0-based mhvtl drive index to its SCSI target number
// (drive 0 -> target 1, drive 1 -> target 2; the changer is target 0).
func driveSCSITarget(driveIndex int) int {
	return driveIndex + 1
}

// resolveByTarget scans /sys/class/<class> for a base device node whose backing
// SCSI device is at channel 0, the given target, LUN 0, and returns its /dev
// path. The boolean reports whether a match was found.
func resolveByTarget(class string, nodeRe *regexp.Regexp, target int) (string, bool) {
	classDir := filepath.Join("/sys/class", class)

	entries, err := os.ReadDir(classDir)
	if err != nil {
		return "", false
	}

	for _, entry := range entries {
		name := entry.Name()
		if !nodeRe.MatchString(name) {
			continue
		}

		link, err := os.Readlink(filepath.Join(classDir, name, "device"))
		if err != nil {
			continue
		}

		channel, tgt, lun, ok := parseHCTL(filepath.Base(link))
		if ok && channel == 0 && tgt == target && lun == 0 {
			return filepath.Join("/dev", name), true
		}
	}

	return "", false
}

// parseHCTL parses a "H:C:T:L" SCSI address (the final path component of a
// sysfs device link) into its channel, target, and LUN.
func parseHCTL(hctl string) (channel, target, lun int, ok bool) {
	parts := strings.Split(hctl, ":")
	if len(parts) != 4 {
		return 0, 0, 0, false
	}

	channel, errC := strconv.Atoi(parts[1])
	target, errT := strconv.Atoi(parts[2])
	lun, errL := strconv.Atoi(parts[3])

	if errC != nil || errT != nil || errL != nil {
		return 0, 0, 0, false
	}

	return channel, target, lun, true
}

// SkipIfDriveNotReady waits until the drive can complete a rewind, then skips
// the test if it never becomes ready within the timeout.
//
// dev must be the non-rewinding st node (e.g. /dev/nst0). A blocking
// "mt -f <dev> rewind" sends a real command down to the drive: it returns
// promptly once the medium is mounted, and blocks while the drive is "becoming
// ready" (so each attempt runs under its own short timeout and is retried).
// This is deliberately not `mt status`, which opens the node O_NONBLOCK and
// returns the st driver's cached state without ever probing the drive.
//
// The skip path covers environments lacking the mhvtl ioctl fix, where a loaded
// drive never finishes becoming ready.
func SkipIfDriveNotReady(t *testing.T, dev string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	var lastErr error

	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err := exec.CommandContext(ctx, "mt", "-f", dev, "rewind").Run()

		cancel()

		if err == nil {
			return
		}

		lastErr = err

		time.Sleep(500 * time.Millisecond)
	}

	t.Skipf("tape drive %s did not become ready within 30s after load"+
		" (kernel/mhvtl ioctl incompatibility — last rewind error: %v)", dev, lastErr)
}

// MtxCommand returns an exec.Cmd for "mtx -f <dev> <args...>".
//
// It mirrors the production code and invokes mtx directly — the integration
// test binary is expected to run with the privilege needed to issue SCSI
// commands (CAP_SYS_RAWIO plus device access). "make test-integration" provides
// this by running the test process under sudo; a non-privileged run simply
// skips via SkipIfMhvtlUnavailable.
func MtxCommand(ctx context.Context, dev string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "mtx", append([]string{"-f", dev}, args...)...)
}
