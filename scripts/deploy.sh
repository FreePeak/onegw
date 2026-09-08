#!/bin/bash
# Zero-drop deploy runbook (#37). RCA of the 2026-09-08 outage: the sole
# listener was killed BEFORE its replacement was verified, leaving the
# gateway down. This script enforces the opposite ordering:
#
#   build NEW -> start NEW (overlaps live listener; SO_REUSEPORT makes the
#   double-bind safe) -> poll NEW's /admin/health until ok -> ONLY THEN
#   SIGTERM the OLD PID (graceful drain, never SIGKILL) -> confirm exactly
#   one listener remains -> watch briefly for autorespawn (kill -> verify
#   -> re-kill) -> record state.
#
# Invariants:
#   - Never stop-then-start. If any pre-check fails we exit non-zero
#     WITHOUT touching the old listener.
#   - Refuse to start if the port already has 2+ listeners (a previous
#     deploy half-finished; manual cleanup required first).
#   - SIGTERM only: the supervisor treats graceful exits as deliberate
#     stops, so the old instance must not be autorespawned; we still
#     watch and re-kill in case it is.
#   - Never pkill -f: the pattern matches the replacement too.
set -euo pipefail
cd "$(dirname "$0")/.."

BIN=""
CONFIG="./onegw.toml"
# abort after NEW was started: never leave an unverified NEW instance bound.
abort() { [ -z "${NEW_PID:-}" ] || [ "$DRY_RUN" = 1 ] || kill -TERM "$NEW_PID" 2>/dev/null || true; die "$1"; }
DRY_RUN=0

usage() { echo "usage: $0 [--dry-run] [--binary PATH] [--config PATH]" >&2; exit 2; }
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --binary)  [ $# -ge 2 ] || usage; BIN="$2"; shift ;;
    --config)  [ $# -ge 2 ] || usage; CONFIG="$2"; shift ;;
    *) usage ;;
  esac
  shift
done

step() { echo "== $1 =="; }
die() { echo "DEPLOY ABORTED: $1" >&2; exit 1; }
run() { # run <desc> <cmd...>: execute, or print only under --dry-run
  # shift past the description, else "$@" execs a literal binary named
  # after the description's first word ("go build" -> command not found).
  if [ "$DRY_RUN" = 1 ]; then echo "  [dry-run] $*"; else shift; "$@"; fi
}

# Resolve CONFIG to an absolute path: the NEW instance outlives this
# script, and a relative -config would make its cwd load-bearing.
CONFIG=$(cd "$(dirname "$CONFIG")" && pwd)/$(basename "$CONFIG")

[ -f "$CONFIG" ] || die "config not found: $CONFIG"

# --- derive port + admin password from the config (defaults match internal/config).
PORT=$(sed -n 's/^[[:space:]]*listen[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG" | head -1)
PORT=${PORT:-127.0.0.1:8080}
PORT=${PORT##*:}
ADMIN_PW=$(sed -n 's/^[[:space:]]*admin_password[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG" | head -1)
ADMIN_PW=${ADMIN_PW:-admin}
HEALTH="http://127.0.0.1:$PORT/admin/health"

# --- snapshot the CURRENT listener set BEFORE anything is touched.
# Exactly one listener is the expected steady state; two means a previous
# deploy half-finished; we must not pile a third on top.
OLD_PIDS=$(lsof -t -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null || true)
N_OLD=$(printf '%s\n' $OLD_PIDS | grep -c . || true)
[ "$N_OLD" -le 1 ] || die "port $PORT already has $N_OLD listeners ($OLD_PIDS); refusing to add a third. Clean up manually first."
OLD_PID=${OLD_PIDS:-}
[ -n "$OLD_PID" ] || echo "WARN: no live listener on port $PORT (cold start)"

step "Plan"
echo "  port:        $PORT"
echo "  config:      $CONFIG"
echo "  old pid:     ${OLD_PID:-<none>}"
echo "  dry-run:     $DRY_RUN"

# --- 1. build NEW binary (temp path, never the live one).
NEW_BIN="$BIN"
if [ -z "$NEW_BIN" ]; then
  NEW_BIN="$(mktemp -t onegw-new.XXXXXX)"
  step "1. build NEW binary -> $NEW_BIN"
  run "go build" env CGO_ENABLED=0 go build -o "$NEW_BIN" ./cmd/onegw
else
  [ -x "$NEW_BIN" ] || die "binary not executable: $NEW_BIN"
  step "1. using prebuilt binary $NEW_BIN"
fi

# --- 2. start NEW overlapping the live listener. SO_REUSEPORT makes the
# double-bind safe; clients round-robin between old and new.
# The NEW listener is detached from this script's process group (setsid +
# disown) so nothing that kills this script — a job timeout, an interrupted
# bash session, our own abort path — can ever take the gateway down
# (2026-09-08 freeze RCA: a parent timeout SIGTERMed the whole group and
# killed a just-verified NEW listener, freezing every session).
step "2. start NEW instance (overlapping bind)"
if [ "$DRY_RUN" = 1 ]; then
  echo "  [dry-run] perl -MPOSIX setsid $NEW_BIN -config $CONFIG &   # capture NEW_PID"
  NEW_PID="<new>"
else
  # macOS has no setsid(1); perl's POSIX::setsid() moves the child to a
  # NEW SESSION/GROUP so nothing that kills this script — a bash job
  # timeout, cleanup traps, an interrupted session — can ever reach the
  # gateway (2026-09-08 freeze RCA). nohup/disown alone do NOT do this.
  perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' -- "$NEW_BIN" -config "$CONFIG" </dev/null >>/tmp/onegw-new.log 2>&1 &
  NEW_PID=$!
  disown "$NEW_PID" 2>/dev/null || true
  sleep 1
  kill -0 "$NEW_PID" 2>/dev/null || abort "NEW instance (pid $NEW_PID) exited immediately; old listener untouched"
  echo "  NEW_PID=$NEW_PID"
fi

# --- 3. verify NEW health BEFORE touching old. Connections distribute
# across both listeners, so require several consecutive ok responses:
# round-robin guarantees some land on the NEW process.
step "3. poll NEW /admin/health until ok"
OK=0
for i in $(seq 1 60); do
  if [ "$DRY_RUN" = 1 ]; then echo "  [dry-run] curl -sf -H X-Admin-Password:*** $HEALTH"; OK=8; break; fi
  if curl -sf -H "X-Admin-Password: $ADMIN_PW" "$HEALTH" >/dev/null 2>&1; then
    OK=$((OK + 1))
    [ "$OK" -ge 8 ] && break
  else
    OK=0
  fi
  sleep 0.5
done
[ "$OK" -ge 8 ] || abort "NEW instance never became healthy; OLD listener untouched"
echo "  health ok"

# --- 4. ONLY NOW stop the old listener. SIGTERM (graceful drain, 30s
# server deadline), never SIGKILL; never pkill -f (matches replacement).
step "4. SIGTERM old listener ${OLD_PID:-<none>}"
if [ -n "$OLD_PID" ]; then
  if [ "$DRY_RUN" = 1 ]; then
    echo "  [dry-run] kill -TERM $OLD_PID"
  else
    kill -TERM "$OLD_PID" 2>/dev/null || die "failed to signal old pid $OLD_PID"
  fi
fi

# --- 5. kill -> verify -> re-kill: confirm exactly one listener remains
# and it is NEW; if a supervisor autorespawned the old binary, re-TERM it.
step "5. verify single remaining listener (autorespawn watch)"
if [ "$DRY_RUN" = 1 ]; then
  echo "  [dry-run] loop 10s: lsof -t -iTCP:$PORT -sTCP:LISTEN; re-kill any pid != NEW_PID"
else
  # Loop until ONLY NEW remains (old gone, no respawn). Never kill NEW_PID.
  DONE=0
  for i in $(seq 1 30); do
    NOW=$(lsof -t -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null || true)
    if printf '%s\n' $NOW | grep -qx "$NEW_PID" && [ "$(printf '%s\n' $NOW | grep -c .)" = 1 ]; then
      DONE=1; break
    fi
    STRAY=$(printf '%s\n' $NOW | grep -vx "$NEW_PID" | grep . || true)
    if [ -n "$STRAY" ]; then
      echo "  respawn/stale listener(s): $STRAY; re-sending SIGTERM"
      for p in $STRAY; do kill -TERM "$p" 2>/dev/null || true; done
    fi
    sleep 0.5
  done
  [ "$DONE" = 1 ] || abort "expected only NEW_PID $NEW_PID listening on $PORT; last state: $(lsof -t -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null || true)"
  kill -0 "$NEW_PID" 2>/dev/null || abort "NEW instance died during cutover"
fi

# --- 6. final health + record state.
step "6. final health check + state record"
if [ "$DRY_RUN" = 1 ]; then
  echo "  [dry-run] curl -sf -H X-Admin-Password:*** $HEALTH"
else
  curl -sf -H "X-Admin-Password: $ADMIN_PW" "$HEALTH" >/dev/null || die "final health check failed"
fi
echo "DEPLOY COMPLETE $(date -u '+%Y-%m-%dT%H:%M:%SZ') port=$PORT pid=${NEW_PID} binary=$NEW_BIN config=$CONFIG"

# NOTE: the NEW_PID above is the pid of the binary we started, but the
# long-lived supervisor (hub) may adopt/respawn by name; record the port's
# current listener as the authoritative serving pid.
if [ "$DRY_RUN" != 1 ]; then
  SERVING=$(lsof -t -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | head -1)
  echo "serving pid: $SERVING"
fi
