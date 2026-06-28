#!/usr/bin/env bash
# zpool-up.sh — create an ephemeral, file-backed ZFS pool for integration tests.
#
# This is the ZFS analogue of mhvtl-up.sh: it stands up a throwaway pool so the
# pkg/zfs integration tests exercise real ZFS behaviour (snapshot directories
# and the logicalreferenced property) without depending on the production pool.
#
# ZFS accepts a regular file as a vdev directly, so no loopback device is needed.
# The pool is given a child dataset holding a known-size random payload and a
# snapshot, which the integration tests read back and assert against.
#
# Requires:
#   - zpool / zfs in PATH (from the devShell's pkgs.zfs)
#   - the ZFS kernel module available to the running kernel (loaded here with
#     `modprobe zfs`; provided by the host, not built by this repo)
#   - sudo access (pool creation and module loading are privileged)
#
# Tunables (env, with defaults):
#   ZPOOL_NAME        pool name                     (tape_test)
#   ZPOOL_IMG         backing file for the vdev     (/var/tmp/tape-archiver-test-pool.img)
#   ZPOOL_SIZE        backing file size             (256M)
#   ZPOOL_MOUNT       pool mountpoint               (/mnt/tape-test-pool)
#   ZPOOL_DATASET     child dataset (relative)      (archive)
#   ZPOOL_SNAPSHOT    snapshot short name           (test-snap)
#   ZPOOL_PAYLOAD     payload size written + snapped (8M)
#
# On success it prints the env vars the integration tests consume
# (TAPE_POOL_MOUNT, TAPE_POOL_DATASET, TAPE_TEST_SNAPSHOT, TAPE_TEST_MIN_BYTES).

set -euo pipefail

ZPOOL_NAME="${ZPOOL_NAME:-tape_test}"
ZPOOL_IMG="${ZPOOL_IMG:-/var/tmp/tape-archiver-test-pool.img}"
ZPOOL_SIZE="${ZPOOL_SIZE:-256M}"
ZPOOL_MOUNT="${ZPOOL_MOUNT:-/mnt/tape-test-pool}"
ZPOOL_DATASET="${ZPOOL_DATASET:-archive}"
ZPOOL_SNAPSHOT="${ZPOOL_SNAPSHOT:-test-snap}"
ZPOOL_PAYLOAD="${ZPOOL_PAYLOAD:-8M}"

DATASET="${ZPOOL_NAME}/${ZPOOL_DATASET}"
DATASET_MOUNT="${ZPOOL_MOUNT}/${ZPOOL_DATASET}"

# ---------------------------------------------------------------------------
# Sanity checks
# ---------------------------------------------------------------------------

for cmd in zpool zfs; do
  command -v "$cmd" > /dev/null 2>&1 || {
    echo "error: '$cmd' not found in PATH — run this script from within 'nix develop'" >&2
    exit 1
  }
done

# ---------------------------------------------------------------------------
# Kernel module
# ---------------------------------------------------------------------------

echo "==> Loading ZFS kernel module..."
sudo modprobe zfs || {
  echo "error: failed to load the zfs kernel module." >&2
  echo "       ZFS support must be provided by the host kernel (the storage" >&2
  echo "       host runs ZFS; a NixOS dev VM needs" >&2
  echo "       boot.supportedFilesystems = [ \"zfs\" ])." >&2
  exit 1
}

if [ ! -e /dev/zfs ]; then
  echo "error: /dev/zfs not present after modprobe — ZFS is not usable here" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# (Re)create the pool from a fresh file vdev
# ---------------------------------------------------------------------------

if sudo zpool list -H -o name "$ZPOOL_NAME" > /dev/null 2>&1; then
  echo "==> Pool '$ZPOOL_NAME' already exists; destroying it for a clean slate..."
  sudo zpool destroy -f "$ZPOOL_NAME"
fi

echo "==> Creating ${ZPOOL_SIZE} backing file ${ZPOOL_IMG}..."
sudo rm -f "$ZPOOL_IMG"
sudo truncate -s "$ZPOOL_SIZE" "$ZPOOL_IMG"

echo "==> Creating pool '$ZPOOL_NAME' (mountpoint ${ZPOOL_MOUNT})..."
# -f: accept the file vdev. compression=off so logicalreferenced reflects the
# payload directly. snapdir=visible so .zfs/snapshot is easy to inspect (the
# code reaches it regardless, but this keeps manual debugging simple).
sudo zpool create -f \
  -O compression=off \
  -O snapdir=visible \
  -m "$ZPOOL_MOUNT" \
  "$ZPOOL_NAME" "$ZPOOL_IMG"

# ---------------------------------------------------------------------------
# Child dataset + known payload + snapshot
# ---------------------------------------------------------------------------

echo "==> Creating dataset '$DATASET' and writing ${ZPOOL_PAYLOAD} payload..."
sudo zfs create "$DATASET"
# Incompressible random data so the logicalreferenced byte count is predictable.
sudo dd if=/dev/urandom of="${DATASET_MOUNT}/payload.bin" bs=1M \
  count="${ZPOOL_PAYLOAD%M}" status=none

echo "==> Snapshotting '${DATASET}@${ZPOOL_SNAPSHOT}'..."
sudo zfs snapshot "${DATASET}@${ZPOOL_SNAPSHOT}"

# Make the pool readable by the invoking (non-root) user for ad-hoc runs;
# `make test-integration` runs the test binary as root regardless.
sudo chmod -R a+rX "$ZPOOL_MOUNT" 2>/dev/null || true

# Minimum logicalreferenced the tests can assert: the payload, less 5% slack.
MIN_BYTES=$(( ${ZPOOL_PAYLOAD%M} * 1024 * 1024 * 95 / 100 ))

# ---------------------------------------------------------------------------
# Report
# ---------------------------------------------------------------------------

echo ""
echo "==> ZFS test pool is up:"
sudo zfs list -t all -o name,used,logicalreferenced,mountpoint "$ZPOOL_NAME"
echo ""
echo "==> Integration-test environment:"
echo "    TAPE_POOL_MOUNT=${DATASET_MOUNT}"
echo "    TAPE_POOL_DATASET=${DATASET}"
echo "    TAPE_TEST_SNAPSHOT=${ZPOOL_SNAPSHOT}"
echo "    TAPE_TEST_MIN_BYTES=${MIN_BYTES}"
