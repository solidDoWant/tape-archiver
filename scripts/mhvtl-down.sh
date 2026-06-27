#!/usr/bin/env bash
# mhvtl-down.sh — stop the mhvtl virtual tape library and unload the module.
#
# Requires sudo access.

set -euo pipefail

MHVTL_CONFIG_DIR="${MHVTL_CONFIG_DIR:-/etc/mhvtl}"
MHVTL_HOME_DIR="${MHVTL_HOME_DIR:-/opt/mhvtl}"

LIBRARY_ID=10
DRIVE0_ID=11
DRIVE1_ID=12

if ! lsmod | grep -q '^mhvtl '; then
  echo "mhvtl module not loaded; mhvtl-down is a no-op"
  exit 0
fi

# ---------------------------------------------------------------------------
# Stop daemons gracefully via vtlcmd, then fall back to SIGTERM
# ---------------------------------------------------------------------------

echo "==> Stopping vtltape and vtllibrary daemons..."

for id in $DRIVE0_ID $DRIVE1_ID $LIBRARY_ID; do
  if command -v vtlcmd > /dev/null 2>&1; then
    # Daemons run as root; vtlcmd must also run as root to reach their IPC queue.
    sudo env PATH="$PATH" vtlcmd "$id" exit 2>/dev/null || true
  fi
done

# Give daemons up to 5 s to exit cleanly before force-killing.
for _ in $(seq 1 10); do
  if ! pgrep -x vtltape > /dev/null 2>&1 && ! pgrep -x vtllibrary > /dev/null 2>&1; then
    break
  fi
  sleep 0.5
done

# Kill any that are still running.
sudo pkill -x vtltape   2>/dev/null || true
sudo pkill -x vtllibrary 2>/dev/null || true
sleep 1

# ---------------------------------------------------------------------------
# Unload kernel modules
# ---------------------------------------------------------------------------

echo "==> Unloading mhvtl kernel module..."
# Do not swallow rmmod failures: a module stuck "in use" (e.g. a daemon was
# killed abnormally and left its SCSI host registered) must fail loudly.
# Otherwise the next `mhvtl-up` sees the module still loaded, no-ops, and the
# tests run against a stale module.
if ! rmmod_err=$(sudo rmmod mhvtl 2>&1); then
  echo "error: failed to unload mhvtl module: ${rmmod_err}" >&2
  echo "       the module may still be in use; check for lingering vtltape/" >&2
  echo "       vtllibrary processes or a wedged SCSI host before retrying." >&2
  exit 1
fi

echo "==> mhvtl virtual library is down."
