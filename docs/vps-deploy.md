# onegw on a personal VPS (omp + onegw remote deploy)

Runbook for hosting onegw on a personal VPS and pointing omp (or any
OpenAI-compatible client) at it. Everything scriptable is shipped in-repo;
the steps that need the actual VPS are collected in
[§12 Blocked on VPS access](#12-blocked-on-vps-access).

Verified against this tree: flags and paths below are code-checked
(`internal/config/config.go`, `internal/server/server.go`,
`cmd/onegw/main.go`) — do not substitute "close enough" names.

## 1. Topology

```
 laptop / other clients                        VPS
 +-----------------+   HTTPS    +----------------------------------------+
 | omp             | ---------> | :443 Caddy/nginx (TLS termination)     |
 |  models.yml     |            |   -> 127.0.0.1:8080 onegw (systemd)    |
 +-----------------+            |            |                           |
                                |            v                           |
                                |      upstreams (b-ai, GLM, ...)        |
                                +----------------------------------------+
```

- onegw listens on **127.0.0.1:8080 only** — the repo default. TLS and any
  public exposure belong to the reverse proxy. onegw itself has no TLS
  listener; never bind `0.0.0.0:8080` on a VPS without a proxy in front.
- Admin surface (`/admin`, `/admin/health`, `/admin/events` SSE,
  `/metrics`) is behind the same loopback bind; reach it via ssh tunnel
  (§10) or the proxy (§5).
- Resource envelope: onegw self-tunes to the 100 MB contract (90 MiB soft
  heap via `debug.SetMemoryLimit`, GOMAXPROCS capped at 4 —
  `cmd/onegw/main.go applyMemoryTuning`). The unit adds a MemoryHigh
  throttle and a MemoryMax backstop (§4).

Prereqs: Debian 12 / Ubuntu 22.04+ (systemd ≥ 250 for the unit's
hardening set), a DNS name for the gateway, key-based ssh access, 512 MB
RAM is comfortable.

## 2. Provision

```bash
ssh root@YOUR_VPS
useradd -r -s /usr/sbin/nologin onegw
install -d -o onegw -g onegw /var/lib/onegw
install -d -m 0750 -o root -g onegw /etc/onegw
ufw allow OpenSSH && ufw allow 80,443/tcp && ufw enable   # or nftables equivalent
```

## 3. Config: the exact onegw.toml delta for a VPS

`/etc/onegw/onegw.toml` (chown root:onegw, chmod 0640 — the service user
must read it; no secrets in this file):

```toml
[server]
listen = "127.0.0.1:8080"        # loopback; the proxy owns the public port
data_dir = "/var/lib/onegw"      # absolute — the unit's ReadWritePaths and
                                 # deploy_vps.sh both require it (a relative
                                 # data_dir is rejected by the deploy script)
admin_password = ""              # leave empty: comes from ONEGW_ADMIN_PASSWORD
                                 # in the env file; with neither set, first
                                 # boot generates a random password, stores it
                                 # in /var/lib/onegw/admin_password and logs it
                                 # once (the guessable "admin" in-code default
                                 # is gone — still set it explicitly).
# max_body_bytes (32 MiB) / buffered_budget_bytes (48 MiB) defaults are fine;
# raise response_header_timeout (e.g. "120s") for massive thinking prefills
# over slow links.

[auth]
keys = ["<GENERATED_GATEWAY_KEY>"]   # what clients send as their API key.
                                     # Policy form also works: [[auth.keys]]
                                     # with key/name/rpm/tpm/models.

[update]
check_interval = "24h"           # keep release notifications
auto = false                     # on a VPS let scripts/systemd own restarts;
                                 # auto=true does a zero-drop handoff that
                                 # leaves the serving process outside systemd
repo = "FreePeak/onegw"
```

Provider/`[[combo]]` sections are unchanged from
[onegw.toml.example](../onegw.toml.example) — same file works, point it at
the same upstreams.

`/etc/onegw/onegw.env` (chown root:root, chmod 0600 — systemd reads the
EnvironmentFile as root before dropping privileges; the service never
opens it):

```bash
ONEGW_ADMIN_PASSWORD=<long random>
# provider keys, mapped from the [[providers]] name:
#   name -> ONEGW_PROVIDER_<NAME>_KEY with NAME uppercased and "-" -> "_"
ONEGW_PROVIDER_B_AI_KEY=...        # provider "b-ai"
ONEGW_PROVIDER_OPENCODE_KEY=...    # + _KEY2.._KEY9 for extra accounts
ONEGW_KEYS=<gateway-key>           # alternative to [auth] keys in the toml
```

Notes:

- `data_dir = "memory"` disables persistence (no usage.db, no owner.json);
  do not use it on a VPS.
- Dashboard login sessions live in memory: a restart (deploy) logs
  dashboard users out. That is expected; usage data is in SQLite, not
  sessions.

### owner.json behavior

At startup onegw writes `<data_dir>/owner.json` (pid, listen, start time,
config path + mtime, argv, binary build stamp) and mirrors it live in
`GET /admin/health`'s `owner` block. It is re-stamped on every successful
SIGHUP reload and deliberately **not** removed on exit — a stale pid from
a crashed predecessor is evidence. On the VPS it answers "which instance
is canonical" without process archaeology:

```bash
jq . /var/lib/onegw/owner.json        # pid, listen, started_at, config, build
curl -s -H "X-Admin-Password: $PW" http://127.0.0.1:8080/admin/health | jq .owner
```

## 4. systemd unit

```bash
cp contrib/systemd/onegw.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now onegw
systemctl status onegw                      # expect active (running)
curl -s -H "X-Admin-Password: $PW" http://127.0.0.1:8080/admin/health | jq .
```

What the unit enforces (full list in the file):

| Flag | Why |
|---|---|
| `User=onegw`, `NoNewPrivileges`, `CapabilityBoundingSet=` | non-root, no privilege ladder |
| `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp` | read-only FS; only `/var/lib/onegw` writable (`StateDirectory` + `ReadWritePaths`) |
| `EnvironmentFile=/etc/onegw/onegw.env` (no leading `-`) | missing env file fails the start loudly instead of a masked env-gap crash loop |
| `Restart=on-failure`, `RestartSec=5s`, `KillSignal=SIGTERM`, `TimeoutStopSec=35s` | crash recovery without fighting deliberate stops; 35s > onegw's 30s in-flight drain deadline |
| `MemoryDenyWriteExecute`, `RestrictAddressFamilies`, `SystemCallArchitectures=native`, `ProtectKernel*` | static Go binary needs none of those capabilities |

`SO_REUSEPORT` is set by the gateway itself (`cmd/onegw/main.go`), which
is what makes the zero-drop deploy in §6 possible on the same host.

## 5. TLS reverse proxy

### Caddy (recommended: automatic Let's Encrypt)

```caddyfile
# /etc/caddy/Caddyfile
gw.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Caddy flushes streamed responses immediately; nothing else is needed for
SSE or streamed completions.

### nginx

```nginx
server {
    listen 443 ssl http2;
    server_name gw.example.com;
    ssl_certificate     /etc/letsencrypt/live/gw.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/gw.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_buffering off;           # SSE (/admin/events) + streamed completions
        proxy_read_timeout 3600s;      # thinking models stream for minutes
        proxy_send_timeout 3600s;
        client_max_body_size 40m;      # > onegw's 32 MiB max_body_bytes, or nginx 413s first
    }
}
```

Listen hardening rule: the public listener is the proxy (443). onegw stays
on loopback. If you absolutely must expose 8080 directly, put TLS on the
proxy anyway — onegw has no TLS support and the admin surface must not be
plaintext on the internet.

Admin auth (shipped, #2/#45): the dashboard logs in at `POST /admin/login`
and rides a `onegw_admin` session cookie; scripted access uses the
`X-Admin-Password` header (constant-time compare; `?password=` is not a
thing). Unauthenticated requests get 401.

## 6. Deploys: scripts/deploy_vps.sh

```bash
# from your workstation, in a onegw checkout:
scripts/deploy_vps.sh --host root@YOUR_VPS                 # build + upload + takeover
scripts/deploy_vps.sh --host root@YOUR_VPS --dry-run       # print the plan + remote script
scripts/deploy_vps.sh --host root@YOUR_VPS --arch arm64    # aarch64 VPS
scripts/deploy_vps.sh --host deploy@YOUR_VPS --sudo        # non-root ssh user
```

What it does, in order:

1. Builds from `git archive HEAD` locally (repo invariant: never from a
   shared working tree with peer WIP), `CGO_ENABLED=0 GOOS=linux`, and
   uploads to a temp path in the destination directory.
2. Remote pre-flight: at most one listener on the config's port (refuses
   to pile a third on a half-finished deploy), config exists, dest
   writable, data_dir absolute.
3. No active unit → **overlap-bind zero-drop**: starts NEW detached
   (setsid), polls the authenticated `/admin/health` until 8 consecutive
   oks (round-robin across both SO_REUSEPORT listeners), renames NEW over
   the canonical binary, then SIGTERMs the old pid and watches for
   supervisor respawns.
4. Unit active → smoke-tests NEW on a throwaway port + throwaway data
   dir, swaps the canonical binary, `systemctl restart` (stop→start;
   seconds-scale downtime — a hand-spawned NEW cannot be supervised by
   systemd, see the script header), polls health 8×.
5. Final: health twice, exactly one listener, previous binary kept as
   `onegw.prev`.

Rollback: `mv /usr/local/bin/onegw.prev /usr/local/bin/onegw && systemctl restart onegw`.

Config-only changes need no deploy: `kill -HUP <pid>` or
`PUT /admin/config/reload` hot-swaps providers/combos/keys; a bad file is
rejected and the previous config keeps serving. `onegw update` also works
on the VPS (same zero-drop handoff mechanics), but with `auto = false` you
drive binary updates deliberately.

## 7. omp client wiring

`~/.omp/agent/models.yml` — point the provider at the remote gateway; the
gateway key is the value of `ONEGW_KEYS` / `[auth] keys`:

```yaml
providers:
  onegw:
    baseUrl: https://gw.example.com/v1
    apiKey: <GENERATED_GATEWAY_KEY>
    api: openai-completions
    discovery:
      type: openai-models-list
      injectV1: false   # baseUrl already ends in /v1; fetch {baseUrl}/models
    models:
      - id: dev
        name: dev (combo)
        contextWindow: 1000000
      - id: free
        name: free (combo)
        contextWindow: 1000000
```

Model-role mapping is unchanged (`~/.omp/agent/config.yml`):
`smol: onegw/free`, `default: onegw/free`, `plan: onegw/dev`, etc. —
`provider/model` strings work identically against the remote gateway.

## 8. Backup / restore of the SQLite usage store

The usage store is `<data_dir>/usage.db` (WAL journal, single-writer:
onegw is the only writer; the `.backup` command is a reader). Take
consistent snapshots on the VPS itself:

```bash
install -d -m 0700 /var/backups/onegw
sqlite3 /var/lib/onegw/usage.db ".backup '/var/backups/onegw/usage-$(date +%F).db'"
gzip -f /var/backups/onegw/usage-$(date +%F).db
# retention
find /var/backups/onegw -name 'usage-*.db.gz' -mtime +14 -delete
```

`/etc/cron.d/onegw-backup`:

```
30 4 * * * root sqlite3 /var/lib/onegw/usage.db ".backup '/var/backups/onegw/usage-$(date +\%F).db'" && gzip -f /var/backups/onegw/usage-$(date +\%F).db && find /var/backups/onegw -name 'usage-*.db.gz' -mtime +14 -delete
```

Restore (stop the writer first):

```bash
systemctl stop onegw
gunzip -c /var/backups/onegw/usage-YYYY-MM-DD.db.gz > /var/lib/onegw/usage.db
chown onegw:onegw /var/lib/onegw/usage.db
systemctl start onegw
curl -s -H "X-Admin-Password: $PW" http://127.0.0.1:8080/admin/usage | jq .
```

Cross-host transfer without files: `GET /admin/usage/export` →
`POST /admin/usage/import` (X-Admin-Password header on both; the local
store is written first, fire-and-forget).

## 9. Latency sanity checklist

```bash
# client -> gateway -> upstream, first-token + totals (streaming)
curl -N -s -o /dev/null -w 'dns=%{time_namelookup} tcp=%{time_connect} tls=%{time_appconnect} ttfb=%{time_starttransfer} total=%{time_total}\n' \
  https://gw.example.com/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"free","stream":true,"messages":[{"role":"user","content":"ping"}]}'

# same probe straight from the VPS (measures VPS -> upstream leg)
ssh root@YOUR_VPS 'curl -N -s -o /dev/null -w "ttfb=%{time_starttransfer}\n" \
  -H "Authorization: Bearer $GATEWAY_KEY" -H "Content-Type: application/json" \
  -d "{\"model\":\"free\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}" \
  http://127.0.0.1:8080/v1/chat/completions'
```

Run each probe 3× (upstream latency is noisy). Watch for:

- ttfb through the VPS >> ttfb from the VPS to upstream → proxy/TLS
  overhead (buffering left on is the usual culprit).
- One full omp session (smol + default + plan roles) end-to-end before
  pointing every client at the VPS.
- RSS after the session: `systemctl status onegw` — expect well under the
  100 MB target; `bench/memory.sh` in-repo measures it against a mock.

## 10. Monitoring

Preferred: keep the admin surface off the public internet entirely and
use an ssh tunnel:

```bash
ssh -N -L 8443:127.0.0.1:8080 root@YOUR_VPS &
open http://127.0.0.1:8443/admin     # login with ONEGW_ADMIN_PASSWORD
```

Or reach the dashboard through the TLS proxy (`https://gw.example.com/admin`,
cookie login). Scripted checks use the header:

```bash
curl -s -H "X-Admin-Password: $PW" http://127.0.0.1:8080/admin/health | jq '.status, .owner.pid'
```

`GET /metrics` (Prometheus) is served by the same process — treat it as
admin-surface: reach it via the tunnel, not a public proxy route.

## 11. Single-IP egress: 429 guardrails

On the VPS **all your sessions share one egress IP**. Upstream free tiers
(b-ai/one-api nodes especially) rate-limit on key *and* IP, so two loads
that were separate at home (your laptop, your second machine) now draw
from one IP bucket, and a datacenter IP can draw stricter limits than a
residential one.

What onegw already does (verified constants):

- Per-account adaptive ladder on upstream 429s: first 429 benches the
  account 10s, each consecutive 429 doubles it, capped at 60s
  (`coolBase`/`coolCap`, `internal/provider/provider.go`). A `Retry-After`
  header always wins. A success resets the ladder.
- Account pool round-robin; when the whole pool is cooling, onegw answers
  `429 provider_rate_limited` with the soonest recovery as `Retry-After`
  instead of burning a doomed upstream call.
- Combos: retryable statuses are 408/409/429/500/502/503/504/529
  (`internal/types/types.go`), `MaxAttempts=2` per target, then
  fall-through to the next combo target. Quota-window exhaustion answers
  `503 provider_quota_exhausted` and cools the provider until window end.

Guardrails to actually set:

- Enable `quota_window = "5h"` (or daily/weekly) + `quota_limit_tokens`
  on free-tier providers so onegw backs off *before* the upstream does.
- Keep sessions sequential when the free pool is small; the ladder is
  per-account but the IP bucket is shared — parallelism is what empties
  it.
- Spread accounts (`[[providers.accounts]]` with several keys): the
  ladder benches individual keys, not the pool.
- Watch `/admin/quota` (or the dashboard's quota page) after the first
  week; if a provider's IP bucket chronically exhausts, route that
  provider from home instead of the VPS — the gateway topology makes this
  a client-config change, not a code change.

## 12. Blocked on VPS access

Everything below needs the actual VPS and cannot be done from this tree:

1. Provision per §2, install the config (§3) and unit (§4), and run the
   first real deploy (`scripts/deploy_vps.sh --host ...`).
2. Issue the TLS certificate (Caddy auto-TLS or certbot for nginx) and
   verify the proxy allows long-lived SSE (nginx `proxy_buffering off`).
3. Failover drill: run `deploy_vps.sh` against the live gateway and
   confirm the overlap window (`ss -tlnp` shows two listeners during the
   health poll) or, on the systemd path, that restart downtime is
   seconds-scale as documented.
4. `systemd-analyze security onegw` — review the hardening score; relax
   any directive that misfires on your distro's systemd version via a
   drop-in, not by editing the unit.
5. RSS measurement under real streaming load vs the 100 MB envelope (§9).
6. 429-ladder observation against real b-ai upstreams from a datacenter
   IP (§11) — confirm the datacenter-IP assumption before pointing every
   client at the VPS.
7. Backup/restore drill (§8): restore a snapshot into the stopped unit
   and confirm `/admin/usage` shows the rolled-up history.
8. omp end-to-end: a full session (streaming, tool calls, plan role)
   through `https://gw.example.com/v1`.
