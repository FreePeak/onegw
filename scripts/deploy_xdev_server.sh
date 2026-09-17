#!/usr/bin/env bash
# deploy_xdev_server.sh — rsync this tree's compose overlay to an SSH host and
# `docker compose up -d` it as the xdev-server gateway.
#
# Secrets never leave the operator's environment:
#   ONEGW_KEYS                    gateway client key (minted if missing)
#   ONEGW_PROVIDER_OPENCODE_KEY   OpenCode Zen Go account
#   ONEGW_ADMIN_PASSWORD          dashboard (minted if missing)
#
#   ONEGW_PROVIDER_OPENCODE_KEY=sk-... \
#     ./scripts/deploy_xdev_server.sh --host you@gateway-host
#
# No host is baked in — pass --host or set ONEGW_DEPLOY_HOST. The remote .env
# is 0600 and is never overwritten once present, so a re-run cannot rotate
# keys out from under live clients. Pass --rotate-keys only when you mean it.
set -euo pipefail

HOST=${ONEGW_DEPLOY_HOST:-}
REMOTE_DIR=${ONEGW_DEPLOY_DIR:-apps/onegw}
PORT=${ONEGW_HOST_PORT:-8080}
BIND=${ONEGW_HOST_BIND:-0.0.0.0}
ROTATE=0
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(dirname -- "$SCRIPT_DIR")

say() { printf '%s\n' "$*"; }
die() { printf 'deploy_xdev_server: %s\n' "$*" >&2; exit 1; }

usage() {
  awk 'NR>1 { if (!/^#/) exit; sub(/^# ?/, ""); print }' "$0"
  cat <<'EOF'

Flags:
  --host H     SSH target, e.g. you@gateway-host (or $ONEGW_DEPLOY_HOST; required)
  --dir  D     remote directory relative to $HOME (default: apps/onegw)
  --port N     published host port (default: 8080)
  --loopback   bind 127.0.0.1 instead of 0.0.0.0
  --rotate-keys  rewrite remote .env (otherwise an existing .env is kept)
EOF
}

while [ $# -gt 0 ]; do
  case $1 in
    --host) HOST=$2; shift 2 ;;
    --dir) REMOTE_DIR=$2; shift 2 ;;
    --port) PORT=$2; shift 2 ;;
    --loopback) BIND=127.0.0.1; shift ;;
    --rotate-keys) ROTATE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown flag $1 (see --help)" ;;
  esac
done

[ -n "$HOST" ] || die "no SSH target: pass --host you@gateway-host or set ONEGW_DEPLOY_HOST"

command -v ssh >/dev/null || die "ssh not found"
command -v rsync >/dev/null || die "rsync not found"
command -v openssl >/dev/null || die "openssl not found"

[ -f "$ROOT/docker-compose.yml" ] || die "no docker-compose.yml in $ROOT"
[ -f "$ROOT/docker/xdev-server.toml" ] || die "no docker/xdev-server.toml"
[ -f "$ROOT/docker-compose.xdev-server.yml" ] || die "no docker-compose.xdev-server.yml"

say "deploy_xdev_server: $HOST:~/$REMOTE_DIR  bind $BIND:$PORT"

ssh -o BatchMode=yes "$HOST" "mkdir -p ~/$REMOTE_DIR/docker"

rsync -az --delete \
  --exclude '.git/' \
  --exclude '.env' \
  --exclude 'onegw.toml' \
  --exclude 'data/' \
  --exclude '*.log' \
  "$ROOT/docker-compose.yml" \
  "$ROOT/docker-compose.xdev-server.yml" \
  "$ROOT/.env.example" \
  "$HOST:~/$REMOTE_DIR/"
rsync -az --inplace "$ROOT/docker/xdev-server.toml" "$HOST:~/$REMOTE_DIR/docker/"

# Seed .env once. Keys never travel in the rsync payload.
remote_env_exists=$(ssh -o BatchMode=yes "$HOST" "test -f ~/$REMOTE_DIR/.env && echo yes || echo no")
if [ "$remote_env_exists" = yes ] && [ "$ROTATE" -eq 0 ]; then
  say "keeping existing ~/$REMOTE_DIR/.env (pass --rotate-keys to rewrite)"
else
  [ -n "${ONEGW_PROVIDER_OPENCODE_KEY:-}" ] || die "export ONEGW_PROVIDER_OPENCODE_KEY (OpenCode Zen Go key) before the first deploy"
  GW_KEY=${ONEGW_KEYS:-$(openssl rand -hex 24)}
  ADMIN=${ONEGW_ADMIN_PASSWORD:-$(openssl rand -hex 16)}
  say "writing remote .env (0600); gateway key minted unless ONEGW_KEYS was set"
  ssh -o BatchMode=yes "$HOST" "cat > ~/$REMOTE_DIR/.env && chmod 600 ~/$REMOTE_DIR/.env" <<EOF
ONEGW_HOST_PORT=$PORT
ONEGW_HOST_BIND=$BIND
ONEGW_IMAGE=ghcr.io/freepeak/onegw:latest
ONEGW_CONTAINER_NAME=onegw
ONEGW_KEYS=$GW_KEY
ONEGW_ADMIN_PASSWORD=$ADMIN
ONEGW_PROVIDER_OPENCODE_KEY=$ONEGW_PROVIDER_OPENCODE_KEY
EOF
fi

ssh -o BatchMode=yes "$HOST" "chmod 644 ~/$REMOTE_DIR/docker/xdev-server.toml"
ssh -o BatchMode=yes "$HOST" "cd ~/$REMOTE_DIR && docker compose -f docker-compose.yml -f docker-compose.xdev-server.yml up -d --force-recreate --no-deps onegw"

# Wait for the published port. The image healthcheck hits the dashboard root.
say "waiting for published :$PORT"
ok=0
for i in 1 2 3 4 5 6 7 8 9 10; do
  if ssh -o BatchMode=yes "$HOST" "curl -fsS -m 3 http://127.0.0.1:$PORT/ >/dev/null"; then
    ok=1
    break
  fi
  sleep 2
done
[ "$ok" = 1 ] || die "gateway did not answer on :$PORT — ssh $HOST 'docker compose -f ~/$REMOTE_DIR/docker-compose.yml -f ~/$REMOTE_DIR/docker-compose.xdev-server.yml logs --tail 80'"

say "ok  dashboard http://$BIND:$PORT/  (clients: Authorization: Bearer \$ONEGW_KEYS)"
say "    xdev:  xdev connect xdev-server --set-default"
