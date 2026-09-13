#!/usr/bin/env bash
# docker_deploy.sh — deploy (or update) onegw as a Docker container, correctly.
#
#   ./scripts/docker_deploy.sh                       # deploy/refresh on 8080
#   ./scripts/docker_deploy.sh --loopback            # publish on 127.0.0.1 only
#   ONEGW_PROVIDER_XAI_KEY=... ./scripts/docker_deploy.sh
#   ./scripts/docker_deploy.sh --replace             # recreate, keeping config + data
#   ./scripts/docker_deploy.sh --image onegw:local   # use a locally built image
#
# What it does for you, each of which is a step someone otherwise gets wrong:
#
#   * gateway key      — reuses ONEGW_KEYS, or the value in onegw-deploy.env, or
#                        mints one with openssl and stores it (0600) so a
#                        re-run cannot rotate the key out from under clients
#   * provider env     — forwards every ONEGW_* variable you exported (provider
#                        keys, admin password) into the container
#   * config volume    — a named volume at /etc/onegw, chowned to the container
#                        user. The dashboard's provider editor saves straight
#                        into that file with a temp-file + rename, so without a
#                        writable directory every save answers 500
#                        "temp file: open /etc/onegw/.onegw-config-*.toml:
#                        permission denied"; without the volume those edits live
#                        on the writable layer and die with the container
#   * data volume      — /data, so usage.db, oauth-tokens.json and a generated
#                        admin password survive restarts, recreates and updates
#   * --replace        — copies the RUNNING container's config into the volume
#                        before removing it, so in-page edits are not reverted
#   * verification     — waits for /admin/health to answer with the real admin
#                        password and reports what it found
#
# Everything is idempotent: running it again on a healthy deployment reports
# status and changes nothing (pass --replace to rebuild the container).
set -euo pipefail

IMAGE=${ONEGW_IMAGE:-ghcr.io/freepeak/onegw:latest}
NAME=${ONEGW_CONTAINER:-onegw}
PUBLISH=${ONEGW_PUBLISH:-8080:8080}
DATA_VOL=${ONEGW_DATA_VOLUME:-onegw-data}
CONFIG_VOL=${ONEGW_CONFIG_VOLUME:-onegw-config}
ENV_FILE=${ONEGW_ENV_FILE:-$PWD/onegw-deploy.env}
REPLACE=0
PULL=1
VERIFY=1

say()  { printf '%s\n' "$*"; }
die()  { printf 'docker_deploy: %s\n' "$*" >&2; exit 1; }

usage() {
  sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'
  cat <<'EOF'

Flags:
  --image IMAGE        container image            (default $ONEGW_IMAGE or ghcr.io/freepeak/onegw:latest)
  --name NAME          container name             (default onegw)
  --publish SPEC       docker -p spec             (default 8080:8080)
  --loopback           shortcut for --publish 127.0.0.1:8080:8080
  --data-volume NAME   usage/token volume         (default onegw-data)
  --config-volume NAME config volume              (default onegw-config)
  --env-file FILE      env file for the container (default ./onegw-deploy.env)
  --replace            recreate the container, preserving its on-disk config
  --no-pull            do not pull the image first
  --no-verify          skip the health check
  -h, --help           this text
EOF
  exit 0
}

while [ $# -gt 0 ]; do
  case "$1" in
    --image)         IMAGE=${2:?}; shift 2 ;;
    --name)          NAME=${2:?}; shift 2 ;;
    --publish)       PUBLISH=${2:?}; shift 2 ;;
    --loopback)      PUBLISH=127.0.0.1:8080:8080; shift ;;
    --data-volume)   DATA_VOL=${2:?}; shift 2 ;;
    --config-volume) CONFIG_VOL=${2:?}; shift 2 ;;
    --env-file)      ENV_FILE=${2:?}; shift 2 ;;
    --replace)       REPLACE=1; shift ;;
    --no-pull)       PULL=0; shift ;;
    --no-verify)     VERIFY=0; shift ;;
    -h|--help)       usage ;;
    *)               die "unknown flag $1 (try --help)" ;;
  esac
done

command -v docker >/dev/null 2>&1 || die "docker not found in PATH"

# --- where the health check should actually knock ---------------------------
# Publish specs are [ip:]hostport:containerport. Splitting on ':' and taking the
# last field would give the CONTAINER port (8080), so a deployment on 18160
# would probe whatever else the host runs on 8080 — including a different
# gateway, whose 401 then looked like a failed deploy.
IFS=':' read -r -a _pub <<< "$PUBLISH"
case ${#_pub[@]} in
  3) HOST_IP=${_pub[0]}; HOST_PORT=${_pub[1]} ;;
  2) HOST_IP=0.0.0.0;    HOST_PORT=${_pub[0]} ;;
  *) HOST_IP=0.0.0.0;    HOST_PORT=${_pub[0]:-8080} ;;
esac
# 0.0.0.0 is not a connectable address; report the loopback in messages.
[ "$HOST_IP" = "0.0.0.0" ] && HOST_IP=127.0.0.1
HEALTH_URL="http://$HOST_IP:$HOST_PORT"

# --- gateway key: reuse, never rotate ---------------------------------------
# Credentials belong to the operator, not to this script: an install started with
# `-e ONEGW_KEYS=…` (or any provider key) keeps them in its container Env, and a
# --replace that minted a fresh key would lock every wired client out. Inherit
# them before generating anything.
INHERITED=()
if docker ps -a --format '{{.Names}}' | grep -qx "$NAME"; then
  while IFS= read -r line; do
    v=${line%%=*}
    case "$v" in
      ONEGW_KEYS)      ONEGW_KEYS=${line#ONEGW_KEYS=} ;;
      ONEGW_CONFIG|ONEGW_DATA_DIR) : ;;   # the image sets these itself
      ONEGW_IMAGE|ONEGW_CONTAINER|ONEGW_PUBLISH|ONEGW_DATA_VOLUME|ONEGW_CONFIG_VOLUME|ONEGW_ENV_FILE) : ;;
      *)               INHERITED+=("$line") ;;
    esac
  done < <(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$NAME" 2>/dev/null | sed -n '/^ONEGW_/p')
  [ -n "${ONEGW_KEYS:-}" ] && say "inherited the running container's gateway key (no rotation)"
fi

persist_key() {
  ( umask 077
    { [ -f "$ENV_FILE" ] && grep -v '^ONEGW_KEYS=' "$ENV_FILE" || true
      printf 'ONEGW_KEYS=%s\n' "$1"; } > "$ENV_FILE.tmp"
    mv "$ENV_FILE.tmp" "$ENV_FILE" )
}

GENERATED_KEY=0
if [ -z "${ONEGW_KEYS:-}" ] && [ -f "$ENV_FILE" ]; then
  ONEGW_KEYS=$(sed -n 's/^ONEGW_KEYS=//p' "$ENV_FILE" | tail -1)
fi
if [ -z "${ONEGW_KEYS:-}" ]; then
  if command -v openssl >/dev/null 2>&1; then
    ONEGW_KEYS=$(openssl rand -hex 24)
  else
    ONEGW_KEYS=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
  fi
  GENERATED_KEY=1
  persist_key "$ONEGW_KEYS"
  say "generated a gateway key and stored it in $ENV_FILE (0600) — clients authenticate with it"
elif [ ! -f "$ENV_FILE" ] || ! grep -q '^ONEGW_KEYS=' "$ENV_FILE"; then
  persist_key "$ONEGW_KEYS"
fi

# --- forward the caller's ONEGW_* credentials -------------------------------
ENV_ARGS=()
while IFS= read -r v; do
  [ -n "$v" ] || continue
  case "$v" in
    ONEGW_KEYS) continue ;;                 # goes through the env file
    ONEGW_IMAGE|ONEGW_CONTAINER|ONEGW_PUBLISH|ONEGW_DATA_VOLUME|ONEGW_CONFIG_VOLUME|ONEGW_ENV_FILE) continue ;;
  esac
  ENV_ARGS+=(-e "$v=${!v}")
done < <(env | sed -n 's/^\(ONEGW_[A-Z0-9_]*\)=.*/\1/p')
# …plus whatever the container being replaced already had (its Env is the only
# copy of a provider key that was passed with -e and never written to disk).
for pair in "${INHERITED[@]:-}"; do
  v=${pair%%=*}
  [ -n "$v" ] || continue
  case " ${ENV_ARGS[*]:-} " in *" -e $v="*) continue ;; esac
  ENV_ARGS+=(-e "$v=${pair#*=}")
done

if [ "$PULL" = 1 ] && [ "${IMAGE%%/*}" != "onegw" ]; then
  say "pulling $IMAGE"
  docker pull -q "$IMAGE" >/dev/null
fi

# --- an existing deployment --------------------------------------------------
EXISTS=0
if docker ps -a --format '{{.Names}}' | grep -qx "$NAME"; then
  EXISTS=1
fi
if [ "$EXISTS" = 1 ] && [ "$REPLACE" = 0 ]; then
  if [ "$(docker inspect -f '{{.State.Running}}' "$NAME")" = "true" ]; then
    say "container $NAME is already running — nothing to do"
    say "  recreate it with: $0 --replace"
    say "  dashboard:        $HEALTH_URL/admin"
    exit 0
  fi
  say "container $NAME exists but is not running; recreating it"
  REPLACE=1
fi

# --- config volume: preserved config, owned by the container user -----------
docker volume inspect "$CONFIG_VOL" >/dev/null 2>&1 || {
  docker volume create "$CONFIG_VOL" >/dev/null
  say "created config volume $CONFIG_VOL"
}
if [ "$EXISTS" = 1 ] && [ "$REPLACE" = 1 ]; then
  _tmp=$(mktemp -d)
  if docker cp "$NAME:/etc/onegw/onegw.toml" "$_tmp/onegw.toml" 2>/dev/null; then
    # Keep edits that live in the running container's writable layer (an install
    # from before the config volume existed): seed them into the volume FIRST so
    # the recreate starts from the operator's file, not the image default.
    #
    # The seed goes through a RUNNING helper container's mount namespace (docker
    # cp writes through its mounts) instead of a host bind mount: Docker Desktop
    # and Colima share only $HOME, so a $TMPDIR path mounts EMPTY and the copy
    # would fail — silently skipping the migration.
    _seed="$NAME-seed-$$"
    docker rm -f "$_seed" >/dev/null 2>&1 || true
    docker run -d --name "$_seed" -v "$CONFIG_VOL:/etc/onegw" alpine sleep 120 >/dev/null
    docker cp "$_tmp/onegw.toml" "$_seed:/etc/onegw/onegw.toml"
    kept=$(docker exec "$_seed" sh -c 'grep -c "^\[\[providers\]\]" /etc/onegw/onegw.toml || true')
    docker rm -f "$_seed" >/dev/null
    [ -n "$kept" ] || die "could not carry the running config into $CONFIG_VOL (nothing copied)"
    say "carried the running config into $CONFIG_VOL ($kept providers)"
  fi
  rm -rf "$_tmp"
  docker rm -f "$NAME" >/dev/null
fi
# The atomic config save needs a writable DIRECTORY (uid 100 in the image).
docker run --rm -u 0 --entrypoint chown -v "$CONFIG_VOL:/etc/onegw" "$IMAGE" \
  -R onegw:onegw /etc/onegw >/dev/null

# --- data volume -------------------------------------------------------------
docker volume inspect "$DATA_VOL" >/dev/null 2>&1 || {
  docker volume create "$DATA_VOL" >/dev/null
  say "created data volume $DATA_VOL"
}

# --- run ---------------------------------------------------------------------
say "starting $NAME from $IMAGE (publish $PUBLISH)"
RUN_ARGS=(-d --name "$NAME" --restart unless-stopped -p "$PUBLISH"
          -v "$DATA_VOL:/data" -v "$CONFIG_VOL:/etc/onegw")
[ -f "$ENV_FILE" ] && RUN_ARGS+=(--env-file "$ENV_FILE")
RUN_ARGS+=("${ENV_ARGS[@]}" "$IMAGE")
docker run "${RUN_ARGS[@]}" >/dev/null

# --- verify ------------------------------------------------------------------
PW=""
for _ in $(seq 1 20); do
  PW=$(docker exec "$NAME" cat /data/admin_password 2>/dev/null || true)
  [ -n "$PW" ] && break
  sleep 0.5
done

OK=0
if [ "$VERIFY" = 1 ]; then
  for _ in $(seq 1 30); do
    if [ -n "$PW" ]; then
      code=$(curl -s -o /dev/null -w '%{http_code}' -H "X-Admin-Password: $PW" \
             "$HEALTH_URL/admin/health" || true)
      [ "$code" = "200" ] && { OK=1; break; }
    else
      code=$(curl -s -o /dev/null -w '%{http_code}' "$HEALTH_URL/" || true)
      case "$code" in 200|302|303) OK=1; break ;; esac
    fi
    sleep 1
  done
fi

say ""
say "onegw is deployed"
say "  container   $NAME   ($(docker inspect -f '{{.State.Status}}' "$NAME"))"
say "  dashboard   $HEALTH_URL/admin"
say "  data        volume $DATA_VOL  -> /data      (usage.db, oauth-tokens.json, admin_password)"
say "  config      volume $CONFIG_VOL -> /etc/onegw (edit it in the dashboard: Providers page)"
if [ "$GENERATED_KEY" = 1 ]; then
  say "  gateway key $ONEGW_KEYS"
  say "              (also in $ENV_FILE — clients send it as 'Authorization: Bearer …')"
fi
if [ -n "$PW" ]; then
  say "  admin pass  $PW"
  say "              (minted on first boot; stored in the data volume)"
else
  say "  admin pass  as configured in your env/config"
fi
if [ "$VERIFY" = 1 ] && [ "$OK" = 0 ]; then
  say ""
  say "WARNING: /admin/health did not answer 200 within 30s. Logs:"
  say "  docker logs $NAME | tail -20"
fi

# --- what to do next ---------------------------------------------------------
say ""
if docker exec "$NAME" onegw help 2>/dev/null | grep -q 'oauth'; then
  say "subscription (OAuth) accounts:"
  say "  docker exec -it $NAME onegw oauth list"
  say "  docker exec -it $NAME onegw oauth login -provider xai -account <the account name in your config>"
else
  say "this image predates the \`onegw oauth\` subcommand, so the CLI login is unavailable in it."
  say "build the current tree and redeploy to get it (plus the dashboard Sign-in button):"
  say "  docker build -t onegw:local . && $0 --image onegw:local --replace"
fi
say ""
say "back up (config + data, both volumes): see docs/vps-deploy.md § Container installs"
