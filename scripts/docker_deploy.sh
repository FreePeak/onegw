#!/usr/bin/env bash
# docker_deploy.sh — deploy (or update) onegw with Docker, correctly.
#
# Two back ends, same guarantees:
#
#   ./scripts/docker_deploy.sh                       # docker run, on :8080
#   ./scripts/docker_deploy.sh --compose             # docker compose, on :8080
#   ./scripts/docker_deploy.sh --compose --port 18080 --loopback
#   ./scripts/docker_deploy.sh --compose --build     # image from this tree
#   ./scripts/docker_deploy.sh --compose --with-search   # + local SearXNG
#   ONEGW_PROVIDER_XAI_KEY=... ./scripts/docker_deploy.sh
#   ./scripts/docker_deploy.sh --replace             # recreate, keeping config + data
#   ./scripts/docker_deploy.sh --image onegw:local   # use a locally built image
#
# What it does for you, each of which is a step someone otherwise gets wrong:
#
#   * gateway key      — reuses ONEGW_KEYS, or the value in the env file, or
#                        mints one with openssl and stores it (0600) so a
#                        re-run cannot rotate the key out from under clients
#   * provider env     — forwards every ONEGW_* variable you exported (provider
#                        keys, admin password) into the container — for compose,
#                        into the project's .env, so `docker compose ps`, `logs`
#                        and `down` keep working without re-typing the secret
#   * host port        — compose publishes $ONEGW_HOST_PORT instead of a
#                        hardcoded 8080, and an unset --port on a busy 8080
#                        moves to a free port: Docker does not always fail
#                        loudly there (macOS forwards may not bind at all), so a
#                        container that comes up "healthy" can still be
#                        unreachable while the HOST's own onegw answers on 8080
#   * config volume    — owned by the container user. The dashboard's provider
#                        editor saves straight into that file with a temp-file +
#                        rename, so without a writable directory every save
#                        answers 500 "temp file: open
#                        /etc/onegw/.onegw-config-*.toml: permission denied" —
#                        which is exactly what a fresh named volume does (Docker
#                        seeds it from the image but copies it root-owned)
#   * data volume      — /data, so usage.db, oauth-tokens.json and a generated
#                        admin password survive restarts, recreates and updates
#   * --replace        — copies the RUNNING container's config into the volume
#                        before removing it, so in-page edits are not reverted
#   * verification     — waits for /admin/health to answer 200 WITH THE REAL
#                        ADMIN PASSWORD on the published port, and cross-checks
#                        the container when it does not, so "a different process
#                        owns this port" is reported as that and not as a failed
#                        deploy
set -euo pipefail

MODE=run                      # run | compose
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
COMPOSE_DIR=${ONEGW_COMPOSE_DIR:-$(dirname -- "$SCRIPT_DIR")}
COMPOSE_FILE=$COMPOSE_DIR/docker-compose.yml
IMAGE=${ONEGW_IMAGE:-ghcr.io/freepeak/onegw:latest}
IMAGE_EXPLICIT=${ONEGW_IMAGE:+1}
NAME=${ONEGW_CONTAINER:-onegw}
PUBLISH=${ONEGW_PUBLISH:-8080:8080}
PUBLISH_SET=${ONEGW_PUBLISH:+1}   # --publish or $ONEGW_PUBLISH: --port then refuses to guess
HOST_PORT_WANTED=                 # --port; empty = the .env value, else 8080
HOST_BIND=0.0.0.0                 # --loopback sets 127.0.0.1; .env can pin either
PORT_SET=; LOOPBACK_SET=; NAME_SET=
BIND_SET=                              # HOST_BIND came from a flag or $ONEGW_PUBLISH
ENV_FILE_SET=${ONEGW_ENV_FILE:+1}
DATA_VOL=${ONEGW_DATA_VOLUME:-onegw-data}
CONFIG_VOL=${ONEGW_CONFIG_VOLUME:-onegw-config}
ENV_FILE=${ONEGW_ENV_FILE:-$PWD/onegw-deploy.env}
REPLACE=0
PULL=1
VERIFY=1
BUILD=0
PROFILES=()

say()  { printf '%s\n' "$*"; }
die()  { printf 'docker_deploy: %s\n' "$*" >&2; exit 1; }

usage() {
  # Everything from the shebang to the first non-comment line, minus the '# '.
  awk 'NR>1 { if (!/^#/) exit; sub(/^# ?/, ""); print }' "$0"
  cat <<'EOF'

Flags:
  --compose            drive docker compose (this repo's docker-compose.yml)
                       instead of `docker run`; settings land in the compose
                       project's .env, which every `docker compose` subcommand
                       then reads on its own
  --port PORT          published host port (compose: ONEGW_HOST_PORT in .env;
                       run: rewritten into the -p spec). On a busy port an
                       unset --port moves to a free one, an explicit one fails
  --build              build the image from this tree and pin it
                       (compose: onegw:compose; run: onegw:local)
  --with-search        compose only: also start the profile-gated local
                       SearXNG (point a searxng provider at http://searxng:8080)
  --project-dir DIR    compose project directory (default: this repo root)
  --image IMAGE        container image            (default $ONEGW_IMAGE or ghcr.io/freepeak/onegw:latest)
  --name NAME          container name             (default onegw)
  --publish SPEC       docker -p spec             (default 8080:8080)
  --loopback           publish on 127.0.0.1 only  (compose: ONEGW_HOST_BIND)
  --data-volume NAME   usage/token volume         (default onegw-data)
  --config-volume NAME config volume              (default onegw-config)
  --env-file FILE      env file for the container (default ./onegw-deploy.env;
                       compose mode always uses the project's .env)
  --replace            recreate the container, preserving its on-disk config
  --no-pull            do not pull the image first
  --no-verify          skip the health check
  -h, --help           this text
EOF
  exit 0
}

while [ $# -gt 0 ]; do
  case "$1" in
    --compose)         MODE=compose; shift ;;
    --port)            HOST_PORT_WANTED=${2:?}; PORT_SET=1; shift 2 ;;
    --build)           BUILD=1; shift ;;
    --with-search)     PROFILES+=(search); shift ;;
    --name)            NAME=${2:?}; NAME_SET=1; shift 2 ;;
    --image)           IMAGE=${2:?}; IMAGE_EXPLICIT=1; shift 2 ;;
    --publish)         PUBLISH=${2:?}; PUBLISH_SET=1; shift 2 ;;
    --loopback)        HOST_BIND=127.0.0.1; LOOPBACK_SET=1; BIND_SET=1; shift ;;
    --data-volume)     DATA_VOL=${2:?}; shift 2 ;;
    --config-volume)   CONFIG_VOL=${2:?}; shift 2 ;;
    --env-file)        ENV_FILE=${2:?}; ENV_FILE_SET=1; shift 2 ;;
    --replace)         REPLACE=1; shift ;;
    --no-pull)         PULL=0; shift ;;
    --no-verify)       VERIFY=0; shift ;;
    -h|--help)       usage ;;
    *)               die "unknown flag $1 (try --help)" ;;
  esac
done

command -v docker >/dev/null 2>&1 || die "docker not found in PATH"

# --- normalise the port/bind flags ------------------------------------------
# `--port` and `--loopback` are the friendly spelling of what run mode passes as
# a raw `-p` spec and compose mode writes into ONEGW_HOST_PORT / ONEGW_HOST_BIND.
if [ -n "$PUBLISH_SET" ] && { [ -n "$PORT_SET" ] || [ -n "$LOOPBACK_SET" ]; }; then
  die "--publish cannot be combined with --port/--loopback"
fi
if [ -z "$PUBLISH_SET" ]; then
  PUBLISH="${HOST_BIND}:${HOST_PORT_WANTED:-8080}:8080"
fi
if [ "$MODE" = compose ]; then
  # docker-compose.yml mounts the project's .env into the container, so that
  # file — not --env-file — is the one the stack reads. Settings therefore
  # survive into every later `docker compose` subcommand.
  [ -z "$ENV_FILE_SET" ] || say "note: compose mode always uses the project's .env; ignoring --env-file $ENV_FILE"
  ENV_FILE=$COMPOSE_DIR/.env
fi

# --- small helpers shared by both modes -------------------------------------
env_upsert() { # file KEY VALUE — set KEY=VALUE where it already sits (append if new)
  local f=$1 k=$2 v=$3 tmp
  [ -f "$f" ] || : >"$f"
  tmp=$(mktemp "${f}.tmp.XXXXXX")
  awk -v kv="$k=$v" -v key="$k=" '
    substr($0, 1, length(key)) == key { print kv; seen = 1; next }
    { print }
    END { if (!seen) print kv }' "$f" >"$tmp"
  chmod 600 "$tmp" && mv "$tmp" "$f"
}

port_listener() { # a name for whatever LISTENs on host port $1 ("" when unknown)
  command -v lsof >/dev/null 2>&1 || return 0
  lsof -nP "-iTCP:$1" -sTCP:LISTEN 2>/dev/null | awk 'NR>1 { print $1 " (pid " $2 ")"; exit }'
}

port_busy() { # 0 when something on this host already answers TCP $1
  [ -n "$(port_listener "$1")" ] && return 0
  curl -s -o /dev/null -m 1 "http://127.0.0.1:$1/"
}

free_port() { # the first unclaimed TCP port from $1 upward
  local p=$1 top
  top=$((p + 50))
  while [ "$p" -lt "$top" ]; do
    if ! port_busy "$p"; then printf '%s\n' "$p"; return 0; fi
    p=$((p + 1))
  done
  return 1
}

compose_cli() { (cd "$COMPOSE_DIR" && docker compose "$@"); }

our_published_ports() { # host ports this compose project already publishes
  local ids
  ids=$(compose_cli ps -q 2>/dev/null | tr '\n' ' ')
  [ -n "${ids// /}" ] || return 0
  docker inspect \
    -f '{{range $p, $b := .NetworkSettings.Ports}}{{range $b}}{{println .HostPort}}{{end}}{{end}}' \
    ${ids} 2>/dev/null | sort -u
}

# --- compose preflight: the project directory, .env, and a port that works ----
if [ "$MODE" = compose ]; then
  docker compose version >/dev/null 2>&1 || die \
    "docker compose v2 is unavailable (\"docker compose version\" failed) — drop --compose to use plain docker run mode"
  [ -f "$COMPOSE_FILE" ] || die \
    "no docker-compose.yml in $COMPOSE_DIR (pass --project-dir DIR or set ONEGW_COMPOSE_DIR)"

  if [ ! -f "$ENV_FILE" ] && [ -f "$COMPOSE_DIR/.env.example" ]; then
    (umask 077; cp "$COMPOSE_DIR/.env.example" "$ENV_FILE")
    say "created $ENV_FILE from .env.example"
  fi
  [ -f "$ENV_FILE" ] || (umask 077; : >"$ENV_FILE")
  chmod 600 "$ENV_FILE" 2>/dev/null || true

  # `--publish SPEC` is the raw spelling of the same two settings; decompose it
  # here so the precedence below (and the health check) see a real host port and
  # bind instead of the 8080 default — otherwise `--publish 18111:8080` would
  # publish 18111 while the verification knocked on 8080.
  if [ -n "$PUBLISH_SET" ]; then
    IFS=':' read -r -a _pp <<< "$PUBLISH"
    case ${#_pp[@]} in
      3) HOST_BIND=${_pp[0]}; HOST_PORT_WANTED=${_pp[1]}; BIND_SET=1 ;;
      2) HOST_PORT_WANTED=${_pp[0]} ;;
      1) HOST_PORT_WANTED=${_pp[0]} ;;
    esac
    PORT_SET=1        # an operator-chosen port is never silently relocated
  fi

  # Precedence for the published port, the bind, the image and the container
  # name: an explicit flag beats what .env already says, which beats the
  # 8080 / 0.0.0.0 / ghcr / `onegw` default. So a re-run without --port keeps an
  # existing deployment exactly where the operator left it.
  env_get() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -1; }
  want_port=${HOST_PORT_WANTED:-$(env_get ONEGW_HOST_PORT)}
  want_port=${want_port:-8080}
  if [ -z "$LOOPBACK_SET" ] && [ -z "$BIND_SET" ]; then
    HOST_BIND=$(env_get ONEGW_HOST_BIND)
    HOST_BIND=${HOST_BIND:-0.0.0.0}
  fi
  if [ "$BUILD" = 1 ] && [ -z "$IMAGE_EXPLICIT" ]; then
    IMAGE=onegw:compose                       # tag the local build and pin it
  elif [ -z "$IMAGE_EXPLICIT" ]; then
    IMAGE=$(env_get ONEGW_IMAGE)
    IMAGE=${IMAGE:-ghcr.io/freepeak/onegw:latest}
  fi
  if [ -n "$IMAGE_EXPLICIT" ] || [ "$BUILD" = 1 ]; then
    env_upsert "$ENV_FILE" ONEGW_IMAGE "$IMAGE"
  fi
  if [ -n "$NAME_SET" ]; then
    env_upsert "$ENV_FILE" ONEGW_CONTAINER_NAME "$NAME"
  else
    NAME=$(env_get ONEGW_CONTAINER_NAME)
    NAME=${NAME:-onegw}
  fi

  # Publishing is the step Docker gets QUIETLY wrong: on macOS (Colima, Docker
  # Desktop) a host port another process already holds does not always fail the
  # container start, so the stack reports "healthy" while every request to
  # http://127.0.0.1:8080 is answered by THAT process — typically another onegw,
  # whose different admin password then reads as a broken install.
  if our_published_ports | grep -qx -- "$want_port"; then
    : # already this project's own port — recreate in place
  elif port_busy "$want_port"; then
    holder=$(port_listener "$want_port"); holder=${holder:-another process}
    if [ -n "$PORT_SET" ]; then
      die "host port $want_port is already served by $holder — free it, or pass a different --port"
    fi
    moved=$(free_port 18080) || die "no free host port found in 18080-18130; pass --port explicitly"
    say "host port $want_port is served by $holder, not by this project: Docker would still call"
    say "the container healthy while that process keeps answering on :$want_port."
    say "publishing on $moved instead (recorded in $ENV_FILE — change it with --port)."
    want_port=$moved
  fi
  env_upsert "$ENV_FILE" ONEGW_HOST_PORT "$want_port"
  env_upsert "$ENV_FILE" ONEGW_HOST_BIND "$HOST_BIND"
  PUBLISH="${HOST_BIND}:${want_port}:8080"
fi

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
      ONEGW_HOST_PORT|ONEGW_HOST_BIND|ONEGW_CONTAINER_NAME|ONEGW_BUILD_VERSION|ONEGW_COMPOSE_DIR) : ;;
      *)               INHERITED+=("$line") ;;
    esac
  done < <(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$NAME" 2>/dev/null | sed -n '/^ONEGW_/p')
  [ -n "${ONEGW_KEYS:-}" ] && say "inherited the running container's gateway key (no rotation)"
fi

# Write (or move) one assignment in the env file, leaving the rest untouched.
persist_key() { env_upsert "$ENV_FILE" ONEGW_KEYS "$1"; }

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

# --- carry the caller's ONEGW_* credentials ---------------------------------
# Run mode passes them as `-e`; compose mode records them in the project's .env,
# which docker-compose.yml mounts into the container — so the credentials are not
# tied to the shell that happened to start the stack, and `docker compose ps`,
# `logs` and `down` work afterwards without re-supplying anything.
HOST_KNOBS="ONEGW_IMAGE ONEGW_CONTAINER ONEGW_PUBLISH ONEGW_DATA_VOLUME ONEGW_CONFIG_VOLUME ONEGW_ENV_FILE ONEGW_HOST_PORT ONEGW_HOST_BIND ONEGW_CONTAINER_NAME ONEGW_BUILD_VERSION ONEGW_COMPOSE_DIR"
ENV_ARGS=()
while IFS= read -r v; do
  [ -n "$v" ] || continue
  case "$v" in
    ONEGW_KEYS) continue ;;                 # goes through the env file
  esac
  skip=0
  for k in $HOST_KNOBS; do
    if [ "$k" = "$v" ]; then skip=1; break; fi
  done
  [ "$skip" = 1 ] && continue
  if [ "$MODE" = compose ]; then
    env_upsert "$ENV_FILE" "$v" "${!v}"
  else
    ENV_ARGS+=(-e "$v=${!v}")
  fi
done < <(env | sed -n 's/^\(ONEGW_[A-Z0-9_]*\)=.*/\1/p')
# …plus whatever the container being replaced already had (its Env is the only
# copy of a provider key that was passed with -e and never written to disk).
for pair in "${INHERITED[@]:-}"; do
  v=${pair%%=*}
  [ -n "$v" ] || continue
  if [ "$MODE" = compose ]; then
    grep -q "^${v}=" "$ENV_FILE" || env_upsert "$ENV_FILE" "$v" "${pair#*=}"
    continue
  fi
  case " ${ENV_ARGS[*]:-} " in *" -e $v="*) continue ;; esac
  ENV_ARGS+=(-e "$v=${pair#*=}")
done

# ===========================================================================
# compose mode: hand the stack to docker compose
# ===========================================================================
if [ "$MODE" = compose ]; then
  compose_args=()
  for p in "${PROFILES[@]:-}"; do
    if [ -n "$p" ]; then compose_args+=(--profile "$p"); fi
  done
  up_args=(up -d)
  if [ "$BUILD" = 1 ]; then up_args+=(--build); fi
  if [ "$REPLACE" = 1 ]; then up_args+=(--force-recreate); fi

  # `docker compose up -d` never refreshes a tag it already has locally, so a
  # "deploy or update" entry point has to pull explicitly — otherwise a host that
  # pulled once keeps running that build forever and `onegw update` cannot help
  # (self-update is check-only in a container by design). When the registry is
  # unreachable — GHCR has done exactly this to a blob fetch while the rest of the
  # manifest resolved fine — falling back to the local copy beats refusing to
  # start, provided one exists.
  pull_args=()
  # Only a registry-qualified name can be pulled: once .env pins a tag built on
  # this host (--build writes ONEGW_IMAGE=onegw:compose), `compose pull` would
  # answer "pull access denied" for it and interrupt the other services' pulls.
  firstseg=${IMAGE%%/*}
  PULLABLE=0
  case "$firstseg" in
    *.*|localhost|localhost:*) PULLABLE=1 ;;
  esac
  if [ "$PULL" = 1 ] && [ "$BUILD" != 1 ] && [ "$PULLABLE" = 1 ]; then
    if compose_cli ${compose_args[@]+"${compose_args[@]}"} pull -q; then
      say "pulled $IMAGE"
    elif docker image inspect "$IMAGE" >/dev/null 2>&1; then
      say "WARNING: could not pull $IMAGE (registry or network); continuing with the copy already on this host — pass --no-pull to skip this step"
      pull_args=(--pull never)
    else
      pull_args=(--pull missing)   # nothing local to fall back on: let `up` report it
    fi
  elif [ "$PULL" = 0 ]; then
    pull_args=(--pull never)
  fi
  if [ ${#pull_args[@]} -gt 0 ]; then up_args+=("${pull_args[@]}"); fi

  say "starting the compose stack in $COMPOSE_DIR (image $IMAGE, publish ${HOST_BIND:-0.0.0.0}:${want_port}:8080)"

  # One bring-up + verification pass. Returns:
  #   0 healthy · 2 compose/`up` failed · 3 container not running
  #   4 the published port is answered by a DIFFERENT process (the container
  #     itself answers 200 with the same credential) · 5 slow or failed health
  bring_up() {
    if ! compose_cli ${compose_args[@]+"${compose_args[@]}"} "${up_args[@]}"; then return 2; fi
    CID=$(compose_cli ps -aq onegw | head -1)
    [ -n "$CID" ] || return 2
    if [ "$(docker inspect -f '{{.State.Status}}' "$CID")" != running ]; then return 3; fi
    HEALTH_URL="http://$HOST_IP:$want_port"

    # The admin password is minted on first boot INSIDE the container and lives on
    # the data volume, so it has to be read from there before health can be
    # checked — /admin/health is password-gated like every other admin endpoint.
    for _ in $(seq 1 20); do
      PW=$(docker exec "$CID" cat /data/admin_password 2>/dev/null || true)
      if [ -n "$PW" ]; then break; fi
      sleep 0.5
    done
    if [ "$VERIFY" != 1 ]; then return 0; fi

    OK=0; code=""
    for _ in $(seq 1 30); do
      if [ -n "$PW" ]; then
        code=$(curl -s -o /dev/null -w '%{http_code}' -H "X-Admin-Password: $PW" \
               "$HEALTH_URL/admin/health" || true)
        if [ "$code" = "200" ]; then OK=1; break; fi
      else
        code=$(curl -s -o /dev/null -w '%{http_code}' "$HEALTH_URL/" || true)
        case "$code" in 200|302|303) OK=1; break ;; esac
      fi
      sleep 1
    done
    if [ "$OK" = 1 ]; then
      # The other failure that hides behind a healthy container: a config volume
      # seeded root-owned makes every dashboard save answer
      # 500 "temp file: … permission denied". Reported in the same breath as the
      # health check, so it never surfaces as an unexplained UI bug.
      if docker exec "$CID" test -w /etc/onegw; then
        say "  /etc/onegw is writable by the gateway user — dashboard saves will land"
      else
        say "WARNING: /etc/onegw is NOT writable by the gateway user, so the dashboard's"
        say "        provider editor will answer 500 \"temp file: permission denied\"."
        say "        Re-run the ownership pass: docker compose up -d --force-recreate config-init"
      fi
      return 0
    fi
    inside=$(docker exec "$CID" curl -s -o /dev/null -w '%{http_code}' \
             -H "X-Admin-Password: $PW" http://127.0.0.1:8080/admin/health 2>/dev/null || true)
    if [ "$inside" = "200" ]; then return 4; fi
    return 5
  }

  attempts=0
  while :; do
    attempts=$((attempts + 1))
    rc=0
    bring_up || rc=$?
    if [ "$rc" = 0 ]; then break; fi

    if [ "$rc" = 4 ]; then
      holder=$(port_listener "$want_port"); holder=${holder:-another process on the host}
      if [ -z "$PORT_SET" ] && [ "$attempts" -lt 2 ]; then
        moved=$(free_port 18080) || die "no free host port found in 18080-18130; pass --port explicitly"
        say ""
        say "host port $want_port is answered by $holder, not by this stack — Docker mapped it"
        say "anyway (it does not always fail loudly), so the dashboard looks like a wrong"
        say "password. Recreating on $moved and recording it in $ENV_FILE."
        want_port=$moved
        env_upsert "$ENV_FILE" ONEGW_HOST_PORT "$want_port"
        continue
      fi
      say ""
      say "FAILED: the gateway answers inside its container, but http://$HOST_IP:$want_port"
      say "        returned HTTP ${code:-000} — that port belongs to $holder, not to this"
      say "        stack. Move the deployment:  $0 --compose --port <free port>"
      exit 1
    fi

    if [ "$rc" = 2 ]; then
      say ""
      say "compose up failed — service status and the last 30 log lines:"
      compose_cli ps -a || true
      compose_cli logs --tail 30 || true
      if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
        say ""
        say "$IMAGE is not available locally and could not be pulled (registry and CDN"
        say "outages happen; a stale tag can also fail this way). This tree builds the"
        say "same image:"
        say "  $0 --compose --build"
      fi
      die "see the output above"
    fi

    if [ "$rc" = 3 ]; then
      say "the gateway container is not running; last log lines:"
      docker logs --tail 20 "$CID" 2>&1 | sed 's/^/  /'
      say ""
      say "The usual cause is no gateway key with a 0.0.0.0 bind: onegw refuses to run as"
      say "an open proxy. $ENV_FILE carries ONEGW_KEYS for exactly that reason."
      die "onegw did not stay up"
    fi

    say ""
    say "WARNING: /admin/health did not answer 200 within 30s. Logs:"
    say "  docker compose logs --tail 40 onegw"
    break
  done

  # Report the volumes by the names this stack actually created (docker-compose.yml
  # pins them to onegw-data / onegw-config), not by a guessed prefix — these are
  # the names the backup commands in docs/vps-deploy.md use.
  DATAV=$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' "$CID")
  CONFGV=$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/etc/onegw"}}{{.Name}}{{end}}{{end}}' "$CID")

  # --- what to do next -------------------------------------------------------
  say ""
  say "onegw is deployed (docker compose project: onegw)"
  say "  dashboard   $HEALTH_URL/admin"
  say "  container   $(docker inspect -f '{{.Name}}' "$CID" | sed 's|^/||')  image $IMAGE"
  say "  settings    $ENV_FILE  (host port/bind, gateway key, provider credentials)"
  say "  config      volume $CONFGV -> /etc/onegw  (edit it in the dashboard: Providers page)"
  say "  data        volume $DATAV -> /data  (usage.db, oauth-tokens.json, admin_password)"
  if [ "$GENERATED_KEY" = 1 ]; then
    say "  gateway key $ONEGW_KEYS"
    say "              (also in $ENV_FILE — clients send it as 'Authorization: Bearer …')"
  fi
  if [ -n "$PW" ]; then
    say "  admin pass  $PW"
    say "              (minted on first boot; docker exec $NAME cat /data/admin_password)"
  else
    say "  admin pass  as configured in your .env/config"
  fi
  say ""
  say "configure it after setup, without rebuilding:"
  say "  providers / combos / savers / admin password   $HEALTH_URL/admin"
  say "  port, keys, upstream credentials               edit $ENV_FILE, then: docker compose up -d"
  say "  subscription sign-in                           docker exec -it $NAME onegw oauth login -provider xai -account <config account name>"
  if [ ${#PROFILES[@]} -gt 0 ]; then
    say "  web search                                   a searxng provider's base_url is http://searxng:8080 inside the network"
    say "  stop the search box too                      docker compose --profile search down   (a plain down skips profile services)"
  fi
  say "  update                                         $0 --compose   (pulls, recreates; both volumes keep their content)"
  say "  logs / stop                                    docker compose logs -f onegw / docker compose down"
  exit 0
fi

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
