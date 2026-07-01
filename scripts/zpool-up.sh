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
#   - zpool / zfs in PATH (from the devShell's zfsUserspace)
#   - a loadable ZFS kernel module for the running kernel. Inside 'nix develop'
#     the flake builds a version-matched module and exports $ZFS_MODULES, which
#     this script depmods into a temp tree and loads; otherwise (or when the
#     flake build does not match the running kernel) it falls back to the host's
#     own module via `modprobe zfs` (e.g. the storage host's DKMS build).
#   - sudo access (loading a kernel module and creating a pool are privileged;
#     this is inherent to ZFS and cannot be granted by the flake)
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

# load_zfs_module loads ZFS into the running kernel. It prefers the flake-built,
# version-matched module exposed at $ZFS_MODULES (built against the kernel the
# dev VM boots): ZFS is a multi-module dependency graph (spl, zfs, ...), so it
# must be loaded via modprobe, which needs a modules.dep. depmod cannot write
# into the read-only nix store, so the module tree is copied into a temp dir,
# depmod'd there, and loaded with `modprobe -d`. When $ZFS_MODULES is unset or
# built for a different kernel, it falls back to the host's own module.
load_zfs_module() {
  # Probe sysfs, not `lsmod | grep -q`: under `set -o pipefail` that pipeline
  # can die of SIGPIPE (grep -q exits on first match, lsmod is killed mid-write)
  # and falsely report the module as absent, causing a redundant modprobe. See
  # the longer note in mhvtl-up.sh. /sys/module/zfs exists iff zfs is loaded.
  if [ -d /sys/module/zfs ]; then
    echo "==> ZFS kernel module already loaded."
    return 0
  fi

  local kver src tmp
  kver="$(uname -r)"
  src="${ZFS_MODULES:-}/lib/modules/${kver}"

  if [ -n "${ZFS_MODULES:-}" ] && [ -d "$src" ]; then
    echo "==> Loading flake-built ZFS module for ${kver}..."
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' RETURN
    mkdir -p "$tmp/lib/modules/${kver}"
    cp -rL "$src/." "$tmp/lib/modules/${kver}/"
    chmod -R u+w "$tmp/lib/modules/${kver}"
    # Seed the booted kernel's built-in module metadata so depmod does not warn
    # about it (the zfs-kernel store path holds only the out-of-tree modules).
    # Best-effort: the warnings are harmless and the load still succeeds without.
    local booted="/run/booted-system/kernel-modules/lib/modules/${kver}"
    for meta in modules.order modules.builtin modules.builtin.modinfo; do
      [ -f "$booted/$meta" ] && cp "$booted/$meta" "$tmp/lib/modules/${kver}/$meta"
    done
    depmod -b "$tmp" "$kver"
    if sudo modprobe -d "$tmp" zfs; then
      return 0
    fi
    echo "==> flake module failed to load; falling back to host module..." >&2
  fi

  echo "==> Loading host ZFS module via modprobe..."
  sudo modprobe zfs
}

load_zfs_module || {
  echo "error: failed to load the zfs kernel module." >&2
  echo "       Run inside 'nix develop' (so the flake provides a matching" >&2
  echo "       module), or ensure the host provides one (DKMS, or NixOS" >&2
  echo "       boot.extraModulePackages = [ config.boot.kernelPackages.zfs ])." >&2
  exit 1
}

if [ ! -e /dev/zfs ]; then
  echo "error: /dev/zfs not present after loading the module — ZFS is unusable" >&2
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
sudo zfs list -r -t all -o name,used,logicalreferenced,mountpoint "$ZPOOL_NAME"
echo ""
echo "==> Integration-test environment:"
echo "    TAPE_POOL_MOUNT=${DATASET_MOUNT}"
echo "    TAPE_POOL_DATASET=${DATASET}"
echo "    TAPE_TEST_SNAPSHOT=${ZPOOL_SNAPSHOT}"
echo "    TAPE_TEST_MIN_BYTES=${MIN_BYTES}"
