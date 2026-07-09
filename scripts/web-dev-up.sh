#!/usr/bin/env bash
# web-dev-up.sh — bring up the local `cmd/web` dev stack (issue #265): a local
# OIDC provider, real control + data workers, a background sample-run
# seeding pass, then `cmd/web` itself in the foreground.
#
# Temporal/mhvtl/zpool are NOT brought up here — the `web-dev` Makefile target
# depends on the existing `temporal-up`/`mhvtl-up`/`zpool-up` targets directly,
# so this script assumes they are already up when it starts.
#
# Everything this script itself starts (the OIDC provider, both worker roles,
# and the seeding pass) is detached into its own session via `setsid`, so a
# developer's Ctrl+C — which the terminal only delivers to its foreground
# process group — stops just the `cmd/web` process this script execs into at
# the very end, not the rest of the stack. See docs/web-ui.md's "Local
# development" section.
#
# Idempotent: rerunning this script (e.g. `make web-dev` again after a code
# change + rebuild) skips starting a daemon that is already running (tracked
# via PID files under $WEB_DEV_STATE_DIR) and just re-execs cmd/web, matching
# `temporal-up`'s own `--wait`-based idempotency.
#
# Requires (all provided by `nix develop`): age-keygen (webdevseed's sample
# keypair), mkltfs/ltfs/par2/zstd/tar/mt-st/sg3-utils (the data worker's real
# activities), setsid (util-linux), curl.

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

# ---------------------------------------------------------------------------
# Sanity checks
# ---------------------------------------------------------------------------

for cmd in age-keygen mkltfs setsid curl; do
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
# Recovery binaries (real static age/par2/zstd/tar — the data worker's Report
# phase requires them; `make web-dev`'s prerequisites already built them via
# `make recovery-binaries`, i.e. `nix build .#recoveryBinaries`).
# ---------------------------------------------------------------------------

RECOVERY_DIR="$(nix build --print-out-paths --no-link "$PROJECT_DIR#recoveryBinaries")"
TAPE_RECOVERY_BINARIES_DIR="$RECOVERY_DIR/bin"
TAPE_RECOVERY_SOURCES_DIR="$RECOVERY_DIR/src"

# ---------------------------------------------------------------------------
# WEB_SESSION_KEY: generated once and persisted under $WEB_DEV_STATE_DIR so
# sessions survive a `cmd/web` restart within the same dev session (e.g.
# Ctrl+C, rebuild, `make web-dev` again) — otherwise every restart would sign
# every open browser tab out. Losing/deleting the state dir just signs
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
  [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2> /dev/null
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

echo -n "==> Waiting for the local OIDC provider"
for _ in $(seq 1 20); do
  if curl -fs "http://$OIDC_LISTEN_ADDR/.well-known/openid-configuration" > /dev/null 2>&1; then
    break
  fi
  echo -n "."
  sleep 0.5
done
echo ""

# ---------------------------------------------------------------------------
# Control + data workers (real cmd/worker, both roles) — needed so seeded (and
# any ad hoc, browser-submitted) runs actually progress to completion instead
# of sitting in "Running" forever with no worker polling their task queues.
# ---------------------------------------------------------------------------

start_daemon worker-control env \
  ROLE=control \
  TEMPORAL_ADDRESS="$TEMPORAL_ADDRESS" \
  HEALTH_ADDR="$CONTROL_HEALTH_ADDR" \
  METRICS_ADDR="$CONTROL_METRICS_ADDR" \
  "$BIN_DIR/worker"

start_daemon worker-data env \
  ROLE=data \
  TEMPORAL_ADDRESS="$TEMPORAL_ADDRESS" \
  HEALTH_ADDR="$DATA_HEALTH_ADDR" \
  METRICS_ADDR="$DATA_METRICS_ADDR" \
  TAPE_STAGING_DIR="$WEB_DEV_STATE_DIR/staging" \
  TAPE_RECOVERY_BINARIES_DIR="$TAPE_RECOVERY_BINARIES_DIR" \
  TAPE_RECOVERY_SOURCES_DIR="$TAPE_RECOVERY_SOURCES_DIR" \
  "$BIN_DIR/worker"

wait_for_health() {
  local name="$1" addr="$2"
  echo -n "==> Waiting for $name to become ready"
  for _ in $(seq 1 40); do
    if curl -fs "http://127.0.0.1${addr}/readyz" > /dev/null 2>&1; then
      echo ""
      return 0
    fi
    echo -n "."
    sleep 0.5
  done
  echo ""
  echo "warning: $name did not report ready in time; continuing anyway (see $LOG_DIR/$name.log)" >&2
}

wait_for_health worker-control "$CONTROL_HEALTH_ADDR"
wait_for_health worker-data "$DATA_HEALTH_ADDR"

# ---------------------------------------------------------------------------
# Sample-run seeding (background, bounded lifetime — see cmd/webdevseed's doc
# comment for why this cannot block here: backup runs are a Temporal
# singleton, so seeding 2-3 real dry-runs against mhvtl is inherently
# sequential and can take several minutes; History fills in progressively
# while the developer is already looking at the UI, rather than delaying
# `make web-dev` itself).
# ---------------------------------------------------------------------------

SEED_LOG="$LOG_DIR/webdevseed-$(date +%s).log"
setsid env \
  TEMPORAL_ADDRESS="$TEMPORAL_ADDRESS" \
  MHVTL_CHANGER_DEV="$MHVTL_CHANGER_DEV" \
  MHVTL_DRIVE0_DEV="$MHVTL_DRIVE0_DEV" \
  MHVTL_DRIVE1_DEV="$MHVTL_DRIVE1_DEV" \
  WEBDEVSEED_SOURCE="${WEBDEVSEED_SOURCE:-$TAPE_POOL_DATASET@$TAPE_TEST_SNAPSHOT}" \
  WEBDEVSEED_COUNT="${WEBDEVSEED_COUNT:-3}" \
  "$BIN_DIR/webdevseed" > "$SEED_LOG" 2>&1 < /dev/null &
echo $! > "$WEB_DEV_STATE_DIR/webdevseed.pid"
echo "==> seeding 2-3 sample dry-runs in the background, logging to $SEED_LOG"

# ---------------------------------------------------------------------------
# Report + hand off to cmd/web
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

   Ctrl+C stops only this web server. Temporal/mhvtl/zpool, the OIDC
   provider, and the workers stay up for the next 'make web-dev'. Run
   'make web-dev-down' to tear everything down.
==============================================================================

EOF

exec env \
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
  "$BIN_DIR/web"
