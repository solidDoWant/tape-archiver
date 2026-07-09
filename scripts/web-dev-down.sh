#!/usr/bin/env bash
# web-dev-down.sh — stop everything scripts/web-dev-up.sh started directly
# (the local OIDC provider, control/data workers, and any still-running
# sample-run seeding pass) and remove its state directory.
#
# Temporal/mhvtl/zpool are torn down separately by the `web-dev-down` Makefile
# target via the existing `temporal-down`/`mhvtl-down`/`zpool-down` targets,
# not by this script — mirroring how `web-dev-up.sh` leaves bringing those up
# to `web-dev`'s Make prerequisites.
#
# Idempotent: safe to run when nothing (or only some daemons) are up.

set -uo pipefail

WEB_DEV_STATE_DIR="${WEB_DEV_STATE_DIR:-/var/tmp/tape-archiver-web-dev}"

if [ ! -d "$WEB_DEV_STATE_DIR" ]; then
  echo "web-dev-down: $WEB_DEV_STATE_DIR does not exist; nothing to stop"
  exit 0
fi

stop_daemon() {
  local name="$1"
  local pidfile="$WEB_DEV_STATE_DIR/$name.pid"

  [ -f "$pidfile" ] || return 0

  local pid
  pid="$(cat "$pidfile")"

  if ! kill -0 "$pid" 2> /dev/null; then
    echo "==> $name (pid $pid) already stopped"
    rm -f "$pidfile"
    return 0
  fi

  echo "==> stopping $name (pid $pid)..."
  kill -TERM "$pid" 2> /dev/null || true

  for _ in $(seq 1 20); do
    kill -0 "$pid" 2> /dev/null || break
    sleep 0.5
  done

  if kill -0 "$pid" 2> /dev/null; then
    echo "==> $name (pid $pid) did not exit in time; sending SIGKILL"
    kill -KILL "$pid" 2> /dev/null || true
  fi

  rm -f "$pidfile"
}

# Order does not matter for correctness (each is an independent process), but
# stopping the seeder first avoids it logging a burst of Temporal errors as
# the workers it depends on disappear out from under it.
stop_daemon webdevseed
stop_daemon worker-control
stop_daemon worker-data
stop_daemon webdevoidc

echo "==> removing $WEB_DEV_STATE_DIR"
rm -rf "$WEB_DEV_STATE_DIR"

echo "==> web dev stack (OIDC provider + workers) is down."
