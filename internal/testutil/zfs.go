package testutil

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

const (
	// EnvPoolMount overrides the ZFS pool mountpoint used by integration tests.
	EnvPoolMount = "TAPE_POOL_MOUNT"
	// EnvPoolDataset overrides the ZFS dataset name used by integration tests.
	EnvPoolDataset = "TAPE_POOL_DATASET"
	// EnvTestSnapshot names a snapshot that exists under the test dataset.
	EnvTestSnapshot = "TAPE_TEST_SNAPSHOT"
	// EnvTestMinBytes is the minimum logicalreferenced bytes the test dataset holds.
	EnvTestMinBytes = "TAPE_TEST_MIN_BYTES"

	// Defaults target the throwaway "make zpool-up" pool, never live data.
	defaultPoolMount   = "/mnt/tape-test-pool/archive"
	defaultPoolDataset = "tape_test/archive"
)

// PoolMount returns TAPE_POOL_MOUNT, or the "make zpool-up" mountpoint.
func PoolMount(t *testing.T) string {
	t.Helper()

	if mount := os.Getenv(EnvPoolMount); mount != "" {
		return mount
	}

	return defaultPoolMount
}

// PoolDataset returns TAPE_POOL_DATASET, or the "make zpool-up" dataset.
func PoolDataset(t *testing.T) string {
	t.Helper()

	if dataset := os.Getenv(EnvPoolDataset); dataset != "" {
		return dataset
	}

	return defaultPoolDataset
}

// TestSnapshot returns TAPE_TEST_SNAPSHOT, or "".
func TestSnapshot(t *testing.T) string {
	t.Helper()

	return os.Getenv(EnvTestSnapshot)
}

// TestMinBytes returns TAPE_TEST_MIN_BYTES, or 0 when unset.
func TestMinBytes(t *testing.T) int64 {
	t.Helper()

	raw := os.Getenv(EnvTestMinBytes)
	if raw == "" {
		return 0
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("invalid %s=%q: %v", EnvTestMinBytes, raw, err)
	}

	return value
}

// SkipIfPoolUnavailable skips the test when no pool is mounted at PoolMount.
func SkipIfPoolUnavailable(t *testing.T) {
	t.Helper()

	mount := PoolMount(t)
	if info, err := os.Stat(mount); err != nil || !info.IsDir() {
		t.Skipf("ZFS pool not available at %s"+
			" (run 'make zpool-up' or set %s)", mount, EnvPoolMount)
	}
}

// CreateEphemeralDataset creates PoolDataset/name, destroyed at test end, and
// returns its full name. Skips when zfs create is not permitted.
func CreateEphemeralDataset(t *testing.T, name string) string {
	t.Helper()

	dataset := PoolDataset(t) + "/" + name

	// Clear any leftover from an interrupted prior run.
	_ = exec.CommandContext(t.Context(), "zfs", "destroy", "-r", dataset).Run()

	if out, err := exec.CommandContext(t.Context(), "zfs", "create", dataset).CombinedOutput(); err != nil {
		t.Skipf("cannot create ephemeral dataset %q (privileged zfs required; run 'make test-integration'): %v: %s",
			dataset, err, strings.TrimSpace(string(out)))
	}

	t.Cleanup(func() {
		_ = exec.Command("zfs", "destroy", "-r", dataset).Run()
	})

	return dataset
}

// UnmountDataset unmounts dataset, failing the test on error.
func UnmountDataset(t *testing.T, dataset string) {
	t.Helper()

	if out, err := exec.CommandContext(t.Context(), "zfs", "unmount", dataset).CombinedOutput(); err != nil {
		t.Fatalf("zfs unmount %q: %v: %s", dataset, err, strings.TrimSpace(string(out)))
	}
}

// SkipIfZFSUnavailable skips the test unless zfs is on PATH and /dev/zfs exists.
func SkipIfZFSUnavailable(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("zfs"); err != nil {
		t.Skip("zfs binary not available on PATH (run within 'nix develop')")
	}

	if _, err := os.Stat("/dev/zfs"); err != nil {
		t.Skip("/dev/zfs not present — ZFS kernel module not loaded")
	}
}
