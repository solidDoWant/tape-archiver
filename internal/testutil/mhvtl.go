// Package testutil provides shared helpers for integration tests that require
// external resources: the mhvtl virtual tape library and a mounted ZFS pool.
package testutil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
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

// SkipIfMhvtlUnavailable skips the test unless the mhvtl changer exists and can
// be queried. Querying needs CAP_SYS_RAWIO, which "make test-integration" grants.
func SkipIfMhvtlUnavailable(t *testing.T) {
	t.Helper()

	dev := ChangerDev(t)
	if _, err := os.Stat(dev); err != nil {
		t.Skipf("mhvtl not available: %s not found"+
			" (run 'make mhvtl-up' or set %s)", dev, EnvChangerDev)
	}

	if _, err := tape.NewChanger(dev).Inventory(t.Context()); err != nil {
		t.Skipf("mhvtl not accessible: changer inventory on %s: %v"+
			" (run 'make mhvtl-up' to start the virtual library)", dev, err)
	}
}

// ChangerDev returns MHVTL_CHANGER_DEV, or /dev/sch0.
func ChangerDev(t *testing.T) string {
	t.Helper()

	if dev := os.Getenv(EnvChangerDev); dev != "" {
		return dev
	}

	return defaultChangerDev
}

// Drive0Dev returns drive 0's non-rewinding tape node.
func Drive0Dev(t *testing.T) string {
	t.Helper()

	return resolveDev(EnvDrive0Dev, scsiTapeClass, baseTapeNodeRe, 0, defaultDrive0Dev)
}

// Drive1Dev returns drive 1's non-rewinding tape node.
func Drive1Dev(t *testing.T) string {
	t.Helper()

	return resolveDev(EnvDrive1Dev, scsiTapeClass, baseTapeNodeRe, 1, defaultDrive1Dev)
}

// ResolveDrive0Dev is Drive0Dev for callers without a *testing.T (TestMain).
func ResolveDrive0Dev() string {
	return resolveDev(EnvDrive0Dev, scsiTapeClass, baseTapeNodeRe, 0, defaultDrive0Dev)
}

// Drive0SgDev returns drive 0's SCSI generic node.
func Drive0SgDev(t *testing.T) string {
	t.Helper()

	return resolveDev(EnvDrive0SgDev, scsiGenericClass, baseSgNodeRe, 0, defaultDrive0SgDev)
}

// Drive1SgDev returns drive 1's SCSI generic node.
func Drive1SgDev(t *testing.T) string {
	t.Helper()

	return resolveDev(EnvDrive1SgDev, scsiGenericClass, baseSgNodeRe, 1, defaultDrive1SgDev)
}

const (
	scsiTapeClass    = "scsi_tape"
	scsiGenericClass = "scsi_generic"
)

var (
	// baseTapeNodeRe excludes the per-density/partition aliases (nst0a, st0, ...).
	baseTapeNodeRe = regexp.MustCompile(`^nst[0-9]+$`)
	// baseSgNodeRe matches an sg node (e.g. sg1).
	baseSgNodeRe = regexp.MustCompile(`^sg[0-9]+$`)
)

// resolveDev prefers the env override, then the node at the drive's SCSI
// address, then fallback. Kernel minor numbers follow probe order, which need
// not match mhvtl's drive numbering, so the SCSI address is authoritative.
func resolveDev(env, class string, nodeRe *regexp.Regexp, driveIndex int, fallback string) string {
	if dev := os.Getenv(env); dev != "" {
		return dev
	}

	if dev, ok := resolveByTarget(class, nodeRe, driveSCSITarget(driveIndex)); ok {
		return dev
	}

	return fallback
}

// driveSCSITarget maps an mhvtl drive index to its SCSI target; the changer is
// target 0 in mhvtl's device.conf.
func driveSCSITarget(driveIndex int) int {
	return driveIndex + 1
}

// resolveByTarget returns the /dev node in /sys/class/`class` backed by the
// SCSI device at 0:target:0.
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

// parseHCTL parses an "H:C:T:L" SCSI address into channel, target, and LUN.
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

// driveReadyTimeout bounds WaitDriveReady.
const driveReadyTimeout = 30 * time.Second

// WaitDriveReady waits up to 30s for a rewind on dev to succeed, returning the
// last rewind error if it never does. It uses rewind rather than `mt status`,
// which reports the st driver's cached state without probing the drive.
func WaitDriveReady(ctx context.Context, dev string) error {
	deadline := time.Now().Add(driveReadyTimeout)

	var lastErr error

	for time.Now().Before(deadline) {
		rewindCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := exec.CommandContext(rewindCtx, "mt", "-f", dev, "rewind").Run()

		cancel()

		if err == nil {
			return nil
		}

		lastErr = err

		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("tape drive %s did not become ready within %s after load: last rewind error: %w",
		dev, driveReadyTimeout, lastErr)
}

// SkipIfDriveNotReady skips the test if the loaded drive never becomes ready
// (environments lacking the mhvtl ioctl fix).
func SkipIfDriveNotReady(t *testing.T, dev string) {
	t.Helper()

	if err := WaitDriveReady(t.Context(), dev); err != nil {
		t.Skipf("%v (kernel/mhvtl ioctl incompatibility)", err)
	}
}

// RequireDriveReady fails the test if the loaded drive never becomes ready. Use
// it once the environment has already proven it can ready a drive, so a drive
// that wedges mid-suite fails instead of silently skipping.
func RequireDriveReady(t *testing.T, dev string) {
	t.Helper()

	require.NoError(t, WaitDriveReady(t.Context(), dev))
}
