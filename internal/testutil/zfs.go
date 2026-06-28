package testutil

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
)

const (
	// EnvPoolMount overrides the ZFS pool mountpoint used by integration tests.
	EnvPoolMount = "TAPE_POOL_MOUNT"
	// EnvPoolDataset overrides the ZFS dataset name used by integration tests.
	EnvPoolDataset = "TAPE_POOL_DATASET"
	// EnvTestSnapshot names a snapshot that exists under the test dataset, for
	// exercising the positive read paths. Empty when unset.
	EnvTestSnapshot = "TAPE_TEST_SNAPSHOT"
	// EnvTestMinBytes is the minimum logicalreferenced byte count the test
	// dataset/snapshot is known to hold, used to assert a meaningful value.
	EnvTestMinBytes = "TAPE_TEST_MIN_BYTES"

	// Defaults target the ephemeral, file-backed pool created by "make
	// zpool-up" (see scripts/zpool-up.sh), NOT the production pool. Tests are
	// read-only, but defaulting to a throwaway pool keeps them deterministic and
	// hermetic and avoids silently reaching for live data. To run against the
	// real pool deliberately, set TAPE_POOL_MOUNT=/mnt/bulk-pool-01 (and the
	// matching TAPE_POOL_DATASET). These mirror the zpool-up.sh defaults.
	defaultPoolMount   = "/mnt/tape-test-pool/archive"
	defaultPoolDataset = "tape_test/archive"
)

// PoolMount returns the ZFS dataset mountpoint whose .zfs/snapshot directory the
// tests read, preferring TAPE_POOL_MOUNT and falling back to the "make zpool-up"
// test pool. "make test-integration" sets it explicitly.
func PoolMount(t *testing.T) string {
	t.Helper()

	if mount := os.Getenv(EnvPoolMount); mount != "" {
		return mount
	}

	return defaultPoolMount
}

// PoolDataset returns the ZFS dataset name to query, preferring TAPE_POOL_DATASET
// and falling back to the "make zpool-up" test dataset. It is a full dataset path
// (pool/dataset), not derivable from the mountpoint, so it has its own default.
func PoolDataset(t *testing.T) string {
	t.Helper()

	if dataset := os.Getenv(EnvPoolDataset); dataset != "" {
		return dataset
	}

	return defaultPoolDataset
}

// TestSnapshot returns the short name of a snapshot known to exist under the
// test dataset (from TAPE_TEST_SNAPSHOT), or "" when unset.
func TestSnapshot(t *testing.T) string {
	t.Helper()

	return os.Getenv(EnvTestSnapshot)
}

// TestMinBytes returns the minimum logicalreferenced byte count the test
// dataset is known to hold (from TAPE_TEST_MIN_BYTES), or 0 when unset or
// unparseable.
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

// SkipIfPoolUnavailable skips the test when the ZFS pool is not mounted at the
// PoolMount path. Integration tests that read snapshots or dataset properties
// require a mounted pool; without it (e.g. CI, isolated dev) the test is skipped
// rather than failed. Run "make test-integration", which creates an ephemeral
// file-backed pool via "make zpool-up".
func SkipIfPoolUnavailable(t *testing.T) {
	t.Helper()

	mount := PoolMount(t)
	if info, err := os.Stat(mount); err != nil || !info.IsDir() {
		t.Skipf("ZFS pool not available at %s"+
			" (run 'make zpool-up' or set %s)", mount, EnvPoolMount)
	}
}

// SkipIfZFSUnavailable skips the test when the ZFS userspace tooling or kernel
// support is absent: the zfs binary must be on PATH and /dev/zfs must exist.
// Tests that shell out to zfs (e.g. reading logicalreferenced) use this.
func SkipIfZFSUnavailable(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("zfs"); err != nil {
		t.Skip("zfs binary not available on PATH (run within 'nix develop')")
	}

	if _, err := os.Stat("/dev/zfs"); err != nil {
		t.Skip("/dev/zfs not present — ZFS kernel module not loaded")
	}
}
