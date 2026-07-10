#!/usr/bin/env bash
# web-dev-up.sh — bring up the local `cmd/web` dev stack (issue #265): a local
# OIDC provider, real control + data workers, a background sample-run
# seeding pass, VictoriaLogs/VictoriaMetrics/a log shipper (issue #280), then
# `cmd/web` itself in the foreground.
#
# Temporal/mhvtl are NOT brought up here — the `web-dev` Makefile target
# depends on the existing, genuinely idempotent `temporal-up`/`mhvtl-up`
# targets directly, so this script assumes they are already up when it
# starts. The ZFS test pool is different: zpool-up.sh destroys and recreates
# the pool unconditionally (correct for test-integration/test-e2e, wrong for
# a target meant to be re-run repeatedly across a dev session), so this
# script brings it up itself, but only when it isn't already present — see
# the check below. That check also covers this script's own crash/SIGKILL
# recovery path (a non-goal to change — see issue #268): those cannot be
# trapped, so `web-dev-down` remains the documented remedy, and a subsequent
# `make web-dev` must tolerate whatever they left behind. VictoriaLogs/
# VictoriaMetrics are a third case — genuinely idempotent like temporal/mhvtl,
# but with their own ordering requirement that rules out being a plain
# Makefile prerequisite too; see the dedicated comment further down.
#
# Everything this script itself starts (the OIDC provider, both worker roles,
# and the seeding pass) is detached into its own session via `setsid`, so a
# developer's Ctrl+C — which the terminal only delivers to its foreground
# process group — never reaches them directly. As of issue #268, though, an
# interrupt (SIGINT or SIGTERM) delivered to that foreground process group
# still tears the whole stack down: this script traps the signal, waits for
# the foregrounded `cmd/web` (a background job of this script, not an `exec`)
# to actually exit, then runs the full `web-dev-down` teardown itself — see
# the "Interrupt handling" comment below and docs/web-ui.md's "Local
# development" section.
#
# Idempotent: rerunning this script (e.g. `make web-dev` again after a code
# change + rebuild, or after a crash/SIGKILL left state behind) skips
# starting a daemon that is already running (tracked via PID files under
# $WEB_DEV_STATE_DIR) and just starts cmd/web fresh, matching `temporal-up`'s
# own `--wait`-based idempotency.
#
# Requires (all provided by `nix develop`): age-keygen (webdevseed's sample
# keypair), mkltfs/ltfs/par2/zstd/tar/mt-st/sg3-utils (the data worker's real
# activities), setsid (util-linux), curl, make (to hand off to
# `web-dev-down` on interrupt), docker with the compose plugin (VictoriaLogs/
# VictoriaMetrics/vector, docker-compose.web-dev.yml).

set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

BIN_DIR="${BIN_DIR:-$PROJECT_DIR/bin}"
WEB_DEV_STATE_DIR="${WEB_DEV_STATE_DIR:-/var/tmp/tape-archiver-web-dev}"
LOG_DIR="$WEB_DEV_STATE_DIR/logs"

TEMPORAL_ADDRESS="${TEMPORAL_ADDRESS:-localhost:7233}"

MHVTL_CHANGER_DEV="${MHVTL_CHANGER_DEV:-/dev/sch0}"
MHVTL_DRIVE0_DEV="${MHVTL_DRIVE0_DEV:-/dev/nst0}"
MHVTL_DRIVE1_DEV="${MHVTL_DRIVE1_DEV:-/dev/nst1}"

TAPE_POOL_DATASET="${TAPE_POOL_DATASET:-tape_test/archive}"
TAPE_TEST_SNAPSHOT="${TAPE_TEST_SNAPSHOT:-test-snap}"

WEB_LISTEN_ADDRESS="${WEB_LISTEN_ADDRESS:-:8080}"
WEB_DEV_PORT="${WEB_LISTEN_ADDRESS##*:}"

OIDC_LISTEN_ADDR="${WEBDEVOIDC_LISTEN_ADDR:-127.0.0.1:9998}"
OIDC_CLIENT_ID="${WEBDEVOIDC_CLIENT_ID:-tape-archiver-web-dev}"
OIDC_CLIENT_SECRET="${WEBDEVOIDC_CLIENT_SECRET:-tape-archiver-web-dev-secret}"
OIDC_SUBJECT="${WEBDEVOIDC_SUBJECT:-dev-operator}"
OIDC_EMAIL="${WEBDEVOIDC_EMAIL:-dev-operator@tape-archiver.local}"
OIDC_NAME="${WEBDEVOIDC_NAME:-Dev Operator}"

CONTROL_HEALTH_ADDR=":18080"
CONTROL_METRICS_ADDR=":19090"
DATA_HEALTH_ADDR=":18081"
DATA_METRICS_ADDR=":19091"

# VictoriaLogs/VictoriaMetrics (issue #280, docker-compose.web-dev.yml — both
# run with network_mode: host, so they listen directly on these loopback
# ports). cmd/web does not read these yet (issues #274/#275 add that); they
# are exported to it below anyway so the dev stack is already wired for
# whichever of those two lands first, without another web-dev-up.sh change.
# VICTORIALOGS_STREAM_FILTER's exact semantics are #274's to define; "*" (a
# LogsQL query matching every log line) is the correct default for THIS
# stack specifically because it is single-tenant — this VictoriaLogs
# instance only ever ingests tape-archiver dev-worker logs (see
# scripts/web-dev-vector.yml), so there is nothing else to filter out.
VICTORIALOGS_URL="${VICTORIALOGS_URL:-http://127.0.0.1:9428}"
VICTORIALOGS_STREAM_FILTER="${VICTORIALOGS_STREAM_FILTER:-*}"
VICTORIAMETRICS_URL="${VICTORIAMETRICS_URL:-http://127.0.0.1:8428}"

# ---------------------------------------------------------------------------
# Sanity checks
# ---------------------------------------------------------------------------

for cmd in age-keygen mkltfs setsid curl docker; do
  command -v "$cmd" > /dev/null 2>&1 || {
    echo "error: '$cmd' not found in PATH — run 'make web-dev' from within 'nix develop'" >&2
    exit 1
  }
done

for bin in web worker webdevoidc webdevseed; do
  [ -x "$BIN_DIR/$bin" ] || {
    echo "error: $BIN_DIR/$bin not found — this should have been built as a 'web-dev' prerequisite" >&2
    exit 1
  }
done

mkdir -p "$LOG_DIR" "$WEB_DEV_STATE_DIR/staging"

# ---------------------------------------------------------------------------
# Interrupt handling (issue #268): Ctrl+C delivers SIGINT to this script's
# entire foreground process group — make, this script, and (once started)
# cmd/web all receive it simultaneously; none of start_daemon's setsid
# children do, since setsid moves each into its own session, out of the
# foreground group. SIGTERM delivered to the process group the same way a
# supervisor would (not to `make`'s lone PID, which make does not forward)
# reaches the same three. Both must run the full `web-dev-down` teardown so
# every `make web-dev` starts from a clean slate — see docs/web-ui.md's
# "Local development".
#
# A trap is required because bash's default action for an untrapped
# SIGINT/SIGTERM is to terminate this script immediately, which would race
# cmd/web's own graceful drain (see the grace-period comment further down)
# and skip teardown entirely. The INT/TERM handler only records that a
# signal arrived (and forwards it to cmd/web — see the comment inside); the
# teardown itself runs from the EXIT trap below, so that EVERY exit path
# with an interrupt pending tears down — crucially including `set -e`
# aborts caused by the group-delivered signal killing whatever child
# command this script happened to be running at that moment (a `sleep`
# inside a poll loop, zpool-up.sh, the `nix build` command substitution,
# ...), not just the deliberate exit points.
# ---------------------------------------------------------------------------

INTERRUPTED=0
WEB_PID=""

on_interrupt() {
  INTERRUPTED=1
  # Forward the signal to cmd/web (always as SIGTERM — cmd/web treats
  # INT/TERM identically via signal.NotifyContext, cmd/web/main.go). This
  # matters for a supervisor that signals only this script's lone PID:
  # cmd/web is a separate process (a background job, not an exec), so
  # without forwarding it would never hear the signal, sit un-drained
  # through the whole grace period below, and get SIGKILLed. Harmless on
  # the Ctrl+C path, where cmd/web already received the group-delivered
  # signal directly — NotifyContext swallows the duplicate.
  if [ -n "$WEB_PID" ]; then
    kill -TERM "$WEB_PID" 2> /dev/null || true
  fi
}
trap on_interrupt INT TERM

# run_teardown hands off to the `web-dev-down` Make target — the same
# scripts/web-dev-down.sh + zpool-down/mhvtl-down/temporal-down composition
# `make web-dev-down` already runs standalone — rather than forking a second
# teardown implementation here.
#
# setsid --wait: the teardown must survive a second, impatient Ctrl+C — a
# group-delivered signal must not abort it mid-sequence, so it runs in its
# own session, outside the foreground process group, while --wait keeps the
# call synchronous and preserves the exit code. (If that second Ctrl+C
# kills this script itself, the detached teardown still runs to
# completion on its own.)
#
# MAKEFLAGS/MAKELEVEL/MFLAGS are stripped so this nested invocation never
# tries to negotiate a jobserver token with (or otherwise depend on) the
# outer `make web-dev` invocation that ran this script. WEB_DEV_STATE_DIR
# is already in this script's environment (the Makefile recipe sets it), so
# the nested make would inherit it anyway — passing it explicitly is
# belt-and-braces against a future caller that doesn't export it, not a
# requirement of the Makefile's `?=` default.
run_teardown() {
  echo ""
  echo "==> caught interrupt; running full 'make web-dev-down' teardown..." >&2
  setsid --wait env -u MAKEFLAGS -u MAKELEVEL -u MFLAGS \
    WEB_DEV_STATE_DIR="$WEB_DEV_STATE_DIR" \
    make -C "$PROJECT_DIR" web-dev-down
}

# The EXIT trap is the single place the teardown is invoked from: any exit
# whatsoever — deliberate, or a set -e abort from a signal-killed child —
# tears down iff an interrupt is pending. A genuine startup failure with no
# interrupt (INTERRUPTED=0) deliberately does NOT tear down; it leaves the
# stack up for debugging, exactly as before this issue.
on_exit() {
  # Reset the trap first so nothing run_teardown does can re-enter this
  # handler.
  trap - EXIT
  if [ "$INTERRUPTED" -eq 1 ]; then
    run_teardown
  fi
}
trap on_exit EXIT

# ---------------------------------------------------------------------------
# ZFS test pool: only bring it up if it isn't already there. Unlike
# temporal-up/mhvtl-up (both genuinely idempotent — Makefile prerequisites of
# `web-dev` directly), zpool-up.sh unconditionally destroys and recreates the
# pool on every invocation, which is the correct behavior for
# test-integration/test-e2e (each wants a guaranteed-clean pool) but would be
# actively destructive here: `make web-dev` is meant to be re-run repeatedly
# across a dev session, and blowing away the snapshot backing an in-flight
# webdevseed pass or a run submitted through the UI mid-session on every
# rerun would silently corrupt whatever the developer is currently looking
# at. So this checks first and only calls zpool-up.sh when the expected pool
# is actually absent.
# ---------------------------------------------------------------------------

if ! sudo zpool list -H -o name "${TAPE_POOL_DATASET%%/*}" > /dev/null 2>&1; then
  echo "==> ZFS test pool not present; creating it..."
  "$PROJECT_DIR/scripts/zpool-up.sh"
else
  echo "==> ZFS test pool '${TAPE_POOL_DATASET%%/*}' already present; reusing it."
fi

# ---------------------------------------------------------------------------
# VictoriaLogs + VictoriaMetrics + the vector log shipper (issue #280):
# `docker compose ... up -d --wait` is genuinely idempotent (like
# temporal-up/mhvtl-up), but this is called directly here — the same
# `docker compose` invocation the `web-dev-observability-up` Makefile target
# wraps, not `make web-dev-observability-up` itself, to avoid a nested `make`
# negotiating a jobserver token with the outer `make web-dev` invocation that
# is running this script (see run_teardown's own comment on the same
# concern) — rather than made a `web-dev` Makefile prerequisite, because it
# has an ordering requirement those don't: vector bind-mounts $LOG_DIR
# (WEB_DEV_LOG_DIR below), which must already exist, owned by the invoking
# user, before the container starts, or Docker auto-creates it as root:root
# and the daemons started below (webdevoidc in particular, which — unlike
# worker-control/worker-data — does NOT run under sudo) fail to create their
# own log files in it. $LOG_DIR was already created above (this script's
# very first mkdir -p), so that ordering is satisfied here.
#
# The explicit -p project name (directory basename + "-obs") must stay
# identical to the Makefile's WEB_DEV_OBS_COMPOSE — that's what
# web-dev-observability-down tears down — and distinct from the root
# docker-compose.yml's default project, so neither side's
# `down --remove-orphans` removes the other's containers; see
# docker-compose.web-dev.yml's header comment.
#
# --wait-timeout: with restart: on-failure + healthchecks, a container that
# can never become healthy (e.g. a bad shipper config) would otherwise hang
# this --wait indefinitely; bounding it turns that into a visible failure.
# ---------------------------------------------------------------------------

echo "==> starting VictoriaLogs/VictoriaMetrics dev-observability stack..."
WEB_DEV_LOG_DIR="$LOG_DIR" docker compose \
  -p "$(basename "$PROJECT_DIR" | tr '[:upper:]' '[:lower:]')-obs" \
  -f "$PROJECT_DIR/docker-compose.web-dev.yml" up -d --wait --wait-timeout 120

# ---------------------------------------------------------------------------
# Recovery binaries (real static age/par2/zstd/tar — the data worker's Report
# phase requires them; `make web-dev`'s prerequisites already built them via
# `make recovery-binaries`, i.e. `nix build .#recoveryBinaries`).
# ---------------------------------------------------------------------------

RECOVERY_DIR="$(nix build --print-out-paths --no-link "$PROJECT_DIR#recoveryBinaries")"
TAPE_RECOVERY_BINARIES_DIR="$RECOVERY_DIR/bin"
TAPE_RECOVERY_SOURCES_DIR="$RECOVERY_DIR/src"

# ---------------------------------------------------------------------------
# WEB_SESSION_KEY: generated once and persisted under $WEB_DEV_STATE_DIR so a
# `cmd/web` restart that leaves the state dir in place — a crash/SIGKILL that
# `web-dev-down` (or a subsequent `make web-dev`) tolerates rather than an
# interrupt, which (issue #268) now removes the whole state dir as part of
# its teardown — doesn't sign every open browser tab out. Losing/deleting the
# state dir (including the ordinary Ctrl+C/SIGTERM path now) just signs
# everyone out (pkg/webauth holds no server-side session store), same as a
# real deployment rotating its session key.
# ---------------------------------------------------------------------------

SESSION_KEY_FILE="$WEB_DEV_STATE_DIR/session-key"
if [ ! -s "$SESSION_KEY_FILE" ]; then
  head -c 32 /dev/urandom | base64 > "$SESSION_KEY_FILE"
fi
WEB_SESSION_KEY="$(cat "$SESSION_KEY_FILE")"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

is_running() {
  local pidfile="$1"
  # Two things make this a process-GROUP probe (pgrep -g, procps: succeeds if
  # at least one process in the group is alive) via sudo, rather than a plain
  # single-PID `kill -0 "$pid"`:
  #
  # 1. worker-control/worker-data run under sudo below (their activities need
  #    root for real zfs/zpool commands) — an unprivileged probe against a
  #    still-alive one from a previous invocation fails with "Operation not
  #    permitted", indistinguishable from "no such process", and this
  #    function would wrongly report a genuinely running daemon as dead.
  #    sudo can probe a user-owned process (webdevoidc/webdevseed) exactly as
  #    well as a root-owned one, so this is correct uniformly.
  # 2. This PID file only ever records the PID setsid itself reports via
  #    bash's $! — for a "setsid sudo env ... worker &" launch, that's sudo's
  #    own PID, not the real worker process sudo forks. If sudo has since
  #    died (e.g. a prior stop_daemon escalated to SIGKILL) while its child
  #    kept running, a single-PID check against sudo's now-gone PID reports
  #    "not running" even though the real worker is still alive, orphaned but
  #    still a member of the same process group setsid created (its PGID
  #    equals the session leader's PID, i.e. this recorded PID). Probing the
  #    whole group, not just the recorded leader PID, catches that case.
  #
  # Not `kill -0 -- "-$pid"` (the traditional POSIX "negative PID = process
  # group" kill(2) target): confirmed the hard way that util-linux's kill
  # binary (pkgs.util-linux, this repo's own setsid dependency) does not
  # support it — it parses a leading "-" as an option/name, not a negative
  # PID, and always reports "cannot find process". pgrep -g is the reliable,
  # portable way to do this instead.
  [ -f "$pidfile" ] && sudo pgrep -g "$(cat "$pidfile")" > /dev/null 2>&1
}

# start_daemon launches "$@" detached (setsid) into its own session, under
# name $1's log/pid files, unless a process from a previous invocation is
# already alive.
start_daemon() {
  local name="$1"
  shift
  local pidfile="$WEB_DEV_STATE_DIR/$name.pid"
  local logfile="$LOG_DIR/$name.log"

  if is_running "$pidfile"; then
    echo "==> $name already running (pid $(cat "$pidfile")); leaving it alone"
    return 0
  fi

  setsid "$@" > "$logfile" 2>&1 < /dev/null &
  echo $! > "$pidfile"
  echo "==> $name started (pid $(cat "$pidfile")), logging to $logfile"
}

# wait_for_ready polls path (relative to http://127.0.0.1$addr) until it
# returns 2xx, warning (not silently continuing) if it never does within the
# deadline — every readiness wait in this script goes through here so a
# startup failure is always reported the same way, not discovered later as a
# confusing downstream error.
wait_for_ready() {
  local name="$1" addr="$2" path="$3"
  echo -n "==> Waiting for $name to become ready"
  for _ in $(seq 1 40); do
    if curl -fs "http://127.0.0.1${addr}${path}" > /dev/null 2>&1; then
      echo ""
      return 0
    fi
    echo -n "."
    # || true: a group-delivered interrupt kills this sleep, and under
    # set -e an unguarded nonzero sleep would abort the script from inside
    # this poll loop (the EXIT trap would still tear down, but the readiness
    # phase shouldn't die early when the teardown is about to run anyway —
    # and every sleep in this script is guarded uniformly for the same
    # reason).
    sleep 0.5 || true
  done
  echo ""
  echo "warning: $name did not report ready in time; continuing anyway (see $LOG_DIR/$name.log)" >&2
}

# ---------------------------------------------------------------------------
# Local OIDC provider
# ---------------------------------------------------------------------------

start_daemon webdevoidc env \
  WEBDEVOIDC_LISTEN_ADDR="$OIDC_LISTEN_ADDR" \
  WEBDEVOIDC_CLIENT_ID="$OIDC_CLIENT_ID" \
  WEBDEVOIDC_CLIENT_SECRET="$OIDC_CLIENT_SECRET" \
  WEBDEVOIDC_SUBJECT="$OIDC_SUBJECT" \
  WEBDEVOIDC_EMAIL="$OIDC_EMAIL" \
  WEBDEVOIDC_NAME="$OIDC_NAME" \
  "$BIN_DIR/webdevoidc"

wait_for_ready webdevoidc ":${OIDC_LISTEN_ADDR##*:}" "/.well-known/openid-configuration"

# ---------------------------------------------------------------------------
# Control + data workers (real cmd/worker, both roles) — needed so seeded (and
# any ad hoc, browser-submitted) runs actually progress to completion instead
# of sitting in "Running" forever with no worker polling their task queues.
# ---------------------------------------------------------------------------

# sudo: the control worker's snapshot-resolution activities (and the data
# worker's staging activities below) run real `zfs`/`zpool` commands against
# the pool zpool-up.sh created, which — like every other place this repo
# drives ZFS (test-integration's whole `go test` invocation, zpool-up.sh
# itself) — require root. Without this, ResolveAndCheck fails immediately
# with "Permission denied: the ZFS utilities must be run as root" and every
# seeded/submitted run hangs retrying forever. `sudo env` (not a bare `sudo`)
# because sudo strips the rest of the environment by default — matching how
# the Makefile's own `test-integration`/`test-e2e` targets already pass
# PATH/env through their `sudo env ...` invocations.
start_daemon worker-control sudo env \
  PATH="$PATH" HOME="$HOME" \
  ROLE=control \
  TEMPORAL_ADDRESS="$TEMPORAL_ADDRESS" \
  HEALTH_ADDR="$CONTROL_HEALTH_ADDR" \
  METRICS_ADDR="$CONTROL_METRICS_ADDR" \
  "$BIN_DIR/worker"

start_daemon worker-data sudo env \
  PATH="$PATH" HOME="$HOME" \
  ROLE=data \
  TEMPORAL_ADDRESS="$TEMPORAL_ADDRESS" \
  HEALTH_ADDR="$DATA_HEALTH_ADDR" \
  METRICS_ADDR="$DATA_METRICS_ADDR" \
  TAPE_STAGING_DIR="$WEB_DEV_STATE_DIR/staging" \
  TAPE_RECOVERY_BINARIES_DIR="$TAPE_RECOVERY_BINARIES_DIR" \
  TAPE_RECOVERY_SOURCES_DIR="$TAPE_RECOVERY_SOURCES_DIR" \
  "$BIN_DIR/worker"

wait_for_ready worker-control "$CONTROL_HEALTH_ADDR" "/readyz"
wait_for_ready worker-data "$DATA_HEALTH_ADDR" "/readyz"

# ---------------------------------------------------------------------------
# Sample-run seeding (background, bounded lifetime — see cmd/webdevseed's doc
# comment for why this cannot block here: backup runs are a Temporal
# singleton, so seeding 2-3 real dry-runs against mhvtl is inherently
# sequential and can take several minutes; History fills in progressively
# while the developer is already looking at the UI, rather than delaying
# `make web-dev` itself).
#
# Routed through start_daemon (fixed log name, not a timestamped one) like
# every other daemon here, so a second `make web-dev` while a seeding pass
# from the first is still running (it can take several minutes) leaves the
# first one alone instead of silently orphaning it — start_daemon's own
# is_running check is what makes that safe, and it's also what makes
# web-dev-down.sh able to actually find and stop it later.
# ---------------------------------------------------------------------------

start_daemon webdevseed env \
  TEMPORAL_ADDRESS="$TEMPORAL_ADDRESS" \
  MHVTL_CHANGER_DEV="$MHVTL_CHANGER_DEV" \
  MHVTL_DRIVE0_DEV="$MHVTL_DRIVE0_DEV" \
  MHVTL_DRIVE1_DEV="$MHVTL_DRIVE1_DEV" \
  WEBDEVSEED_SOURCE="${WEBDEVSEED_SOURCE:-$TAPE_POOL_DATASET@$TAPE_TEST_SNAPSHOT}" \
  WEBDEVSEED_COUNT="${WEBDEVSEED_COUNT:-3}" \
  "$BIN_DIR/webdevseed"
SEED_LOG="$LOG_DIR/webdevseed.log"

# ---------------------------------------------------------------------------
# Interrupted before cmd/web even started (e.g. during webdevseed startup
# above)? Nothing to wait for — exit now (the EXIT trap runs the teardown)
# instead of printing a "stack is up" banner for a stack that's about to
# come right back down.
# ---------------------------------------------------------------------------

if [ "$INTERRUPTED" -eq 1 ]; then
  exit 0
fi

# ---------------------------------------------------------------------------
# Report + run cmd/web (foreground from the operator's point of view, but a
# background job of this script — see the interrupt-handling comment above
# for why this can no longer `exec` into it directly: this script must stay
# alive after cmd/web exits to run the teardown).
# ---------------------------------------------------------------------------

cat << EOF

==============================================================================
 tape-archiver web UI dev stack is up.

   URL:      http://127.0.0.1${WEB_DEV_PORT:+:$WEB_DEV_PORT}
   Log in with:
     subject: $OIDC_SUBJECT
     email:   $OIDC_EMAIL
     name:    $OIDC_NAME

   The local OIDC provider has no interactive login form (issue #265's
   documented tradeoff for zero new Go/Docker dependencies) — opening the URL
   and following the redirect signs you in as the user above automatically;
   there is nothing to type.

   Sample dry-run backups are being submitted in the background and will
   appear in History over the next few minutes (tail $SEED_LOG to watch).

   VictoriaLogs:    $VICTORIALOGS_URL
   VictoriaMetrics: $VICTORIAMETRICS_URL
   (cmd/web doesn't render log/metric panels from these yet — issues #274/#275 —
   but both are already ingesting/scraping the dev workers; query them directly
   at the URLs above in the meantime.)

   Ctrl+C (or SIGTERM) stops the whole dev stack: cmd/web shuts down first,
   then the full 'make web-dev-down' teardown runs automatically —
   Temporal/mhvtl/zpool, VictoriaLogs/VictoriaMetrics, the OIDC provider, and
   the workers all come down, so the next 'make web-dev' always starts from a
   clean slate. Run 'make web-dev-down' yourself only after a crash/SIGKILL,
   which cannot be trapped.
==============================================================================

EOF

env \
  WEB_LISTEN_ADDRESS="$WEB_LISTEN_ADDRESS" \
  TEMPORAL_ADDRESS="$TEMPORAL_ADDRESS" \
  OIDC_ISSUER_URL="http://$OIDC_LISTEN_ADDR" \
  OIDC_CLIENT_ID="$OIDC_CLIENT_ID" \
  OIDC_CLIENT_SECRET="$OIDC_CLIENT_SECRET" \
  OIDC_REDIRECT_URL="http://127.0.0.1${WEB_DEV_PORT:+:$WEB_DEV_PORT}/auth/callback" \
  WEB_SESSION_KEY="$WEB_SESSION_KEY" \
  MHVTL_CHANGER_DEV="$MHVTL_CHANGER_DEV" \
  MHVTL_DRIVE0_DEV="$MHVTL_DRIVE0_DEV" \
  MHVTL_DRIVE1_DEV="$MHVTL_DRIVE1_DEV" \
  VICTORIALOGS_URL="$VICTORIALOGS_URL" \
  VICTORIALOGS_STREAM_FILTER="$VICTORIALOGS_STREAM_FILTER" \
  VICTORIAMETRICS_URL="$VICTORIAMETRICS_URL" \
  "$BIN_DIR/web" &
WEB_PID=$!

# web.pid: only consumed by web-dev-down.sh's crash-remedy path. This
# script's own interrupt flow never needs it (WEB_PID is in hand), but if
# this script and make are SIGKILLed — which cannot be trapped — cmd/web
# survives as an orphan holding the web port, and without a pidfile
# `make web-dev-down` (the documented crash remedy) would have no way to
# find and stop it.
echo "$WEB_PID" > "$WEB_DEV_STATE_DIR/web.pid"

# Wait for cmd/web to exit on its own — this must wait indefinitely while
# nothing has interrupted it: a healthy, long-running dev session (this is
# meant to stay up for however long the developer is working) must never be
# killed just for running a while. A poll (kill -0 + sleep), not a blocking
# `wait`, deliberately: a signal landing in the window between this loop's
# condition check and a blocking `wait` would leave the script stuck in
# that wait until cmd/web actually exits, silently defeating the bounded
# grace period below if cmd/web wedges mid-drain. The 1s detection latency
# for a normal cmd/web exit is irrelevant for dev tooling.
while kill -0 "$WEB_PID" 2> /dev/null && [ "$INTERRUPTED" -eq 0 ]; do
  sleep 1 || true
done

# Only once actually interrupted does a bounded grace period apply, counted
# from the moment the signal arrived (not from when cmd/web started).
# cmd/web's total process lifetime after a shutdown signal has been
# observed at 25-70s (longer with a browser SSE connection open) — note
# that is NOT fully attributed: cmd/web's srv.Shutdown is hard-capped at
# shutdownTimeout=10s (cmd/web/main.go), so the remainder is going to the
# post-Shutdown health/metrics/Temporal-client cleanup; tracked separately,
# not investigated in this script. 90s is the generous bound over that
# observed range, after which SIGKILL guarantees an interrupt can never
# hang forever even if cmd/web wedges mid-drain.
if [ "$INTERRUPTED" -eq 1 ]; then
  WEB_GRACE_PERIOD_S=90
  WEB_GRACE_DEADLINE=$((SECONDS + WEB_GRACE_PERIOD_S))
  while kill -0 "$WEB_PID" 2> /dev/null; do
    if [ "$SECONDS" -ge "$WEB_GRACE_DEADLINE" ]; then
      echo "==> cmd/web (pid $WEB_PID) did not exit within ${WEB_GRACE_PERIOD_S}s of the interrupt; sending SIGKILL" >&2
      kill -KILL "$WEB_PID" 2> /dev/null || true
      break
    fi
    sleep 1 || true
  done
fi

# Single final reap of the (now dead, or just-SIGKILLed) cmd/web job. Its
# exit code only matters on the un-interrupted path, where cmd/web exiting
# on its own is the whole script's outcome; on the interrupt path the exit
# below is an orderly stop and the EXIT trap runs the teardown.
WEB_EXIT=0
wait "$WEB_PID" 2> /dev/null || WEB_EXIT=$?

if [ "$INTERRUPTED" -eq 1 ]; then
  exit 0
fi

exit "$WEB_EXIT"
