#!/usr/bin/env bash
# web-dev-down.sh — stop everything scripts/web-dev-up.sh started directly
# (the local OIDC provider, control/data workers, any still-running
# sample-run seeding pass, and — after a crash/SIGKILL of web-dev-up.sh
# itself — an orphaned cmd/web) and remove its state directory.
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

  # sudo, and the whole process GROUP (pkill --pgroup), not a bare
  # single-PID kill:
  #
  # 1. worker-control and worker-data run under sudo (web-dev-up.sh — their
  #    activities need root for real zfs/zpool commands), so an unprivileged
  #    `kill` against them fails with "Operation not permitted" and silently
  #    leaves them running. sudo can signal a user-owned process
  #    (webdevoidc/webdevseed) exactly as well as a root-owned one, so using
  #    it uniformly here is correct for all four daemons.
  # 2. $pid here is only ever the PID setsid itself reports via bash's $! —
  #    for a "setsid sudo env ... worker &" launch, that's sudo's own PID,
  #    not the real worker process sudo forks. A single-PID SIGKILL against
  #    sudo kills sudo before it gets a chance to forward anything to its
  #    child, orphaning the real (root-owned) worker process — it keeps
  #    running, un-managed, even though this function reports success.
  #    Signaling the whole process group (setsid gives the group the same ID
  #    as the leader PID recorded here) reaches that child too, regardless of
  #    how many fork/exec layers sudo introduces.
  #
  # Not `kill -- "-$pid"` (the traditional POSIX "negative PID = process
  # group" kill(2) target): confirmed the hard way that util-linux's kill
  # binary (pkgs.util-linux, this repo's own setsid dependency) does not
  # support it — it parses a leading "-" as an option/name, not a negative
  # PID, and always reports "cannot find process". pkill --pgroup (procps,
  # pkgs.procps — see flake.nix) is the reliable, portable way to do this
  # instead; -0/-TERM/-KILL below are pkill's --signal values, not kill(1)'s.
  if ! sudo pgrep -g "$pid" > /dev/null 2>&1; then
    echo "==> $name (pid $pid) already stopped"
    rm -f "$pidfile"
    return 0
  fi

  echo "==> stopping $name (pid $pid)..."
  sudo pkill --pgroup "$pid" --signal TERM 2> /dev/null || true

  for _ in $(seq 1 20); do
    sudo pgrep -g "$pid" > /dev/null 2>&1 || break
    sleep 0.5
  done

  if sudo pgrep -g "$pid" > /dev/null 2>&1; then
    echo "==> $name (pid $pid) did not exit in time; sending SIGKILL"
    sudo pkill --pgroup "$pid" --signal KILL 2> /dev/null || true
  fi

  rm -f "$pidfile"
}

# stop_web reaps an orphaned cmd/web via $WEB_DEV_STATE_DIR/web.pid — the
# pidfile web-dev-up.sh writes purely for this crash-remedy path. On the
# normal interrupt path web-dev-up.sh has already waited for (or SIGKILLed)
# cmd/web itself before this script runs, so this finds it already stopped;
# it only matters when web-dev-up.sh and make were SIGKILLed (untrappable)
# and cmd/web survived as an orphan still holding the web port.
#
# Deliberately NOT stop_daemon: cmd/web is started as a plain background job
# of web-dev-up.sh (no setsid — it must stay in the foreground process group
# to receive a real Ctrl+C), so its PGID is the `make web-dev` process
# group's, not its own PID. stop_daemon's pgrep -g/pkill --pgroup semantics
# would probe/kill whatever unrelated process happens to own that (possibly
# recycled) process-group ID by now. A plain single-PID kill is correct
# here, and no sudo is needed — cmd/web runs as the invoking user.
stop_web() {
  local pidfile="$WEB_DEV_STATE_DIR/web.pid"

  [ -f "$pidfile" ] || return 0

  local pid
  pid="$(cat "$pidfile")"

  if ! kill -0 "$pid" 2> /dev/null; then
    echo "==> web (pid $pid) already stopped"
    rm -f "$pidfile"
    return 0
  fi

  echo "==> stopping web (pid $pid)..."
  kill -TERM "$pid" 2> /dev/null || true

  # cmd/web's post-signal lifetime has been observed at 25-70s (see
  # web-dev-up.sh's grace-period comment), so this waits longer than
  # stop_daemon's 10s before escalating.
  for _ in $(seq 1 160); do
    kill -0 "$pid" 2> /dev/null || break
    sleep 0.5
  done

  if kill -0 "$pid" 2> /dev/null; then
    echo "==> web (pid $pid) did not exit in time; sending SIGKILL"
    kill -KILL "$pid" 2> /dev/null || true
  fi

  rm -f "$pidfile"
}

# Order does not matter for correctness (each is an independent process), but
# stopping the seeder first avoids it logging a burst of Temporal errors as
# the workers it depends on disappear out from under it.
stop_web
stop_daemon webdevseed
stop_daemon worker-control
stop_daemon worker-data
stop_daemon webdevoidc

echo "==> removing $WEB_DEV_STATE_DIR"
# sudo: worker-data (running as root — see web-dev-up.sh) writes real staging
# files (archives, PAR2 volumes, the PDF report) under here as root, which an
# unprivileged rm cannot remove.
sudo rm -rf "$WEB_DEV_STATE_DIR"

echo "==> web dev stack (OIDC provider + workers) is down."
