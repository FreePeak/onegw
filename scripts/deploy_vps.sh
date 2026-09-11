#!/bin/bash
# Zero-drop VPS deploy (#55) — the remote analog of scripts/deploy.sh.
#
# Builds the binary from `git archive HEAD` on THIS machine (repo invariant:
# never build from a shared working tree that peer sessions are editing),
# uploads it, and takes the VPS gateway over in the order the local runbook
# enforces:
#
#   pre-flight (at most one listener; refuse a third on a half-finished
#   deploy) -> upload NEW binary to a temp path on the destination
#   filesystem -> start NEW detached (setsid, own session, outlives this
#   ssh) -> poll NEW's authenticated /admin/health until ok -> ONLY THEN
#   SIGTERM the OLD pid (graceful 30s drain, never SIGKILL) -> confirm
#   exactly one listener remains (re-TERM supervisor respawns) -> health
#   twice -> atomically swap NEW over the canonical binary path.
#
# Two takeover paths:
#   - no active systemd unit: full overlap-bind zero-drop. The gateway sets
#     SO_REUSEPORT on its listener (cmd/onegw/main.go), so NEW binds beside
#     OLD and clients round-robin during the health window.
#   - `systemctl is-active onegw` = active: smoke-test NEW on a throwaway
#     port + throwaway data dir, swap the canonical binary, then
#     `systemctl restart`. A hand-spawned NEW would be a process systemd
#     cannot supervise (KillMode=mixed would kill it on the unit's next
#     restart), so overlap does not compose with systemd ownership. Restart
#     is stop->start with a 35s drain budget (TimeoutStopSec=35s in
#     contrib/systemd/onegw.service) — seconds-scale downtime, in-flight
#     streams preserved, and a broken build can never turn the restart into
#     an outage because it was rejected by the self-test BEFORE the swap.
#
# Secrets: the admin password is parsed from the REMOTE config (or its env
# file) on the remote host and stays in a remote shell variable. curl
# receives it via stdin (`curl -H @-`), so it never appears on an argv
# (visible in `ps`), never in a local variable, and is never echoed. Do NOT
# run this script under `bash -x`. Provider keys are read by SOURCING the
# remote env file into the remote shell only (the spawned NEW needs them at
# config load; a missing key is a config error, not a secret leak).
#
# Ssh-drop posture: the takeover runs as one remote `bash -s`. If the ssh
# connection drops mid-run, the remote script dies with the session — but
# SIGTERM-OLD is the LAST mutation, strictly after NEW proved healthy, so
# every drop window leaves the gateway serving (worst case: NEW already
# bound beside OLD; re-running refuses to add a third listener and asks
# for manual cleanup).
#
# Invariants (same as scripts/deploy.sh):
#   - Never stop-then-start on the overlap path: any pre-check failure
#     exits non-zero WITHOUT touching the old listener.
#   - SIGTERM only; never pkill -f (the pattern matches the replacement).
#   - The previous binary is preserved as <dest>/onegw.prev for rollback.
#
# Requirements LOCAL: git, go (cross build) unless --binary. REMOTE: ssh
# key auth as root (or a user that can write <dest> and signal the
# gateway; combine with --sudo); curl >= 7.55 (-H @-); setsid (util-linux)
# or perl; lsof or ss (iproute2); systemctl for the systemd path.
set -euo pipefail

HOST="" ARCH="" BIN="" SUDO_MODE=0 DRY_RUN=0
CONFIG="/etc/onegw/onegw.toml"      # remote config path
DEST="/usr/local/bin"               # remote binary directory
SERVICE="onegw"                     # remote systemd unit name

usage() {
  echo "usage: $0 --host user@vps [--arch amd64|arm64] [--binary PATH] \\
    [--config REMOTE_TOML] [--dest REMOTE_BINDIR] [--service UNIT] [--sudo] [--dry-run]" >&2
  exit 2
}
while [ $# -gt 0 ]; do
  case "$1" in
    --host)    [ $# -ge 2 ] || usage; HOST="$2"; shift ;;
    --arch)    [ $# -ge 2 ] || usage; ARCH="$2"; shift ;;
    --binary)  [ $# -ge 2 ] || usage; BIN="$2"; shift ;;
    --config)  [ $# -ge 2 ] || usage; CONFIG="$2"; shift ;;
    --dest)    [ $# -ge 2 ] || usage; DEST="$2"; shift ;;
    --service) [ $# -ge 2 ] || usage; SERVICE="$2"; shift ;;
    --sudo)    SUDO_MODE=1 ;;
    --dry-run) DRY_RUN=1 ;;
    *) usage ;;
  esac
  shift
done
[ -n "$HOST" ] || { echo "DEPLOY ABORTED: --host is required (e.g. root@gw.example.com)" >&2; exit 2; }
# Validate values before they are interpolated into remote argv.
for v in "$HOST" "$CONFIG" "$DEST" "$SERVICE"; do
  printf '%s' "$v" | grep -Eq '^[A-Za-z0-9._@/-]+$' \
    || { echo "DEPLOY ABORTED: unsafe value: $v" >&2; exit 2; }
done
case "$ARCH" in "") ;; amd64|arm64) ;; *) echo "DEPLOY ABORTED: --arch must be amd64 or arm64" >&2; exit 2 ;; esac

step() { echo "== $1 =="; }
die() { echo "DEPLOY ABORTED: $1" >&2; exit 1; }
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10)
STAGE=$(mktemp -d /tmp/onegw-vps.XXXXXX)
trap 'rm -rf "$STAGE"' EXIT

# --- takeover script: written to $STAGE and piped over ssh as `bash -s`,
# values as positional args ($1 config, $2 dest, $3 uploaded name, $4 sudo
# prefix, $5 unit). Printed verbatim by --dry-run.

# git archive HEAD must run inside the checkout: cd to the repo top (same
# convention as scripts/deploy.sh; git resolves it from the script location
# so invoking the script from any cwd works).
cd "$(dirname "$0")/.."
git rev-parse --git-dir >/dev/null 2>&1 || { echo "DEPLOY ABORTED: not inside a onegw git checkout" >&2; exit 1; }
cat > "$STAGE/takeover.sh" <<'REMOTE_EOF'
set -eu
CONFIG=$1; DEST=$2; NEW_NAME=$3; SUDO=$4; SERVICE=$5
NEW_PATH="$DEST/$NEW_NAME"
FINAL="$DEST/onegw"
# Remove the uploaded NEW binary if the takeover aborts anywhere (after a
# successful swap the path no longer exists; rm -f is a no-op then).
trap 'rm -f "$NEW_PATH" 2>/dev/null || true' EXIT
ENVFILE=$(dirname "$CONFIG")/onegw.env

warn() { echo "  WARN: $*" >&2; }
die()  { echo "REMOTE ABORTED: $*" >&2; exit 4; }

# --- derive port + admin password from the REMOTE config (never echoed).
# Defaults mirror internal/config: listen 127.0.0.1:8080; admin password
# from toml, else ONEGW_ADMIN_PASSWORD in the env file, else "admin".
PORT=$(sed -n 's/^[[:space:]]*listen[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG" | head -1)
PORT=${PORT:-127.0.0.1:8080}; PORT=${PORT##*:}
ADMIN_PW=$(sed -n 's/^[[:space:]]*admin_password[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG" | head -1)
if [ -z "$ADMIN_PW" ] && [ -f "$ENVFILE" ]; then
  ADMIN_PW=$(sed -n -e 's/^ONEGW_ADMIN_PASSWORD="\?\([^"]*\)"\?$/\1/p' "$ENVFILE" | head -1)
fi
if [ -z "$ADMIN_PW" ]; then
  # Empty/absent key + no env entry: the gateway generated its first-run
  # credential under the data dir (internal/config/adminpw.go) — probe with
  # that instead of the guessable "admin".
  DATA_DIR=$(sed -n 's/^[[:space:]]*data_dir[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG" | head -1)
  [ -n "$DATA_DIR" ] && [ -f "$DATA_DIR/admin_password" ] && ADMIN_PW=$(cat "$DATA_DIR/admin_password")
fi
ADMIN_PW=${ADMIN_PW:-admin}
HEALTH="http://127.0.0.1:$PORT/admin/health"

# curl -H @- keeps the password off argv (curl >= 7.55).
health() { printf 'X-Admin-Password: %s\n' "$ADMIN_PW" | curl -sSf -H @- -o /dev/null --max-time 5 "$HEALTH"; }

listeners() {
  if command -v lsof >/dev/null 2>&1; then
    lsof -t -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null || true
  else
    ss -tlnpH "sport = :$PORT" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | sort -u
  fi
}
count() { if [ $# -eq 0 ]; then echo 0; else printf '%s\n' "$@" | grep -c .; fi; }

# --- data_dir guard: a relative data_dir would place usage.db in whatever
# cwd the detached process inherits. Absolute path or unset (config
# default is an absolute $HOME/.onegw) only.
if grep -Eq '^[[:space:]]*data_dir[[:space:]]*=[[:space:]]*"[^/"]' "$CONFIG"; then
  die "data_dir in $CONFIG is relative — set an absolute path (e.g. /var/lib/onegw) first"
fi

# Provider keys: the NEW process needs them at config load (a missing key
# is a fatal config error). Source the env file into THIS shell only.
# Failure aborts before anything is started.
if [ -f "$ENVFILE" ]; then
  # shellcheck disable=SC1090
  set -a; . "$ENVFILE"; set +a
fi

OLD=$(listeners); NOLD=$(count $OLD)
[ "$NOLD" -le 1 ] || die "port $PORT already has $NOLD listeners ($OLD); refusing a third. Clean up manually first."

SYSTEMD=0
if command -v systemctl >/dev/null 2>&1 \
   && [ "$($SUDO systemctl is-active "$SERVICE" 2>/dev/null || true)" = "active" ]; then
  SYSTEMD=1
fi
echo "  port: $PORT  old pid: ${OLD:-<none>}  mode: $([ "$SYSTEMD" = 1 ] && echo systemd-restart || echo overlap-bind)"

if [ "$SYSTEMD" = 1 ]; then
  # Smoke-test NEW on a throwaway port + data dir + throwaway admin
  # password (same provider env sourced above). The three [server] values
  # are FORCED whether or not the real config states them: a sed replace
  # silently no-ops when a key is commented out or absent, and then NEW
  # would grab the PRODUCTION port instead of a throwaway one. The awk
  # also appends a [server] table if the real config has none.
  SELFPORT=$((PORT + 1)); [ "$SELFPORT" -lt 65536 ] || SELFPORT=18099
  TMPD=$(mktemp -d)
  TESTCFG="$TMPD/onegw-selftest.toml"
  TESTPW=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
  awk -v sp="$SELFPORT" -v dd="$TMPD/data" -v pw="$TESTPW" '
    BEGIN { inServer = 0; seenServer = 0; gotL = 0; gotD = 0; gotP = 0 }
    function flushServer() {
      if (!inServer) return
      if (!gotL) print "listen = \"127.0.0.1:" sp "\""
      if (!gotD) print "data_dir = \"" dd "\""
      if (!gotP) print "admin_password = \"" pw "\""
      inServer = 0
    }
    {
      if ($0 ~ /^[[:space:]]*\[/) {
        flushServer()
        if ($0 ~ /^[[:space:]]*\[server\][[:space:]]*$/) { inServer = 1; seenServer = 1 }
        print; next
      }
      if (inServer) {
        if ($0 ~ /^[[:space:]]*listen[[:space:]]*=/) { print "listen = \"127.0.0.1:" sp "\""; gotL = 1; next }
        if ($0 ~ /^[[:space:]]*data_dir[[:space:]]*=/) { print "data_dir = \"" dd "\""; gotD = 1; next }
        if ($0 ~ /^[[:space:]]*admin_password[[:space:]]*=/) { print "admin_password = \"" pw "\""; gotP = 1; next }
      }
      print
    }
    END {
      flushServer()
      if (!seenServer) {
        print ""; print "[server]"
        print "listen = \"127.0.0.1:" sp "\""; print "data_dir = \"" dd "\""; print "admin_password = \"" pw "\""
      }
    }' "$CONFIG" > "$TESTCFG" || { rm -rf "$TMPD"; die "selftest config rewrite failed"; }
  setsid "$NEW_PATH" -config "$TESTCFG" </dev/null >/dev/null 2>&1 &
  TESTPID=$!
  TESTOK=0
  for i in $(seq 1 20); do
    # Authenticated against the THROWAWAY password: a 200 from
    # /admin/health can only come from OUR process, not from whatever else
    # may live on the selftest port.
    if printf 'X-Admin-Password: %s\n' "$TESTPW" \
       | curl -sSf -H @- -o /dev/null --max-time 2 "http://127.0.0.1:$SELFPORT/admin/health" 2>/dev/null; then
      TESTOK=1; break
    fi
    sleep 0.5
  done
  kill -TERM "$TESTPID" 2>/dev/null || true
  # A stray that ignores TERM must not squat on the selftest port for the
  # next deploy; escalate once after a grace period.
  sleep 1
  if kill -0 "$TESTPID" 2>/dev/null; then
    kill -KILL "$TESTPID" 2>/dev/null || true
  fi
  wait "$TESTPID" 2>/dev/null || true
  rm -rf "$TMPD"
  [ "$TESTOK" = 1 ] || die "NEW failed the throwaway-port self-test; live service untouched"
  echo "  self-test ok (127.0.0.1:$SELFPORT, throwaway data dir)"

  # Swap the canonical binary. Never touch $FINAL until NEW proved itself;
  # if the snapshot/swap itself fails, the live service is still running
  # the old inode and we abort before any restart.
  if ! $SUDO cp -f "$FINAL" "$FINAL.prev" 2>/dev/null; then
    warn "no previous binary to snapshot (first deploy?)"
  fi
  if ! $SUDO mv -f "$NEW_PATH" "$FINAL"; then
    die "canonical swap failed; live service untouched (still running the old inode)"
  fi

  $SUDO systemctl restart "$SERVICE"
  echo "  systemctl restart issued; polling health"
  OK=0
  for i in $(seq 1 60); do
    if health; then OK=$((OK + 1)); [ "$OK" -ge 8 ] && break; else OK=0; fi
    sleep 0.5
  done
  if [ "$OK" -lt 8 ]; then
    echo "  swapped binary never became healthy." >&2
    echo "  Rollback: $SUDO mv -f $FINAL.prev $FINAL && $SUDO systemctl restart $SERVICE" >&2
    die "post-restart health never ok"
  fi
  AFTER=$(listeners); NAFTER=$(count $AFTER)
  [ "$NAFTER" = 1 ] || die "expected 1 listener after restart, got $NAFTER ($AFTER)"
  echo "  health ok; serving pid: $AFTER"
else
  # Overlap-bind path (no active unit): mirror scripts/deploy.sh.
  [ -n "$OLD" ] || echo "  WARN: no live listener on port $PORT (cold start)"
  if command -v setsid >/dev/null 2>&1; then
    setsid "$NEW_PATH" -config "$CONFIG" </dev/null >>/tmp/onegw-new.log 2>&1 &
  else
    # util-linux setsid missing; perl is Essential on Debian/Ubuntu.
    perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' -- "$NEW_PATH" -config "$CONFIG" \
      </dev/null >>/tmp/onegw-new.log 2>&1 &
  fi
  NEW_PID=$!
  sleep 1
  kill -0 "$NEW_PID" 2>/dev/null || die "NEW (pid $NEW_PID) exited immediately; old listener untouched"
  echo "  NEW_PID=$NEW_PID (detached, own session)"

  # Poll NEW health BEFORE touching OLD: connections round-robin across the
  # two SO_REUSEPORT listeners, so require several consecutive oks.
  OK=0
  for i in $(seq 1 60); do
    if health; then OK=$((OK + 1)); [ "$OK" -ge 8 ] && break; else OK=0; fi
    sleep 0.5
  done
  if [ "$OK" -lt 8 ]; then
    kill -TERM "$NEW_PID" 2>/dev/null || true
    die "NEW never became healthy; drained NEW, OLD listener untouched"
  fi
  echo "  health ok"

  # Atomic canonical swap (same filesystem) BEFORE the drain: rename(2)
  # does not disturb running processes — OLD keeps its inode, NEW is now
  # reachable at $FINAL for the next restart.
  if ! $SUDO cp -f "$FINAL" "$FINAL.prev" 2>/dev/null; then
    warn "no previous binary to snapshot (first deploy?)"
  fi
  if ! $SUDO mv -f "$NEW_PATH" "$FINAL"; then
    # Swap failed: roll the takeover back cleanly rather than leaving two
    # listeners. NEW was verified healthy but cannot be installed, so the
    # OLD binary stays canonical and serving.
    kill -TERM "$NEW_PID" 2>/dev/null || true
    die "canonical swap failed; drained NEW, OLD listener untouched (live service unchanged)"
  fi

  # ONLY NOW stop the old listener. Explicit pid, SIGTERM (graceful 30s
  # drain), never SIGKILL, never pkill -f.
  if [ -n "$OLD" ]; then
    echo "  SIGTERM old listener $OLD"
    kill -TERM "$OLD" 2>/dev/null || die "failed to signal old pid $OLD (permissions?)"
  fi

  # Kill -> verify -> re-kill: loop until ONLY NEW remains (a supervisor
  # respawn or a stale listener is re-TERMed).
  DONE=0
  for i in $(seq 1 70); do
    NOW=$(listeners)
    if [ -z "$NOW" ]; then sleep 0.5; continue; fi
    if printf '%s\n' $NOW | grep -qx "$NEW_PID" && [ "$(count $NOW)" = 1 ]; then
      DONE=1; break
    fi
    STRAY=$(printf '%s\n' $NOW | grep -vx "$NEW_PID" | grep . || true)
    if [ -n "$STRAY" ]; then
      echo "  respawn/stale listener(s): $STRAY; re-sending SIGTERM"
      for p in $STRAY; do kill -TERM "$p" 2>/dev/null || true; done
    fi
    sleep 0.5
  done
  [ "$DONE" = 1 ] || die "expected only NEW_PID $NEW_PID listening; last state: $(listeners | tr '\n' ' ')"
  kill -0 "$NEW_PID" 2>/dev/null || die "NEW died during cutover"
  echo "  single listener: $NEW_PID"
fi

# --- final: health twice (separate observations), then record state.
health || die "final health check #1 failed"
sleep 2
health || die "final health check #2 failed"
echo "DEPLOY COMPLETE $(date -u '+%Y-%m-%dT%H:%M:%SZ') port=$PORT pid=$(listeners | head -1) binary=$FINAL config=$CONFIG mode=$([ "$SYSTEMD" = 1 ] && echo systemd || echo overlap)"
REMOTE_EOF

# --- 1. build NEW from git archive HEAD (never the working tree) ------------
NEW_BIN="$BIN"
if [ -z "$NEW_BIN" ]; then
  command -v go >/dev/null 2>&1 \
    || die "go not found locally (or pass --binary with a prebuilt linux binary)"
  step "1. build NEW (git archive HEAD -> linux/${ARCH:-amd64})"
  git archive HEAD | tar -x -C "$STAGE"
  NEW_BIN="$STAGE/onegw-linux"
  if [ "$DRY_RUN" = 1 ]; then
    echo "  [dry-run] (cd $STAGE && CGO_ENABLED=0 GOOS=linux GOARCH=${ARCH:-amd64} go build -trimpath -ldflags '-s -w' -o onegw-linux ./cmd/onegw)"
  else
    (cd "$STAGE" && CGO_ENABLED=0 GOOS=linux GOARCH="${ARCH:-amd64}" \
      go build -trimpath -ldflags "-s -w" -o "$NEW_BIN" ./cmd/onegw) \
      || die "cross build failed"
  fi
else
  [ -x "$NEW_BIN" ] || die "binary not executable: $NEW_BIN"
  step "1. using prebuilt linux binary $NEW_BIN"
fi

# --- 2. remote pre-flight: reachability, arch, config, dest writability -----
step "2. remote pre-flight"
if [ "$DRY_RUN" = 1 ]; then
  echo "  [dry-run] ssh ${SSH_OPTS[*]} $HOST 'uname -m; test -f $CONFIG; test -w $DEST'"
  REMOTE_ARCH="${ARCH:-amd64}"
else
  REMOTE_ARCH_UNAME=$(ssh "${SSH_OPTS[@]}" "$HOST" 'uname -m') || die "ssh $HOST failed"
  case "$REMOTE_ARCH_UNAME" in
    x86_64)  REMOTE_ARCH="amd64" ;;
    aarch64) REMOTE_ARCH="arm64" ;;
    *) die "unsupported remote arch: $REMOTE_ARCH_UNAME (override with --arch)" ;;
  esac
  [ -n "$ARCH" ] && [ "$ARCH" != "$REMOTE_ARCH" ] && die "--arch $ARCH does not match remote $REMOTE_ARCH"
  ssh "${SSH_OPTS[@]}" "$HOST" "test -f '$CONFIG'" || die "remote config not found: $CONFIG"
  ssh "${SSH_OPTS[@]}" "$HOST" "test -d '$DEST' && test -w '$DEST'" \
    || die "remote dest $DEST missing or not writable by the ssh user (try --sudo or connect as root)"
  echo "  remote arch: $REMOTE_ARCH; config: $CONFIG; dest: $DEST"
fi
if [ -z "$ARCH" ]; then ARCH="$REMOTE_ARCH"; fi

# --- 3. upload NEW to a temp path on the destination filesystem -------------
# Same directory as the final binary => rename(2) swap stays atomic.
NEW_NAME=".onegw.new.$$"
step "3. upload NEW binary -> $HOST:$DEST/$NEW_NAME"
if [ "$DRY_RUN" = 1 ]; then
  echo "  [dry-run] scp $NEW_BIN $HOST:$DEST/$NEW_NAME"
else
  scp -q "${SSH_OPTS[@]}" "$NEW_BIN" "$HOST:$DEST/$NEW_NAME" || die "scp failed"
  ssh "${SSH_OPTS[@]}" "$HOST" "chmod 0755 '$DEST/$NEW_NAME'" || die "chmod remote binary failed"
fi

# --- 4. takeover (one remote bash -s; script piped, values as args) ---------
step "4. remote takeover"
if [ "$DRY_RUN" = 1 ]; then
  echo "  [dry-run] ssh $HOST bash -s -- '$CONFIG' '$DEST' '$NEW_NAME' '$([ "$SUDO_MODE" = 1 ] && echo sudo -n || echo)' '$SERVICE'"
  sed 's/^/  | /' "$STAGE/takeover.sh" >&2
  echo "== DRY-RUN COMPLETE (nothing was touched) =="
  exit 0
fi
SUDO=""; [ "$SUDO_MODE" = 1 ] && SUDO="sudo -n"
ssh "${SSH_OPTS[@]}" "$HOST" bash -s -- "$CONFIG" "$DEST" "$NEW_NAME" "$SUDO" "$SERVICE" \
  < "$STAGE/takeover.sh" \
  || die "remote takeover failed (see messages above; old listener untouched unless the failure is past the drain step)"

echo "DEPLOY COMPLETE: $HOST — canonical binary $DEST/onegw (previous kept as onegw.prev)"
echo "  verify from your machine: curl -sSf https://gw.example.com/v1/models -H 'Authorization: Bearer <gateway-key>'"
