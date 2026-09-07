# onegw PRD

*Last updated: 2026-09-07 (service now auto-resumes: supervisor restarts
onegw on abnormal exit, kill-tested; /admin/health is now password-gated like
/admin/usage — it carries the live in-flight gauge; dashboard passes the
password and scripts updated; earlier: dashboard shows live in-flight
request concurrency; codified ordered design
priorities: fast > security > massive sessions > token saving > lowest RAM;
imported kilocode + xai bearer-token providers from 9router; custom
wire-format gaps tracked as #12; easier-setup roadmap — auto-release CI,
one-command install, one-click agent-CLI install, Docker deploy — tracked as
#14; always-thinking effort coercion for glm-5.3/glm-5.3-flash — 400 on
streaming requests, root cause and fix in #16)*

## Product

`onegw` — single-binary LLM gateway in Go, a resource-efficient alternative to
[9router](https://github.com/decolua/9router) (JS) and
[LiteLLM](https://github.com/BerriAI/litellm) (Python). One process fronts
OpenAI-, Anthropic-, and Gemini-compatible providers behind those same three
HTTP surfaces; routes by `provider/model`, applies fallback chains
("combos"), round-robins accounts, and tracks usage/quota. Hard targets:

- **RAM: ≤ 100 MB resident** under sustained load (default Go GC tuning; no
  per-request buffering of streams).
- **Throughput: 1–2 B tokens/day** (~12–23k tok/s sustained; bursts far higher
  because streaming is I/O-bound passthrough).
- **Sessions: millions of concurrent** — sessions are pass-through by design;
  no conversation state is held in memory.

### Design priorities (ordered — tie-break rubric for every design decision)

1. **Fast** — streaming passthrough, zero frameworks, translation is the
   only O(body) work and only on cross-format requests.
2. **Security** — bearer-key auth at the edge, keys only in gitignored TOML,
   client `provider/model` strings rewritten so they never leak upstream,
   localhost bind by default; per-key policies (#3) extend this.
3. **Long-running massive sessions** — stateless by design; no conversation
   cache; nothing accumulates per session, so uptime is unbounded and
   session count is irrelevant to memory.
4. **Save tokens** — input-side `tool_result` compression (saver); output
   side tracked as #5.
5. **Lowest RAM usage** — the 100 MB contract below; byte-budget
   backpressure; SQLite measured, not assumed.

### Non-goals

- OAuth device flows for subscription providers (Claude Code, Codex, ...) — v1
  accepts API keys and bearer tokens only. OAuth adapters can slot in later
  via the same `Provider` interface (tracked: #2).
- Cloud sync, browser extension, electron tray.
- Billing / spend enforcement. Usage tracking is informational (cost estimates
  only).
- Response/semantic caching, guardrails framework, MCP/A2A gateways,
  realtime/audio endpoints — out of the minimal-gateway scope; revisit only
  on demand.

## Architecture (HLD)

```
CLI tools (Claude Code, Codex, Cursor, ...)          40+ upstream providers
        │                                                    ▲
        ▼                                                    │ SSE / chunked
  onegw :8080  ── openai │ anthropic │ gemini ── surfaces  ──┘
        │
   router:  model → adapter → account pool → retry/fallback chain
        │
   middleware: auth → token-saver → usage counters → translation → upstream
```

Single Go binary. Zero framework (net/http + httputil.ReverseProxy for
same-format passthrough). Packages:

| Package    | Responsibility                                                        |
| ---------- | --------------------------------------------------------------------- |
| `types`    | Unified internal request/response model + wire formats                |
| `translat` | Bidirectional OpenAI ↔ Anthropic ↔ Gemini translation (body + SSE)    |
| `provider` | Provider interface, registry, per-provider HTTP client, adapters      |
| `router`   | Model resolution, combo fallback chains, per-account round-robin      |
| `saver`    | RTK-style tool_result compression filters (prefix sniffing, idempotent)|
| `usage`    | Lock-sharded atomic counters, batched periodic flush to SQLite        |
| `store`    | SQLite (usage rollups only — config lives in TOML)                    |
| `server`   | HTTP surfaces, `/v1/*`, `/v1beta/*`, `/anthropic/*`, admin, dashboard |
| `auth`     | Bearer-key auth, per-key model restrictions (planned, #3)             |

### Memory strategy (the 100 MB contract)

- **Streams are never buffered.** Same-format requests: byte passthrough
  (`httputil.ReverseProxy` semantics, flush-per-chunk). Cross-format: event-wise
  SSE re-encoders with a small bounded window, `http.Flusher` per event.
- Request/response bodies are parsed with `json.Decoder` streaming where
  practical; tool_result compression mutates only bounded pieces.
- Usage counters: fixed-size lock-sharded atomics (no maps per request); flush
  every N seconds to SQLite in one transaction.
- **Global in-flight byte bound.** Per-request body caps do not bound total
  RSS: the non-streaming cross-format path (parse whole request, parse whole
  response) holds O(body) bytes per request. A global semaphore sized in bytes
  (default 48 MiB) gates that path; streaming/passthrough paths never acquire
  it. Under saturation requests wait, memory stays flat.
- Sessions are **stateless**: no conversation cache; `session_id` only keys
  usage rollups. Massive session counts cost nothing.
- `GOGC` default; soft memory limit `GOMEMLIMIT=90MiB` set at startup if
  unset. Allocation-heavy JSON reuse in hot loops.
- **SQLite RSS is measured, not assumed.** `modernc.org/sqlite` is a generated
  C→Go port with nontrivial heap. M5 includes a benchmark asserting its
  resident cost under the flush workload; if it exceeds the budget the store
  falls back to append-only JSONL rollups with periodic compaction.
- SQLite is the only disk I/O besides logs; WAL mode, single writer goroutine.

### Throughput math

1–2 B tok/day ≈ 12–23 k tok/s avg. Typical SSE chunk ≈ 4–32 KB carrying tens of
tokens; per-chunk cost is a `bufio` copy + occasional JSON event rewrite —
Go http server handles this on ~1 core. Translation paths are the only O(body)
work and only run on cross-format requests; they stream event-by-event so
memory is O(event), not O(conversation).

## Milestones

| M   | Scope                                                                 | Status |
| --- | --------------------------------------------------------------------- | ------ |
| M1  | Core types, OpenAI/Anthropic/Gemini translation, streaming            | done   |
| M2  | Router: combos, round-robin, retry/fallback                           | done   |
| M3  | Token saver filters, usage tracking, SQLite store                     | done   |
| M4  | Server surfaces (OpenAI/Anthropic/Gemini), auth, admin, dashboard     | done   |
| M5  | Benchmarks (RSS ≤ 100 MB @ load), smoke test, hardening               | done   |

All v1 milestones are complete and live-verified (B.AI traffic, pi CLI agent
loop, glm/openrouter routes). Post-v1 work is tracked as GitHub issues —
see [Open work](#open-work).

### Verified numbers (bench/memory.sh, mock upstream, macOS arm64)

- Baseline RSS 17 MiB; 30 concurrent 800 KB streaming requests → peak
  67 MiB, flat across rounds; 850 K tokens relayed in the 12 s window
  (~71 K tok/s, ~3× the 23 k tok/s needed for 2 B/day). Mock reports a
  synthetic 200 tokens/request, so the honest load proof is ~2 100 reqs ×
  800 KB ≈ 140 MB/s relayed at 67 MiB RSS.
- `GOMEMLIMIT=90 MiB` soft limit set at startup; `GODEBUG=madvdontneed=1`
  used in benchmarks because macOS MADV_FREE overstates RSS.
- Byte-budget gate: buffered path (body read + saver + translation) holds a
  4× body-size reservation against a 48 MiB global budget; saturation
  returns 503 + Retry-After.

## Competitive positioning

Compared against the two reference gateways ( LiteLLM README + docs,
9router README, as of 2026-09-07).

### Landscape

| | **LiteLLM** | **9router** | **onegw** |
| --- | --- | --- | --- |
| Runtime | Python + FastAPI; Postgres required for keys, Redis recommended in prod; benchmark spec 4 CPU / 8 GB per instance | Node.js 20 + Next.js 16; SQLite-backed config; no published RAM figures | Single static Go binary (`CGO_ENABLED=0`); optional SQLite for usage only; measured 17 MiB idle → 67 MiB peak |
| Scope | Platform gateway: SDK + proxy, 100+ providers, teams/budgets/billing, guardrails, MCP + A2A gateways | Localhost "free AI router & token saver": 40+ providers, subscription maximization, dashboard-first config | Minimal personal/team gateway: 3 wire surfaces, any-to-any translation, combos, token saver, usage rollups |
| Surfaces | OpenAI universal in; Anthropic `/v1/messages` in → any provider; Gemini pass-through only | OpenAI surface (+ Anthropic base URL); Cursor/Kiro/Vertex/Ollama/Responses translations | OpenAI, Anthropic, **and Gemini-native** surfaces in, any-to-any out, per-event SSE re-encoding |
| Fallback | Model-group fallback chains, latency/cost-based routing, session affinity | 3-tier smart fallback (subscription → cheap → free) + custom combos | Named combos (ordered retry chains), weighted round-robin account pools, quota cooldown |
| Config | `config.yaml` + DB overlay (`store_model_in_db`), UI writes models at runtime | Dashboard UI → SQLite; no file config | Single TOML file + env overrides; restart (hot reload: #1) |
| Auth | Virtual keys in Postgres, teams/roles, JWT, SSO (enterprise-gated) | Dashboard-generated keys; enforcement off by default | Static bearer-key list; per-key policy planned (#3) |
| Token saver | None in OSS | RTK (input) + Caveman/Ponytail (output prompt injection) + Headroom (external compress) | RTK-style input-side `tool_result` compression, bounded sniffer |
| Observability | Prometheus `/metrics`, Langfuse/LangSmith/etc callbacks, spend logs | Dashboard quota/analytics + reset countdowns | `/admin/health`, `/admin/usage`, embedded dashboard; Prometheus planned (#4) |
| Backpressure | Docs admit high init/per-request memory growth | Not published | Byte-budget semaphore: 48 MiB global bound, `503 + Retry-After` |

### Where onegw wins

- **Resource envelope**: measured 17 → 67 MiB RSS with no external
  Postgres/Redis; LiteLLM's own docs specify 4 CPU / 8 GB instances and
  admit "high memory usage during initialization and per request"; 9router
  publishes no memory figures and needs Node + PM2/Docker.
- **Gemini as a first-class translated surface** (LiteLLM: pass-through
  only; 9router: no Gemini-native client surface).
- **Memory contract with enforcement** — byte-budget backpressure instead of
  unbounded per-request growth.
- **Infra-as-file config** (TOML, reviewable, no DB overlay semantics).

### Gaps accepted or deferred (issue-tracked)

- OAuth subscription providers + token refresh — 9router's core
  "maximize-the-subscription" pitch; deferred from v1 → #2.
- Output-side token savers (Caveman/Ponytail/Headroom analogues) → #5.
- Quota reset-window tracking + per-provider spending limits → #7.
- Model aliases → #6; per-key rate limits/restrictions → #3; Prometheus
  → #4; audio/embeddings surfaces → #9; streaming request bodies → #8;
  multi-node rollup export → #10; runtime config writes → #11; web-search
  provider → #13.
- Install/ops friction: no prebuilt releases, manual build, manual
  agent-CLI wiring, no Docker image → #14.
- Not pursued (non-goals): cloud sync (9router-only), billing/budget
  enforcement, semantic caching, guardrails/MCP/A2A, runtime dashboard
  config as the primary path (config stays file-based; #11 is optional
  convenience), advanced LB strategies beyond round-robin + combos
  (latency/cost-based routing, session affinity — LiteLLM platform
  features, not minimal-gateway scope).

## Routing model

- Model string forms: `provider/model` (direct), `combo-name` (ordered
  fallback list).
- Combo: `[{provider, model}]`, tried in order; retry on 429/5xx/network
  error with backoff; non-retryable errors (4xx) fail fast; per-provider
  concurrency caps; weighted round-robin account pools with quota cooldown.
- Account = API key + optional base URL override + weight.
- Provider kinds: `openai`, `anthropic`, `gemini` — arbitrary upstreams via
  `base_url` override (40+ providers reachable: OpenRouter, GLM, Kimi,
  DeepSeek, Groq, ...). B.AI, GLM, and OpenRouter routes live-verified.

## Key decisions

- **net/http only, no web framework** — fewer deps, predictable memory.
- **modernc.org/sqlite** (pure Go, no cgo) — cross-compile single binary;
  for one-writer usage rollups; store RSS measured inside the budget in the
  bench (heap_sys 55 MiB peak with translation+store+streams, i.e. SQLite
  not the driver). `CGO_ENABLED=0`.
- **Translation via unified intermediate model** — correctness over
  cleverness; streaming re-encoders are separate from body translation.
  N+M decoders/encoders instead of N×M pairwise adapters.
- **Passthrough-first**: same-format traffic is byte-copied with a bounded
  usage sniffer (64 KiB rolling window); never parsed. Upstream model is
  rewritten into the passthrough body so client-side `provider/model`
  strings don't leak upstream.
- **Byte-budget semaphore (mutex + poll)**, not a token channel — a channel
  cannot express all-or-nothing multi-unit take without deadlock.
- **Config = single TOML file + env overrides**; usage state is the only
  SQLite content. SIGHUP reload: #1.
- **Migration tooling**: `cmd/import9r` imports 9router's data.sqlite
  (API-key connections → accounts/keys/combos TOML), lowering the switch
  cost from 9router.

## Open work

All post-v1 tasks live as GitHub issues (https://github.com/FreePeak/onegw/issues):

| #  | Task                                                        | Source             |
| -- | ----------------------------------------------------------- | ------------------ |
| ~~#1~~ | ~~SIGHUP hot reload of config~~ — **done 2026-09-07**; config swaps as one atomic snapshot, bad file rejected, old usage tracker flushed | v2 tracker |
| #2 | OAuth device flows for subscription providers               | 9router gap        |
| #3 | Per-key rate limits and model restrictions                  | v2 tracker         |
| #4 | Prometheus metrics endpoint                                 | v2 tracker         |
| #5 | Output-side token savers (prompt injection / compression)   | 9router gap        |
| #6 | Model aliases in config                                     | 9router gap        |
| #7 | Quota reset-window tracking and spending limits             | 9router gap        |
| #8 | Streaming request bodies (client→upstream)                  | v2 tracker         |
| #9 | Audio and embeddings surfaces (STT/TTS/embeddings)          | 9router gap        |
| #10 | Multi-node usage rollup export                            | v2 candidate       |
| ~~#16~~ | ~~Always-thinking upstreams 400 on streaming medium/disable-thinking requests (glm-5.3 family)~~ — **done 2026-09-07**; per-provider `always_thinking` globs + same-format effort coercion/drop, knob documented in README + onegw.toml.example, regression tests (commit 40977cf) | production hit |
| #11 | Runtime config surface (dashboard/API writes)              | LiteLLM gap        |
| #12 | Custom wire formats: commandcode (NDJSON), grok-cli (Responses), cursor (protobuf) | 9router gap |
| #13 | Web-search provider (SearXNG integration)                 | v2 candidate       |
| #14 | Easier setup: auto-release CI, one-command install, one-click agent-CLI install, Docker deploy | user request |
| #17 | Self-healing thinking-dialect fallback: coerce + retry on thinking-class 400s, learn per (provider, model), combo-advance as last resort | #16 follow-up |

### Always-thinking effort coercion (#16, done 2026-09-07)

GLM-5.3 family models always reason; the upstream rejects explicit
disable-thinking requests with error 1210 ("use low, high or max"). New
per-provider config `always_thinking = ["model-glob", ...]` (path.Match
syntax) makes onegw rewrite such requests on the passthrough path instead
of forwarding them: `reasoning_effort` none|minimal|medium → low; thinking
`{type:disabled}` and `enable_thinking:false` are dropped (upstream default
thinking-on applies). Knobs are never invented — only explicit disable
requests are rewritten, so providers that legitimately accept "none"
(OpenAI gpt-5.x) are untouched unless listed. Regression tests in
`internal/server/thinking_test.go`; live glm route verified.

Snapshot mirror with done-history: `docs/prd-task-tracker.md`.

### Distribution and setup (#14)

Requested 2026-09-07; four independently shippable workstreams (detail in
the issue):

- **Auto-release CI/CD** — every commit/merge to `master` builds static
  multi-platform binaries (`linux`/`darwin` × `amd64`/`arm64`,
  `CGO_ENABLED=0`) and publishes a tagged release; `latest` tracks newest.
- **One-command local install** — install script fetches the release
  binary, writes a starter `onegw.toml` (localhost bind), and starts the
  server; launchd/systemd unit optional.
- **One-click agent-CLI integration** — `onegw connect <tool>` writes
  provider/base-URL + key into omp.sh, pi.dev, Claude Code, opencode, and
  grok cli configs (pi `models.json` is the proven pattern).
- **One-command cloud/VPS deploy** — multi-stage Dockerfile (static Go
  binary, minimal image) published to `ghcr.io` + `docker run`/compose
  example persisting `data/`.

## Current status (post-M5)
- **9router importer** (`cmd/import9r`): reads 9router's data.sqlite, imports
  API-key connections as onegw providers (accounts, upstream model discovery,
  gateway auth keys) and OAuth bearer-token connections whose upstream
  accepts the token as-is (`bearerTokenProviders`: xai → api.x.ai, kilocode →
  api.kilo.ai/api/openrouter; per-endpoint models URL). JWT `exp` parsed and
  surfaced as a rotate-me comment when under 48 h. Custom wire formats
  (commandcode NDJSON, grok-cli Responses, cursor protobuf) skipped with
  warning — #12. Real config lives in `onegw.toml` (gitignored).
- **Live providers**: B.AI (7 accounts, 48 models, round-robin + fallback
  verified), GLM (1 account), **kilocode** (1 bearer account, 371-model
  OpenRouter-style catalog incl. `kilo-auto/free`; free-model chat + SSE
  stream + Anthropic-surface translation verified live) and **xai** (1
  bearer account, 12 models; `grok-4.6` chat verified live). Fixes that
  fell out: URL version-segment join (`/v1`, `/api/paas/v4` bases), upstream
  model rewrite on same-format passthrough, `developer`→`system` role
  normalization (pi CLI payloads).
- **pi CLI wired**: `onegw` provider in `~/.pi/agent/models.json` + ONEGW_KEY
  env; full agent loop (read/edit/bash) tested through onegw.
- **Dashboard**: password-gated persisted rollups (today, aggregated per
  provider+model), since-start totals, saver "saved" column, health/mem
  strip, live in-flight concurrency gauge (counter incremented across the
  proxy pipelines, shown as `live` and exposed as `inflight` in
  `/admin/health`), 401 flow verified in browser.
- onegw runs as a supervised persistent service on 127.0.0.1:8080 with
  autoresume: the supervisor restarts it on abnormal exit (crash, OOM,
  SIGKILL; bounded backoff) — kill-tested live; deliberate stops stay
  stopped. Deploys remain rolling: pre-build, atomic binary swap, graceful
  drain via SIGTERM.

## Docs

- `docs/ARCHITECTURE.md` — package detail, memory contract, config reference.
- `README.md` — project overview, quick start, benchmarks; `LICENSE` (MIT).
- GitHub issues — the durable task record (see Open work above).
- `docs/prd-task-tracker.md` — historical done-list + issue snapshot mirror.
- `bench/memory.sh` — RSS measurement harness; `scripts/smoke.sh` —
  end-to-end surface tests; `cmd/mockupstream` — fake provider.
