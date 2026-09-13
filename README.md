<div align="center">
  <img src="assets/logo.svg" alt="onegw logo" width="140" />
</div>

# onegw

<div align="center">

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![LLM Gateway](https://img.shields.io/badge/llm--gateway-8A2BE2)](https://github.com/FreePeak/onegw)
[![OpenAI Compatible](https://img.shields.io/badge/OpenAI-compatible-412991?logo=openai&logoColor=white)](#surfaces)
[![Anthropic Compatible](https://img.shields.io/badge/Anthropic-compatible-191937?logo=anthropic&logoColor=white)](#surfaces)
[![Gemini Compatible](https://img.shields.io/badge/Gemini-compatible-1C69FF?logo=google&logoColor=white)](#surfaces)
[![Streaming](https://img.shields.io/badge/SSE-streaming-FF6F61)](#surfaces)
[![Single Binary](https://img.shields.io/badge/binary-CGO_ENABLED%3D0-3E6259)](#features)
[![RAM](https://img.shields.io/badge/RSS-%E2%89%A4%20100%20MB-brightgreen)](#benchmarks)
[![Latest release](https://img.shields.io/github/v/release/FreePeak/onegw?color=8A2BE2&label=release)](https://github.com/FreePeak/onegw/releases/latest)
[![Build](https://img.shields.io/github/actions/workflow/status/FreePeak/onegw/release.yml?label=build)](https://github.com/FreePeak/onegw/actions/workflows/release.yml)

</div>

**Single-binary LLM gateway in Go.** One process fronts OpenAI-, Anthropic-, and
Gemini-compatible providers behind any of those three API surfaces — with
cross-format translation, fallback routing, token saving, and usage tracking —
inside a ~100 MB RAM envelope.

A resource-efficient alternative to JavaScript gateways like
[9router](https://github.com/decolua/9router): no runtime, no per-request
buffering, no conversation state.

<div align="center">

![onegw admin console — dashboard overview](docs/screenshots/dashboard-overview.png)

**The admin console is part of the binary** — live request feed, token/cost
rollups, provider & combo editing, theme-aware dark UI. Zero CDN, zero Node.

</div>

> **TL;DR** — one `curl` installs a static Go binary; you get three API
> surfaces (OpenAI ⇄ Anthropic ⇄ Gemini, any-to-any), fallback combos,
> multi-key pools with adaptive 429 cooldown, token savers, OAuth subscription
> logins, and a built-in dashboard — at 17 MiB baseline RSS.

<div align="center">

`single static Go binary` · `~17 MiB RSS` · `SSE byte passthrough` ·
`any-to-any translation` · `fallback combos` · `multi-key pools` ·
`OAuth device flow` · `RTK tool_result compression` · `built-in dashboard` ·
`zero external assets`

</div>

## Contents

| | |
| --- | --- |
| **[Quick start](#quick-start)** | install in one command · Docker · build from source · first request |
| **[Why onegw](#why-onegw)** | comparison vs. Node/JS gateways |
| **[Features](#features)** | routing, pools, fault intelligence, savers, OAuth |
| **[Dashboard](#dashboard)** | what the 9 console pages show |
| **[Configuration](#configuration)** | TOML reference, OpenCode Zen, SearXNG, cache profiles |
| **[Surfaces](#surfaces)** | endpoints & translation matrix |
| **[Operations](#operations)** | VPS deploy, ownership, hot reload |
| **[Benchmarks](#benchmarks)** · **[Development](#development)** · **[Docs](#docs)** | measured numbers, test commands, deeper docs |


## Quick start

### One command (macOS / Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/FreePeak/onegw/master/scripts/install.sh | sh
```

Installs the latest release binary (sha256-verified against the release's
SHA256SUMS), writes a starter config (loopback bind, generated admin password),
and starts the gateway on 127.0.0.1:8080. It prints the gateway key and
dashboard password — on a first run the credential is also persisted at
`<data_dir>/admin_password` and explained under
[Dashboard](#dashboard). Set `ONEGW_LISTEN` or `ONEGW_KEYS` to override.
Config lives in `~/.onegw/onegw.toml`; add `[[providers]]` blocks there
(see [Configuration](#configuration)).

Re-running the installer (reinstall/update) never rotates credentials: the
existing config's admin password and gateway keys are reused verbatim, and
while a gateway is already serving, the re-run only swaps the binary and
leaves the running instance and its config untouched.

### One command (VPS / cloud, Docker)

```bash
./scripts/docker_deploy.sh                       # deploy or refresh, on :8080
ONEGW_PROVIDER_OPENROUTER_KEY=sk-... ./scripts/docker_deploy.sh --loopback
```

The script owns every step that is easy to get wrong, because each one has bitten
someone already: it **mints the gateway key once** and stores it in
`onegw-deploy.env` (0600) so a re-run cannot rotate it out from under wired
clients; it **creates and chowns the config volume** so the dashboard's provider
editor can actually save (atomic temp file + rename needs a writable directory);
it **forwards every `ONEGW_*` variable** you exported (provider keys, admin
password); it keeps `usage.db`, `oauth-tokens.json` and a generated admin
password on a named data volume; and it **waits for `/admin/health` to answer
with the real password**, then prints the dashboard URL, key and credential.
Running it again is idempotent, and `--replace` recreates the container after
copying the running config into the volume first — so in-page edits survive
(verified end to end: a two-provider config crosses the migration intact).
`--loopback`, `--publish`, `--image onegw:local`, `--env-file`, `--data-volume`,
`--config-volume`, `--no-pull`, `--no-verify`, `--help`.

Prefer the raw commands? The equivalent is:

```bash
docker volume create onegw-config            # editable config lives here
docker run --rm -u 0 --entrypoint chown -v onegw-config:/etc/onegw \
  ghcr.io/freepeak/onegw:latest -R onegw:onegw
docker run -d --name onegw --restart unless-stopped -p 8080:8080 \
  -e ONEGW_KEYS="$(openssl rand -hex 24)" \
  -v onegw-data:/data -v onegw-config:/etc/onegw \
  ghcr.io/freepeak/onegw:latest
```

Runs the non-root image (~40 MB, healthchecked) with usage data persisted in
the `onegw-data` volume. `ONEGW_KEYS` is required, not defaulted: the image
binds `0.0.0.0:8080`, and a non-loopback listener with no auth keys is
refused at startup (`refusing to serve "0.0.0.0:8080" with no auth keys`)
instead of being served as an open proxy — put a real secret there. The admin
password is the one credential that *is* generated: it is printed once as
`FIRST-RUN ADMIN PASSWORD` in `docker logs` and persisted at
`/data/admin_password`. Pass provider keys as env, e.g.
`-e ONEGW_PROVIDER_OPENROUTER_KEY=sk-...`; or mount a config DIRECTORY with
`-v onegw-config:/etc/onegw`. For a compose setup with
resource limits, see [`docker-compose.yml`](docker-compose.yml):

```bash
ONEGW_KEYS=$(openssl rand -hex 24) docker compose up -d
```

That file pins no credentials on purpose: it **refuses to start** without
`ONEGW_KEYS` (a gateway key is never generated, and the image's `0.0.0.0` bind
makes one mandatory), and leaves the admin password unset so first boot mints
one.

Signing a subscription account in from inside the container needs no paths —
the image ships both entry points and sets `ONEGW_DATA_DIR=/data`:

```bash
docker exec -it onegw onegw oauth list                      # stored accounts
docker exec -it onegw onegw oauth login \
  -provider xai -account you@example.com                    # prints URL + code
```

The account name must match the `[[providers.accounts]]` name in your config
(the dashboard's provider editor writes both halves for you).

**Give `/etc/onegw` a writable directory** — the editor saves straight into the
config file with an atomic temp-file + rename, so a plain `docker run` used to
answer 500 `temp file: open /etc/onegw/.onegw-config-*.toml: permission denied`
until the image chowned that directory to the container user (fixed; rebuild or
wait for the next tag). A **named volume** is the right way to keep it editable
*and* durable — Docker seeds it from the image with the correct ownership:

```bash
docker volume create onegw-config
docker run -d --name onegw --restart unless-stopped -p 8080:8080 \
  -e ONEGW_KEYS=... -v onegw-data:/data -v onegw-config:/etc/onegw \
  ghcr.io/freepeak/onegw:latest
```

A **directory** mount works too — on a Linux VPS give it to the container's user
(`chown -R 100:101 ./cfg`), since no share layer translates ownership for you.

A **single-file** mount (`-v $PWD/onegw.toml:/etc/onegw/onegw.toml`) cannot
support in-page editing, for two independent reasons: the atomic save needs a
writable directory for its temp file first (images before 2026-09-13 stopped
right there with `temp file: open /etc/onegw/.onegw-config-*.toml: permission
denied` — the image now chowns `/etc/onegw`), and even then `rename()` cannot
replace a bind-mounted file (`device or resource busy`). `:ro` is read-only by
definition. Keep the file form for a hand-managed config. Without any mount the
baked config is editable but lives in the container's writable layer and is
**lost on recreate**. To back up or move
a container install — volume, config, image, and what survives — see
[docs/vps-deploy.md § Container installs](docs/vps-deploy.md#container-installs-what-to-back-up-and-how-to-move-it).

### Build from source

```bash
go build -o onegw ./cmd/onegw
cp onegw.toml.example onegw.toml   # add provider keys
./onegw                            # listens on 127.0.0.1:8080 (loopback only)
```

### Updating

`onegw update` keeps a running gateway current with zero dropped requests:

```bash
onegw update            # check + apply (zero-drop handoff restart)
onegw update --check    # report only
onegw version           # what is running
```

The running gateway checks GitHub releases daily (default repo
`FreePeak/onegw`; `[update] check_interval` in `onegw.toml`, `0`/`off`
disables, `auto = true` applies without asking). Applying is done BY the
serving process: it downloads the platform asset (sha256-verified when
GitHub publishes a digest), smoke-runs it, atomically renames it over the
old binary (keeping a `.old` backup), starts the new build on the
SO_REUSEPORT listener, waits until the new process proves it owns the port
by answering `/admin/update` with its own pid, then drains the old pid.
Any failed step rolls back — the old gateway never stops serving.

`GET /admin/update` reports status (current/latest/pid/last check),
`POST /admin/update` forces a check, `POST /admin/update` with
`{"apply":true}` runs the handoff — same auth as the other admin
endpoints (`X-Admin-Password`).

Running in Docker? Self-update is deliberately disabled (the container
filesystem belongs to the image): the gateway still checks and logs newer
releases, and `onegw update` prints the host-side commands —
`docker pull ghcr.io/freepeak/onegw:<tag>` plus recreate
(`docker compose up -d` for compose). The `/data` volume keeps usage
history across the recreate.

Then point any OpenAI-, Anthropic-, or Gemini-compatible client at the
gateway. Examples with curl:

```bash
# OpenAI client → OpenAI upstream (passthrough)
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $ONEGW_KEY" \
  -d '{"model":"openrouter/deepseek/deepseek-v3.2","messages":[{"role":"user","content":"hi"}]}'

# Anthropic client → OpenAI upstream (translated, streamed)
curl http://127.0.0.1:8080/v1/messages \
  -H "x-api-key: $ONEGW_KEY" -H "anthropic-version: 2023-06-01" \
  -d '{"model":"openrouter/deepseek/deepseek-v3.2","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hi"}]}'

# Gemini client → Anthropic upstream (translated)
curl "http://127.0.0.1:8080/v1beta/models/anthropic/claude-sonnet-4-5:generateContent?alt=sse" \
  -H "x-goog-api-key: $ONEGW_KEY" \
  -d '{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}'
```

Use a combo name as the model to get an ordered fallback chain
(`"model": "coding-stack"`).

## Why onegw

| Concern | Typical Node/JS gateway | onegw |
| --- | --- | --- |
| Runtime | Node.js + framework | Single static Go binary (`CGO_ENABLED=0`) |
| Streaming | Parsed & re-serialized | Byte passthrough; cross-format streams re-encoded event-by-event |
| Sessions | Cached conversations | Stateless — memory is independent of session count |
| RAM | Hundreds of MB | **Measured: 17 MiB baseline → 67 MiB peak** under 30 concurrent 800 KB streams |
| Throughput target | — | 1–2 B tokens/day (~12–23 k tok/s sustained; bench hit ~71 K tok/s) |

## Features

- **Three wire surfaces, any-to-any translation.** OpenAI, Anthropic, and
  Gemini clients can all talk to all three upstream kinds — request bodies and
  SSE streams translated on the fly through a unified intermediate model.
- **Combos (fallback chains).** A model name can resolve to an ordered list of
  `{provider, model}` targets: retry on 429/5xx/network errors with backoff,
  fail fast on other 4xx, per-provider concurrency caps.
- **Account pools.** Multiple API keys per provider with weighted round-robin
  and quota cooldown. Upstream 429s trigger an adaptive per-account cooldown:
  the first rate-limit response benches the key for 10 s, each consecutive
  429 doubles the bench (capped at 60 s), and a successful call resets the
  ladder — keys that keep hammering a spent limit sink to the 60 s bench
  instead of re-entering rotation every few seconds. An upstream
  `Retry-After` header wins verbatim. When every account of a provider is
  cooling, the gateway answers 429 + `Retry-After` (or falls through to the
  next combo target) instead of burning a doomed upstream attempt.
  The ladder is tunable per deployment or per provider — `[rotation]`
  `cooldown_base`/`cooldown_cap`, `flap_threshold`/`flap_open`,
  `model_bench_ttl` — with the shipped values as defaults, so an existing
  config is unchanged. Which errors rotate at all is deliberately NOT a knob:
  that classification came from live incidents.
- **Terminal billing refusals.** A `402` (or an `insufficient_quota` /
  insufficient-balance answer) is not a rate limit, so the credential is
  taken out of rotation instead of being re-offered every 10-60 s: the combo
  falls through immediately, the console log records one `key_invalidated`
  row, and the account stays out until an operator clears it
  (`POST /admin/api/v1/providers/{name}/accounts/{acct}/reset`) or the key is
  rotated in config. A provider whose every account is terminal answers
  `503` + `insufficient_quota` naming the accounts, rather than a
  rate-limit lie.
- **Upstream fault intelligence.** Transient upstream faults — proxied
  auth-verify outages (401), one-api distributor parse-reject 400s,
  response-header timeouts (504), model-wide concurrency 429s — are
  reclassified as retryable `502`/`504` so combo chains fall through
  instead of surfacing a lying status; genuine schema errors still fail
  fast.
- **Account-selection strategies.** Beyond the default (fastest recent
  decode speed, round-robin among equals), `selection` opts a provider into
  `p2c` (health score: recent strikes, speed, recency, and subscription
  headroom from the upstream quota tracker), `least-used`,
  `strict-random` (shuffle deck), or `random`.
- **Sticky round-robin combos.** `strategy = "round-robin"` with
  `round_robin_limit` (default 3) keeps one leg at the front for N
  consecutive successes, then rotates to the next — so leg #1 stops
  absorbing every request (and every failure) until it is exhausted, while
  each leg still enjoys a run of warm prefix caches.
- **Sticky accounts & session affinity.** A per-provider `sticky` window
  pins one key to a request identity to keep upstream prompt caches warm,
  and an opt-in `session_header` derives a stable per-key session id when
  the client sends none.
- **Token saver (input + output).** Input side: RTK-style `tool_result`
  compression (prefix sniffing, idempotent, same-format surgical JSON walk)
  cuts prompt tokens before they reach the upstream. Output side: system-prompt
  injection of terse-output directives and an external compress hook (below).
- **OAuth device flows for subscription providers (#2).** `onegw oauth
  login -provider xai` (the standalone `onegw-oauth` binary runs the same
  code) runs the RFC 8628 device flow (Kilo Code's bespoke
  dialect also built in), stores the token in
  `<data_dir>/oauth-tokens.json` (0600, atomic writes), and the gateway
  injects it as the upstream bearer credential at request time — with a
  per-account refresher that tops up the token before expiry (single-flight
  per account) and cools the account when a refresh fails.
- **Usage tracking.** Lock-sharded atomic counters flushed to SQLite on a
  timer; per `provider / model / key / day / hour` rollups; admin API and
  built-in dashboard.
- **Byte-budget backpressure.** The non-streaming cross-format path holds a
  4× body-size reservation against a 48 MiB global budget; saturation returns
  `503 + Retry-After` in the client's wire format instead of growing RSS.


## Dashboard

The built-in admin console lives at `http://127.0.0.1:8080/admin` — sign in
with `admin_password` from your config (a 12-hour HttpOnly cookie; the
`X-Admin-Password` header keeps working for scripts). Fully server-rendered
Go `html/template` + htmx + uPlot, vendored inline: zero external assets, no
CDN, no Node toolchain — the console ships inside the single binary.

First run with no `admin_password` and no `ONEGW_ADMIN_PASSWORD`? The
gateway generates a random credential, stores it in
`<data_dir>/admin_password` (0600), and prints it once in the startup log
(`FIRST-RUN ADMIN PASSWORD`, visible in `docker logs` too) — so a fresh
install is never reachable with a guessable default. Settings → Admin
password in the console changes it (re-proving the current password,
applying through the same reload path as SIGHUP, and signing every session
out — the new password is required immediately).

**Overview** — today's request/token/saver totals, an hourly token chart,
  a top-providers rail, the byte-budget meter (`503` rejection count
  included), and a live SSE strip (in-flight, uptime, heap, GC)
  refreshing every second. Every page renders fully without JavaScript;
  SSE only adds the live updates. The console is dark by default, follows
  your OS preference on first visit, and the `◐` header button persists
  your choice — the screenshot at the top of this README shows the dark theme.

Eight more pages complete the console:

- **Usage** — per `provider/model` rollups over today / 7 days / 1 month /
  all time, with uPlot charts (stacked tokens and request counts). The same
  data is cursor-paginated at `GET /admin/api/v1/usage/daily` and
  CSV-exportable.
- **Console Log** — a live request feed (in-memory ring, newest request
  highlighted, errors in red): model, status, tokens in/out/cached/saved
  per line. Also available as JSON at `GET /admin/api/v1/logs?limit=N`.
- **Providers / Combos** — live config views with in-page editing: add or
  edit a provider (kind, base URL, models, responses-wire ids, upstream
  quota profile, sticky window, account pool) or a combo's fallback chain;
  each save is spliced into `onegw.toml`, validated, and hot-reloaded into
  the running gateway. An account row can name an OAuth service (`xai`) for a
  subscription login instead of a key, and the card then carries the
  **Sign in** / **Sign out** buttons (see
  [Grok subscriptions](#grok-subscriptions-supergrok--grok-build)). Keys and
  tokens are always masked.
- **Quota / Token Saver** — read-only: local quota windows with reset
  countdowns, upstream-reported subscription windows (OpenCode Go,
  z.ai GLM Coding Plan — see below), and token-saver stats.
- **CLI Tools** — copy-paste preset cards for wiring agent CLIs to the
  gateway: Claude Code, opencode, grok, Codex CLI, omp, pi, and hermes —
  the bearer key renders as a `$ONEGW_KEY` placeholder, never a real key.
- **Settings** — build/pid/uptime/listen info and a Maintenance card
  (config reload, sign out), plus a reference of the admin API surface
  (endpoints, auth, SSE topics) as served by the running gateway.

The grouped read-only API lives under `/admin/api/v1/`
(`providers`, `combos`, `quota`, `subscription`, `saver`, `logs`,
`usage/daily`); the flat
`/admin/*` endpoints stay unchanged for scripts. Rollup retention
(`[usage].retention_days`, default 90) prunes old rows daily.

## Configuration

Single TOML file (`-config` flag, `./onegw.toml`, or `$ONEGW_CONFIG`), plus
env overrides:

| Env | Purpose |
| --- | --- |
| `ONEGW_PROVIDER_<NAME>_KEY` | API key for provider `<NAME>` |
| `ONEGW_KEYS` | Comma-separated client keys accepted by the gateway |
| `ONEGW_ADMIN_PASSWORD` | Admin/dashboard password |
| `GOMEMLIMIT`, `GOGC`, `GOMAXPROCS` | Honored if set; otherwise tuned at startup and on every config reload — the soft heap limit follows the buffered budget (90 MiB with the default 48 MiB, budget + 25% headroom above that; GOGC 60, ≤ 4 procs) |

See [`onegw.toml.example`](onegw.toml.example) for the full reference:
providers (`kind = "openai" | "anthropic" | "gemini" | "opencode" | "opencode-free" | "searxng" | "openai-responses"`, optional
`base_url`, models, multiple `[[providers.accounts]]` or the `keys = [...]`
multi-key shortcut), combos, server limits, saver and usage settings.
`data_dir = "memory"` disables persistence.

### OpenCode Zen Go subscription

`kind = "opencode"` fronts an [OpenCode](https://opencode.ai/auth) Go
subscription. `base_url` defaults to `https://opencode.ai/zen/go`; with no
`models` list the full live Go catalog is advertised (35 models: GLM, Kimi,
DeepSeek V4, MiMo, MiniMax, Qwen, plus the Responses-only families — Grok
4.5/4.6, GPT-5.6, Muse Spark). Auth is the subscription key(s) as bearer
credentials, and the gateway always sends an `x-opencode-session` upstream:
the client's own session header when present, otherwise a stable per-key id
(keeps upstream prompt caches warm, isolates conversations). Subscription
keys round-robin and cool on quota errors exactly like any account pool:

```toml
[[providers]]
name = "opencode"
kind = "opencode"
keys = ["oc-key-1", "oc-key-2"]   # one account per key
```

**Per-model endpoint routing.** The open-weight catalog speaks OpenAI Chat
Completions (`/v1/chat/completions`); the `gpt-*`, `grok-*` and
`muse-spark-*` families are served only on the OpenAI **Responses API**
(`/v1/responses`). onegw picks the endpoint per routed model and translates
between the Responses wire and whichever client surface asked — so
`"model": "opencode/grok-4.6"` works from OpenAI, Anthropic, and Gemini
clients, streaming and non-streaming alike. A Responses stream that closes
without `response.completed` is surfaced as an upstream error, never a
clean finish.

### OpenCode Zen Free tier

`kind = "opencode-free"` fronts the keyless public Free tier of the same
vendor (what OmniRoute exposes as its no-auth `opencode` provider):
`base_url` defaults to `https://opencode.ai/zen/v1`, no credentials are
configured or sent (a bare `Bearer` reads as anonymous upstream), and the
gateway always sends `x-opencode-session` — client value when present, else
a stable derived id — which is the free tier's only hard requirement. The
catalog is a small rotating `-free` lineup (`big-pickle`,
`mimo-v2.5-free`, `nemotron-3-ultra-free`, `muse-spark-*-free`, …; the
vendor delists ids without notice — delisted models answer 401 `Model X
is not supported`, so pin `models` to what you've probed). Endpoint
routing matches the Go tier per model: `muse-spark-*` (the `-free` pair
included, live-verified) serves on the Responses wire, everything else on
`/v1/chat/completions` — onegw translates either way. Rate limits are
IP-scoped and anonymous: upstream 429s (`FreeUsageLimitError`) bench the
provider like any other, and combos fall through.

```toml
[[providers]]
name = "opencode-free"
kind = "opencode-free"          # keyless — no api_key line at all
```

### Subscription quota tracking

`subscription_quota` (issue #79) turns on **upstream-reported** plan
tracking for subscription providers, ported from 9router's usage services
(and OmniRoute's quota preflight). Unlike the local `quota_window` counters
(which track what this gateway spent), the gateway polls the vendor's own
usage endpoint once per minute per account and shows the plan's real
windows — used percent and reset time — on the Quota page and
`GET /admin/api/v1/subscription`:

| dialect | vendor endpoint (per account, bearer key) | windows |
|---|---|---|
| `opencode-go` | `https://opencode.ai/zen/go/v1/usage` | rolling 5h, weekly, monthly (%) |
| `zai` | `https://api.z.ai/api/monitor/usage/quota/limit` | session (5h), weekly (credits or tokens, % + plan level) |
| `zai-cn` | `https://open.bigmodel.cn/api/monitor/usage/quota/limit` | same shape (China region) |
| `commandcode` | `https://api.commandcode.ai` (base; the probe appends `/alpha/whoami`, `/alpha/billing/credits`, `/alpha/billing/subscriptions`, `/alpha/usage/summary`) | 5-hour + weekly USD windows (used/cap), monthly credits pool (spend vs pool total); plan label from subscriptions |
| `grok-cli` | `https://cli-chat-proxy.grok.com/v1/billing?format=credits` | the SuperGrok shared weekly pool (`creditUsagePercent`, one window); plan label from the token's `tier` claim |

An account whose vendor-reported window is **fully consumed** parks until
the vendor's stated reset (capped at one poll cycle so an early reset or a
probe hiccup self-heals): the pool skips it and combos fall through instead
of burning doomed upstream attempts. Probes are strictly fail-open — a
vendor outage never benches a healthy account; the Quota page just shows
the last-known state plus the probe error.

```toml
[[providers]]
name = "glm"
kind = "openai"
base_url = "https://api.z.ai/api/coding/paas/v4"
subscription_quota = "zai"   # + GET /admin/api/v1/subscription

[[providers]]
name = "opencode"
kind = "opencode"
keys = ["oc-key-1", "oc-key-2"]
subscription_quota = "opencode-go"   # both keys tracked independently
```

### OpenAI Responses-wire upstreams

`kind = "openai-responses"` fronts upstreams speaking the OpenAI Responses
API at `{base_url}/responses` (e.g. orcarouter.ai). Clients keep using the
normal chat-completions surface; onegw translates the request and re-encodes
the SSE stream back (buffered aggregation for non-streaming clients). Note
that cross-format translation drops upstream prompt-caching knobs (see the
PRD's prompt-caching matrix).

```toml
[[providers]]
name = "orcarouter"
kind = "openai-responses"
base_url = "https://api.orcarouter.ai/v1"
models = ["z-ai/glm-5.3-flash-free", "deepseek/deepseek-v4-flash-free"]
[[providers.accounts]]
name = "me"
api_key = ""
```


### Web search (SearXNG)

`kind = "searxng"` is a virtual provider: no chat model behind it, just a
[SearXNG](https://docs.searxng.org/) instance. Any `"<name>/<x>"` model string
routes to it (canonical id `<name>/query`, advertised via `/v1/models`); the
last user message becomes the query. The gateway runs one SearXNG JSON search
(`GET {base_url}/search?q=…&format=json`) and answers with a synthetic OpenAI
completion — a markdown list of title + URL + snippet — so every existing
surface works unchanged: OpenAI, Anthropic, and Gemini clients (cross-format
translation included), streaming and buffered, and usage rollups carry a
chars/4 estimate.

Search failures are retryable `503 search_unavailable` errors, which makes the
kind **fail-open in combos**: put it first and requests degrade to the real
model when the SearXNG instance is down instead of failing:

```toml
[[providers]]
name = "search"
kind = "searxng"               # SearXNG JSON API at {base_url}/search
base_url = "http://searx:8080" # required
max_results = 5                # results formatted into the completion (0 = 5)
timeout = "10s"                # per-search timeout (0 = 10s)
# If the instance requires auth (SearXNG accepts X-API-Key or basic auth):
#[providers.extra_headers]
#X-API-Key = "..."

[[combo]]
name = "search-or-llm"         # fail-open: search down -> falls to the model
targets = ["search/query", "openrouter/openai/gpt-5.5"]
```

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $ONEGW_KEY" \
  -d '{"model":"search/query","messages":[{"role":"user","content":"onegw gateway Go"}]}'
```

No upstream credential is needed for the kind (public instances are open);
only `base_url` is validated. Your SearXNG instance must allow the JSON
format (`search.format=json`).

### Always-thinking models

Some upstreams (e.g. GLM `glm-5.3` / `glm-5.3-flash`) reason unconditionally
and reject disable-thinking knobs: `reasoning_effort` must be
`low|high|max` (streaming `medium` returns 400), and
`thinking:{"type":"disabled"}` / `enable_thinking:false` are refused. List
such models per provider with `always_thinking = ["glm-5.3*"]` (globs use
`path.Match`; `*` does not cross `/`). Requests routed to a matching model
are rewritten instead of forwarded: `reasoning_effort`
`""/none/minimal/medium` → `low` (`high`/`xhigh`/`max` pass through), and
disable-thinking knobs are dropped so the upstream default (thinking on)
applies. The same rewriting is applied to cross-format requests before
translation (e.g. an Anthropic-surface client routed to a matching model —
the request is coerced upfront instead of burning the first combo target
on a 400).

### No-thinking models

The mirror failure: models with NO thinking mode that reject any
reasoning-effort knob outright. kilo's openrouter gateway duplicates
`reasoning_effort` into `reasoning.effort` internally, then refuses the
request whenever either is present — 400 `"reasoning_effort" and
"reasoning.effort" are both provided with conflicting values`, for
`kilo-auto/free` at every value (live 2026-09-11; the alias rotates to
free models without thinking, e.g. dots-3-note-preview). List such models
per provider with `no_thinking = ["kilo-auto/*"]` (same `path.Match`
globs); requests routed to a matching model have `reasoning_effort`,
`thinking` and `enable_thinking` deleted outright, on the same-format
and cross-format paths alike, so the upstream default (no thinking)
applies. A no-thinking match wins over an always-thinking one. An
unlisted model self-heals: the conflict 400 is learned at runtime
(mirroring `alwaysThinking400`), retried once with the knobs stripped,
then falls through.

### Prompt-cache profiles

Per-provider `cache_profile` opts a route into upstream prompt-cache
anchoring (issue #34). Anchoring always runs LAST — after model rewrite,
token saver, thinking-knob adaptation (always/no-thinking) and any
cross-format translation —
so anchors never sit at pre-normalization offsets (a stale anchor costs a
full prefix rewrite). Profiles:

- `""` / `"none"` (default): the body is forwarded byte-identical. GLM,
  DeepSeek and b-ai ignore cache fields entirely (live-probed), so no
  bytes are ever invented for them.
- `"sticky-key"`: injects the request identity (the `X-Opencode-Session`
  header, else the auth-key label — the same identity sticky round-robin
  pins) as top-level `prompt_cache_key` for implicit sticky-routing
  upstreams (xAI, OpenRouter, Kimi).
- `"dashscope-marker"`: preserves client `cache_control` markers on the
  OpenAI wire (Qwen accepts up to 4 markers, 20-block lookback); bodies
  carrying more than 4 keep the last 4.
- `"claude-anchor"`: for Anthropic-format upstreams. Every client
  `cache_control` marker is stripped (client markers point at
  pre-normalization offsets) and re-anchored at canonical positions —
  the last system block, the last cache-eligible tool (`defer_loading`
  tools skipped) and the last assistant turn (final message on turn
  one) — so a completed exchange keeps a byte-stable prefix.

Providers with a cache profile always take the buffered pipeline (the
raw streaming fast path is bypassed) so anchoring applies to every
request, including passthrough JSON bodies.

### Sticky account round-robin

Multi-account providers (several `[[providers.accounts]]`) rotate keys
round-robin per request by default. `sticky = "5m"` (a duration, per
provider) instead pins one account to a request identity for that window —
the first call picks the next account in rotation and reuses it, keeping
upstream prompt caches warm across repeat calls. The identity is the
`X-Opencode-Session` header when the client sends one, else the auth-key
label. A failed upstream attempt unpins immediately (retries and combo
fallback land on a different key), and a pinned account that cools on quota
rotates to the next one and re-pins. Off by default (`sticky = ""`).

```toml
[[providers]]
name = "orcarouter"
kind = "openai-responses"
base_url = "https://api.orcarouter.ai/v1"
sticky = "5m" # one key per session/key identity for 5 minutes
```


**Adaptive 429 cooldown.** On an upstream 429 the picked account cools:
`Retry-After` (when the upstream sends one) verbatim, otherwise an
adaptive ladder — 10 s for the first 429, doubling per consecutive 429,
60 s cap, reset to base by any successful call. While every account of
the provider is cooling, requests do not reach the upstream at all: combo
chains fall through to the next target and direct routes are answered
429 with a `Retry-After` naming the pool's soonest recovery. This turns
per-key burst limits (one-api/new-api style: empty-body 429, no
`Retry-After`) from a retry storm into a self-balancing rotation. The same
ladder benches an account on a premium-gating 403 (`access_denied` /
"Deposit required" — the credential lacks access to the model, issue #48):
the pool rotates to the next account or combo target instead of surfacing
the 403, and a fully-gated pool answers the cooling-pool 429 + `Retry-After`.

**Stated rate windows are honored.** A 429 whose body names a
request-count window (new-api style: "Maximum 8 requests within 1
minutes") benches the account for that window verbatim — like a
`Retry-After` header would — instead of the 10 s ladder base that
re-enters the still-closed window (live tokenrouter evidence: 429 at :46,
ladder retry at :57 hit the same wall, success only ~30-40 s later). The
same window rides the surfaced error as the client `Retry-After`.

**Provider-wide RPM budget.** Some upstreams rate-limit per user /
per model lane rather than per key (live tokenrouter 2026-09-09: 8
req/min shared across both keys — one key 429ed with only ~5 attempts in
its trailing window). No per-account `rpm` can express that wall, so the
provider itself takes one shared token bucket:

```toml
[[providers]]
name = "tokenrouter"
models = ["z-ai/glm-5.3-free"]
rpm = 6   # SHARED budget, all accounts of this provider, worst minute 2+6
```

Size it to `upstream_limit - 2` (the bucket holds a 2-request burst);
when the shared budget drains, the pool reports the honest refill
instant — combo chains fall through instead of feeding the shared window
doomed attempts, and per-account `rpm` budgets keep working alongside it.

### Session affinity (per-key session headers)

When the client sends none of the forwarded identity headers
(`x-grok-conv-id`, `x-grok-session-id`, `x-session-id`, `session_id` —
client-sent values are always relayed verbatim), a provider can opt into a
derived one: the gateway sends a stable per-key opaque id (`ses_` + hex) in
the configured header, so repeat calls with one credential land on a warm
upstream prompt cache:

```toml
[[providers]]
name = "xai"
kind = "openai"
# ...
session_header = "x-grok-conv-id"   # e.g. xAI per-server prompt cache
```

Default is off (no header is ever invented); the OpenCode Zen kind does the
same for its upstream with `x-opencode-session` automatically.

### Model tiering (config-only)

Agent CLIs pick models per task type; the gateway just routes. Define two
combos — a cheap one and a strong one — and point the CLI's role slots at
them:

```toml
[[combo]]
name = "tiny"      # free/flash-tier upstreams for background + filler work
targets = ["orcarouter/z-ai/glm-5.3-flash-free", "orcarouter/deepseek/deepseek-v4-flash-free"]

[[combo]]
name = "planning"  # frontier models for planning and default work
targets = ["anthropic/claude-sonnet-4-5", "gemini/gemini-3-pro"]
```

For omp, pin the combos as models of the `onegw` provider in
`~/.omp/agent/models.yml`, then map the role slots in
`~/.omp/agent/config.yml` (`modelRoles`) — cheap roles to `tiny`, default
and planning roles to `planning`:

```yaml
# models.yml (provider onegw, baseUrl http://127.0.0.1:8080/v1)
models:
  - id: tiny
  - id: planning

# config.yml
modelRoles:
  tiny: onegw/tiny
  smol: onegw/tiny
  commit: onegw/tiny
  default: onegw/planning
  plan: onegw/planning
```

Other agent CLIs follow the same shape: wherever the tool distinguishes a
small/fast model from a main one (env vars, `settings.json`, `models.json`),
point the cheap slot at the `tiny` combo and the strong slot at `planning`
via the same gateway base URL. Task-aware combo reordering on the gateway
side (auto-picking the tier from request content) is not built — it is
tracked in [issue #44](https://github.com/FreePeak/onegw/issues/44).

### Output-side token savers


Two optional knobs under `[saver]` (both also need the `enabled` flag):

**Prompt injection** — `[[saver.inject]]` prepends one honest,
conciseness-demanding directive to the system prompt of matching requests:

```toml
[[saver.inject]]
mode = "terse"          # "caveman" (ultra-short), "terse", "ponytail", or "custom"
# models = ["gpt-5*"]   # path.Match globs; empty matches every model
# text = "..."          # custom mode only, shipped verbatim
```

The shipped prompts instruct the model to be maximally concise — no false
persona claims, no withholding requested content. Injection is idempotent
(a `onegw-terse-directive` marker is never applied twice), works on all
three surfaces, survives cross-format translation, and never fails a
request (bodies it cannot parse pass through untouched).
`mode = "ponytail"` ships the
[ponytail](https://github.com/DietrichGebert/ponytail) lazy-senior-dev
ladder (adapted from the upstream ruleset, MIT): YAGNI → reuse what the
codebase has → stdlib → native platform → installed dependency → one
line → minimum that works, with validation/error handling/security and
one runnable check never on the chopping block. It cuts what the agent
BUILDS — pair it with `caveman`/`terse`, which cut what it SAYS. If the
client already runs the ponytail plugin itself (its ruleset tagline is
detected in the prompt), the gateway skips injection instead of stacking
the ladder twice.

**External compress** — `[saver.external]` forwards large requests'
`messages[]` to a Headroom-protocol service and uses its compressed
response (`POST {url}` with `{"messages":[...]}` → `{"messages":[...]}`):

```toml
[saver.external]
enabled = true
url = "http://127.0.0.1:8819/v1/compress"
# timeout_ms = 2000    # per-request external-call timeout
# min_bytes  = 32768   # only bodies at least this large are compressed
# fail_open  = true    # external errors pass the request through uncompressed
```

Fail-open semantics: any external failure (HTTP error, timeout, malformed
response, or a "compressed" payload larger than the original) means the
request continues with its original messages — a saving optimization must
never become an outage. Set `fail_open = false` to answer 502 instead.
### OAuth accounts (device flow)

```toml
[[oauth.accounts]]
provider = "xai"   # [[providers]] entry whose upstream calls carry the token
account  = "main"  # [[providers.accounts]] name
service  = "xai"   # oauth profile: xai | kilocode (defaults to provider)
```

```bash
onegw oauth login -provider xai -account main   # prints URL + code, opens browser, polls, stores
onegw oauth list                                # stored accounts + expiry state
```

Inside the container the same commands run through the gateway binary
(`docker exec -it onegw onegw oauth …`). `-data-dir` is only needed when the
config's `data_dir` sits somewhere the CLI cannot infer: it defaults to what the
gateway itself resolves (the config's `data_dir` → `$ONEGW_DATA_DIR` →
`~/.onegw`), and the resolved store path is printed before the approval prompt,
so a mismatch is visible before you approve rather than after a request still
403s.

Tokens never live in TOML; the account's static `api_key` is the fallback
until a token is stored. Endpoints are overridable per account
(`device_url` / `token_url` / `client_id` / `scope`) for self-hosted IdPs.
Kilo Code tokens carry no refresh token — re-run `login` when they expire.

**Moving the session to another machine.** A login's whole credential state is
one file — `<data_dir>/oauth-tokens.json` (0600, atomic writes). There is no
Keychain item and no machine binding (the refresh grant carries only the public
client id), so a copy skips the re-login:

```bash
scp -p old-mac:'~/.onegw/data/oauth-tokens.json' /tmp/oauth-tokens.json
# target with NO store yet: the plain copy is enough
cp -p /tmp/oauth-tokens.json ~/.onegw/data/oauth-tokens.json

# target that ALREADY has a store: merge, never overwrite — the one file holds
# every provider's tokens, and the per-key `updated_at` bump is what makes the
# store's newer-stamp-wins merge adopt the copy
(umask 077; jq --argjson s "$(jq -c '.tokens // {}' /tmp/oauth-tokens.json)" \
   '. as $d | {tokens: ((($d.tokens) // {}) * $s | with_entries(.value.updated_at = (now|todate)))}' \
   ~/.onegw/data/oauth-tokens.json > ~/.onegw/data/oauth-tokens.json.tmp) \
  && chmod 600 ~/.onegw/data/oauth-tokens.json.tmp \
  && mv ~/.onegw/data/oauth-tokens.json.tmp ~/.onegw/data/oauth-tokens.json
```

The gateway re-stats the file at most once per second, so an external copy is
adopted without a restart — but only when the copied entry's stamp is newer.
One caveat: a session is single-active and refresh **rotates** the refresh
token, so a copied session is a *move*, not a clone — if two machines refresh
the same token, whichever wins leaves the other needing `onegw-oauth login`.

**One session, several surfaces.** A vendor often exposes the same
subscription on more than one wire (xAI: `api.x.ai` chat-completions and the
Grok Build Responses proxy). xAI rotates device sessions, so logging in twice
would knock the first out — instead declare a second account that `owner`
borrows, which resolves the same stored token and never refreshes it twice:

```toml
[[oauth.accounts]]            # the login: owns + refreshes the session
provider = "xai"
account  = "mnhatlinh.doan@gmail.com"
service  = "xai"

[[oauth.accounts]]            # borrower: no login of its own
provider = "grokbuild"        # another [[providers]] entry, any kind
account  = "mnhatlinh.doan@gmail.com"
service  = "xai"
owner    = "xai/mnhatlinh.doan@gmail.com"
```

A refresh failure cools every account that resolves that key, and the Quota
page's `parked` marker shows it. `onegw-oauth login` is always run against the
**owner** entry.

### Grok subscriptions (SuperGrok / Grok Build)

A consumer **SuperGrok** (or X Premium/Premium+/SuperGrok Heavy) plan is
reachable with one `auth.x.ai` device-code session — no API key and no
per-token billing. xAI exposes that same session on two surfaces, and onegw
can front either:

| surface | base URL | wire | notes |
|---|---|---|---|
| Public Model API | `https://api.x.ai` | `kind = "openai"` (chat-completions) | needs scope `api:access` in the token; `grok-4.5` answers on `/v1/responses` only, so a chat-shaped body there fails with 422 `missing input` |
| Grok Build proxy | `https://cli-chat-proxy.grok.com` | `kind = "openai-responses"` | OAuth JWT only (never an `xai-…` key with the plain client id), Responses wire, fingerprint headers `X-XAI-Token-Auth` + `x-grok-cli-version` + `x-grok-client-*` |

```bash
onegw oauth login -provider xai -account you@example.com        # gateway binary
onegw-oauth login -provider xai -account you@example.com        # same code, standalone
docker exec -it onegw onegw oauth login -provider xai -account you@example.com
```

All three do the same thing; the docker form works because the image ships both
binaries and sets `ONEGW_DATA_DIR=/data`. `-data-dir` is only needed when the
config's `data_dir` is somewhere the CLI cannot infer — it defaults to what the
gateway itself resolves (config → `$ONEGW_DATA_DIR` → `~/.onegw`), and the store
path is printed before the approval prompt so a mismatch is visible up front.
The **account name is the join key**: it must equal the `[[providers.accounts]]`
name, or the gateway has nothing to attach the token to.

The gateway stores the session in `oauth-tokens.json` and refreshes it ahead of
expiry — but xAI claims `expires_in = 21600` (6 h) for device tokens it then
revokes silently at ~40-45 min, so the `xai` profile caps the lifetime onegw
trusts at 40 min (`Provider.MaxTokenTTL`, enforced on both the device grant and
every rotation). Without that cap the refresher sleeps for hours while every
`xai/*` request 403s. `subscription_quota = "grok-cli"` then surfaces the
**shared weekly pool** on the Quota page — parking the account when that pool
hits 100 % instead of burning doomed upstream attempts.

**Signing in from the dashboard.** The console does the same login the CLI
does — no TOML editing, no shell:

1. **Providers → `+ Add provider`**: `kind = openai`, base URL
   `https://api.x.ai`, the model ids you want advertised, and a
   `responses models` line of `grok-4.5*` for the ids xAI serves on
   `/v1/responses`. (For the Grok Build proxy instead: `kind =
   openai-responses`, base URL `https://cli-chat-proxy.grok.com`.)
2. **`+ account`**: the account name — xAI names sessions after the login, so
   use e.g. `you@example.com` — and pick the service **`xai`** in that row's
   select. Leave the key field empty: a subscription account has none. Save
   writes both `[[providers.accounts]]` and the matching `[[oauth.accounts]]`
   entry (`service = "xai"`) into `onegw.toml` and hot-reloads the running
   gateway.
3. The card now shows `you@example.com · xai · signed-out` with **Sign in**.
   Clicking it starts the device flow and displays the **code** and the
   activation link; approve in the browser and the dialog follows the login by
   itself, flipping the badge to `signed-in · <expiry>` — no restart, and the
   token lives in `<data_dir>/oauth-tokens.json` (0600), never in the config.
   **Sign out** deletes that token and leaves the config (and the wiring) in
   place: until the next sign-in the account serves with its static `api_key` if
   it has one, and is benched by the upstream `401`/`403` if it does not.

Rows and entries stay in step: clearing a row's service removes its
`[[oauth.accounts]]` entry again — give the row a static key in the same save to
downgrade a subscription account, because a row left with neither a key nor a
service is refused (it would authenticate as nothing while the pool kept dialing
it). A borrower row (`owner` set) shows `borrows <key>` instead of a button — its
session belongs to the entry that owns the login, which is also why a login is
only ever started against the owner, and why unticking the owner is refused
until the borrower is removed. Fields the editor does not surface (per-account
`base_url`, `weight`) are carried over from the file on save, so a routine edit
cannot quietly re-point or re-weight an account.
The buttons are thin wrappers over the admin API, so scripts can drive them
directly:

```bash
curl -s -X POST -H "X-Admin-Password: $PW" \
  'http://127.0.0.1:8080/admin/config/oauth/login?key=xai/you@example.com'  # code + link
curl -s -H "X-Admin-Password: $PW" http://127.0.0.1:8080/admin/config/oauth/accounts
curl -s -X POST -H "X-Admin-Password: $PW" \
  'http://127.0.0.1:8080/admin/config/oauth/logout?key=xai/you@example.com'
```

One login per account: clicking **Sign in** again while a flow is pending
re-offers the same code instead of starting a second device login, because xAI
keeps a single active session per account and two live logins would invalidate
each other. A pending prompt is process-local — a restart or reload drops the
prompt (never a stored token), so finish that one with `onegw oauth login`
(`docker exec -it onegw onegw oauth login -provider xai -account <you>`).

Because xAI serves some ids only on its native Responses endpoint (under an
OAuth bearer that includes the flagship `grok-4.5`; a chat-shaped body reaching
`/v1/responses` 422s `missing input`), an `openai`-kind provider can opt ids off
the chat wire:

```toml
[[providers]]
name = "xai"
kind = "openai"
base_url = "https://api.x.ai"
responses_models = ["grok-4.5*"]   # path.Match globs; empty = everything on chat
```

Matching models are encoded as Responses (stream forced, `store:false`) and the
SSE reply is translated back, so OpenAI-speaking clients see an ordinary
completion. The raw same-format passthrough keys off the per-model format, so an
opted-in request can never bypass the translation.

Not supported, deliberately: the `grok.com` web surface (needs paired `sso` +
`sso-rw` browser cookies behind Cloudflare TLS fingerprinting, and rotating
them is a maintenance trap), and putting SuperGrok behind the `cursor`
provider — `api2.cursor.sh` authenticates a Cursor session JWT only, and
xAI's token carries no Cursor entitlement.

## Surfaces

| Client speaks | Endpoint | Upstream kinds |
| --- | --- | --- |
| OpenAI | `POST /v1/chat/completions`, `POST /v1/completions` | openai (passthrough), anthropic, gemini |
| Anthropic | `POST /v1/messages`, `POST /anthropic/v1/messages` | anthropic (passthrough), openai, gemini |
| Gemini | `POST /v1beta/models/{model}:generateContent[?alt=sse]` | gemini (passthrough), openai, anthropic |
| — | `GET /v1/models` | Config-defined model + combo list |
| — | `GET /admin` | Dashboard (multi-page admin console; `/` redirects there) |
| — | `GET /admin/health`, `GET /admin/usage` | Admin (password-protected) |
| — | `GET /admin/config/oauth/accounts`, `POST /admin/config/oauth/login`, `POST /admin/config/oauth/logout` | Subscription (device-flow) sign-in from the dashboard |

Translation maps stop reasons, usage fields (including cache and thinking
tokens), and tool calls across all three formats. Stream events are re-encoded
per event — N+M decoders/encoders instead of N×M pairwise adapters.

## Operations

### Deploying on a VPS

For a remote/personal-server deployment — hardened systemd unit, zero-drop
deploy script, TLS proxy setup, SQLite backup, and 429 guardrails — see
[docs/vps-deploy.md](docs/vps-deploy.md).

### Ownership: who is running what

The gateway records itself at startup — pid, listen address, start time,
config path + mtime, argv, and binary build stamp (module version, git
revision, dirty flag) — in `<data_dir>/owner.json` and in
`GET /admin/health`'s `owner` block. Operators (and agents) answer
"which instance is canonical" from a live endpoint or a file, not
process-table archaeology. The file is atomic (tmp + rename), re-stamped
on every successful SIGHUP reload, and deliberately **not** removed on
exit: a stale pid from a crashed predecessor is evidence. With a
zero-drop deploy (start NEW → verify health → SIGTERM OLD), the surviving
owner.json is always the serving instance.

### Config changes: reload, don't restart

`kill -HUP <pid>` (or `PUT /admin/config/reload`) hot-swaps providers,
combos, auth keys, saver, admin password. A bad config is rejected and
the previous one keeps serving. Restart is only needed for binary
changes or `data_dir` moves.

## Benchmarks

`bench/memory.sh` measures RSS against a mock provider
(`cmd/mockupstream`), with `GODEBUG=madvdontneed=1` because macOS
MADV_FREE overstates Go RSS after frees.

Verified on macOS arm64:

- Baseline RSS **17 MiB**.
- 30 concurrent 800 KB streaming requests → peak **67 MiB**, flat across
  rounds; ~850 K tokens relayed in the 12 s window (~71 K tok/s).
- Buffered path saturates at the 48 MiB byte budget and returns
  `503 + Retry-After` — memory stays flat instead of growing with load.

## Development

```bash
go test ./...          # unit tests
scripts/smoke.sh       # end-to-end: all surfaces, translation, usage, admin
bench/memory.sh        # RSS benchmark with mock upstream
```

## Docs

- [`docs/PRD.md`](docs/PRD.md) — product scope, architecture summary, milestones, decisions.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — request pipeline, memory contract, translation matrices, store schema.
- GitHub [issues](https://github.com/FreePeak/onegw/issues) — durable task record.
- `docs/prd-task-tracker.md` — done-history snapshot mirroring issues.

## License

MIT — see [`LICENSE`](LICENSE).
