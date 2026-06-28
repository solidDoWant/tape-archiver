#!/usr/bin/env bash
# zpool-down.sh — destroy the ephemeral ZFS test pool created by zpool-up.sh.
#
# Idempotent: a no-op when the pool does not exist. Requires sudo access.
#
# Tunables (must match zpool-up.sh):
#   ZPOOL_NAME   pool name                (tape_test)
#   ZPOOL_IMG    backing file to remove   (/var/tmp/tape-archiver-test-pool.img)

set -euo pipefail

ZPOOL_NAME="${ZPOOL_NAME:-tape_test}"
ZPOOL_IMG="${ZPOOL_IMG:-/var/tmp/tape-archiver-test-pool.img}"

if ! command -v zpool > /dev/null 2>&1; then
  echo "zpool not in PATH; nothing to tear down"
  exit 0
fi

if sudo zpool list -H -o name "$ZPOOL_NAME" > /dev/null 2>&1; then
  echo "==> Destroying pool '$ZPOOL_NAME'..."
  # -f: succeed even if a dataset is busy (e.g. a cwd left inside a snapshot).
  sudo zpool destroy -f "$ZPOOL_NAME"
else
  echo "pool '$ZPOOL_NAME' not present; zpool-down is a no-op"
fi

echo "==> Removing backing file ${ZPOOL_IMG}..."
sudo rm -f "$ZPOOL_IMG"

echo "==> ZFS test pool is down."
