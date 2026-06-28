package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	// EnvPoolMount overrides the ZFS pool mountpoint used by integration tests.
	EnvPoolMount = "TAPE_POOL_MOUNT"
	// EnvPoolDataset overrides the ZFS dataset name used by integration tests.
	EnvPoolDataset = "TAPE_POOL_DATASET"

	defaultPoolMount = "/mnt/bulk-pool-01"
)

// PoolMount returns the ZFS pool mountpoint, preferring TAPE_POOL_MOUNT and
// falling back to /mnt/bulk-pool-01 (the storage host's bind-mounted pool, per
// SPEC.md §4.1).
func PoolMount(t *testing.T) string {
	t.Helper()

	if mount := os.Getenv(EnvPoolMount); mount != "" {
		return mount
	}

	return defaultPoolMount
}

// PoolDataset returns the ZFS dataset name to query in integration tests,
// preferring TAPE_POOL_DATASET and falling back to the final path component of
// the pool mountpoint (e.g. /mnt/bulk-pool-01 -> bulk-pool-01).
func PoolDataset(t *testing.T) string {
	t.Helper()

	if dataset := os.Getenv(EnvPoolDataset); dataset != "" {
		return dataset
	}

	return filepath.Base(PoolMount(t))
}

// SkipIfPoolUnavailable skips the test when the ZFS pool is not mounted at the
// PoolMount path. Integration tests that read snapshots or dataset properties
// require the real pool; in environments without it (e.g. CI, isolated dev),
// the test is skipped rather than failed.
func SkipIfPoolUnavailable(t *testing.T) {
	t.Helper()

	mount := PoolMount(t)
	if info, err := os.Stat(mount); err != nil || !info.IsDir() {
		t.Skipf("ZFS pool not mounted: %s not available"+
			" (set %s to the pool mountpoint)", mount, EnvPoolMount)
	}
}
