# xdev-server: gateway on Docker

A gateway host runs onegw as the **xdev-server** provider: OpenCode Zen Go
(account credential from the environment) plus OpenCode Zen Free
(keyless, currently vendor-gated). Clients reach it at
`http://<gateway-host>:8080/v1`.

This overlay does not rewrite the image or `docker-compose.yml`. It
bind-mounts `docker/xdev-server.toml` (no secrets) over the image default
and puts keys in compose `.env` (gitignored, 0600).

## First deploy (from this tree)

```sh
export ONEGW_PROVIDER_OPENCODE_KEY=sk-...   # OpenCode Zen Go credential
./scripts/deploy_xdev_server.sh --host you@gateway-host
```

`--host` is required: nothing about a particular machine is baked into
this repo. Set `ONEGW_DEPLOY_HOST` instead if you would rather not repeat
the flag.

The script rsyncs compose + overlay, mints `ONEGW_KEYS` / admin password
once, `docker compose up -d`, and curls the dashboard root. Re-runs keep
the remote `.env` unless you pass `--rotate-keys`.

Flags: `--host H`, `--dir D`, `--port 8080`, `--loopback` (bind
127.0.0.1), `--rotate-keys`.

Equivalent by hand:

```sh
ssh "$HOST" 'mkdir -p ~/apps/onegw/docker'
rsync -az docker-compose.yml docker-compose.xdev-server.yml .env.example \
  "$HOST:~/apps/onegw/"
rsync -az docker/xdev-server.toml "$HOST:~/apps/onegw/docker/"
# write ~/apps/onegw/.env (0600) from .env.example — fill ONEGW_KEYS,
# ONEGW_ADMIN_PASSWORD, ONEGW_PROVIDER_OPENCODE_KEY, ONEGW_HOST_BIND=0.0.0.0
ssh "$HOST" 'cd ~/apps/onegw && docker compose -f docker-compose.yml -f docker-compose.xdev-server.yml up -d'
```

The image `ghcr.io/freepeak/onegw:latest` is pulled on the host. No Go
toolchain needed there.

## What the gateway exposes

| Client model | Upstream |
|---|---|
| `free` | combo: `opencode/deepseek-v4.1-flash` then `opencode/qwen3.8-flash` |
| `xdev` | combo: `opencode/deepseek-v4.1-flash` |
| `deepseek-v4.1-flash`, `qwen3.8-flash`, … | OpenCode Zen Go |
| `opencode-free/*` | listed on `/v1/models`; chat currently 403s (vendor gate) |

Zen Free (`kind = opencode-free`) was live-probed 2026-09-17: the
catalog lists `big-pickle` and `*-free` ids, but chat returns **403
FreeTierError** ("only from within OpenCode") and that 403 is not
combo-fallbackable, so `free` is the Go subscription. Keep the free
provider so a vendor thaw starts listing without a config edit.

## Clients

```sh
export XDEV_SERVER_URL=http://<gateway-host>:8080
export XDEV_SERVER_KEY=<ONEGW_KEYS from the remote .env>
xdev connect xdev-server --set-default
# or curl:
curl -sS "$XDEV_SERVER_URL/v1/models" -H "Authorization: Bearer $XDEV_SERVER_KEY"
```

Dashboard: `http://<gateway-host>:8080/` (admin password in the remote
`.env`). The overlay bind-mounts the toml read-only, so in-page config
saves will not persist — edit `docker/xdev-server.toml` and recreate, or
drop the overlay.

## Secrets

Never commit `.env`, `onegw.toml` with keys, or `ONEGW_PROVIDER_OPENCODE_KEY`.
Rotate the upstream key if it ever lands in git.
