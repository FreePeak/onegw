# onegw PRD

*Last updated: 2026-09-08 (filed #41 — dashboard/admin-API revamp umbrella: 9router IA researched from source
(sidebar: Endpoint & Key / Providers / Combos / Usage / Quota / Token Saver / CLI Tools / Console Log),
LiteLLM Admin UI feature set, and frontend skills identified (anthropics/skills frontend-design +
web-artifacts-builder — single-bundled-HTML path preserves the no-external-assets constraint); scope pinned
read-mostly per the runtime-dashboard-config non-goal; earlier: grok via OpenAI Responses API: `FmtResponses`
item-id tool correlation + terminal-event guard — a Responses stream that
closes without `response.completed` is an error, not a clean finish);
`kind = "opencode"` routes per model — gpt-*/grok-*/muse-spark-* to
`/v1/responses`, the rest to `/v1/chat/completions` (probed live: grok
chat path 503s "Endpoint is unavailable"); default Go catalog refreshed to
the live 35-model listing; central fix: `attempt` now implements the
documented buffered non-streaming cross-format path (it previously emitted
a degenerate empty stream for every cross-format `stream:false` request)
and `EncodeOpenAIResponse` no longer drops text content; earlier:
prompt-caching research across eight provider
families — new [Prompt caching (upstream)](#prompt-caching-upstream) section
with the per-provider matrix; filed #31-#36. Verdict: the same-format path is
already cache-friendly — its three body mutations are deterministic and
idempotent, so the prefix is byte-stable across turns and client cache knobs
reach upstream untouched — while cross-format translation drops them outright;
earlier: dashboard token units: compact formatter gained
a B tier — sums ≥ 1e9 now render `1B`, not `1002.2M`; trailing `.0` trimmed
across K/M/B; earlier: opencode provider kind: OpenCode Zen Go
subscription integration — `kind = "opencode"`, default base
`https://opencode.ai/zen/go`, full Go catalog advertised by default
(muse-spark excluded, Responses-API-only), bearer auth, mandatory
`x-opencode-session` upstream (client header forwarded when present, else
stable per-key derived id), multi-key `keys = [...]` account shortcut for
round-robin + quota cooldown; earlier: dashboard usage range filter — today
/ 7 days / 1 month / all time, default all time, selection persisted in
`?range=` so a
reload keeps it; header totals now include sum (in+out) tokens; earlier:
saver defects fixed: ApplyRaw's re-encode HTML-escaped <,>,& across untouched
strings — tag-heavy bodies grew up to +87% (9511→17797 B) while /admin/usage
reported savings (now SetEscapeHTML(false), whole-body never-grow gate,
wire-delta accounting — NOTE: the saved column now reports the true wire
reduction, so pre-fix historic totals are not comparable); long-line byte-cut
split multi-byte runes into U+FFFD mojibake (now rune-boundary backoff); both
pinned by tests verified via mutation checking; earlier:
internal/saver/loss_profile_test.go — pins the RTK trade with runtime
evidence: generic path never truncates distinct lines, dedup-before-truncate
keeps mid-file errors in identical-run logs, truncating filters label the
elided middle with counts, long-line cuts carry byte markers, pretty JSON
sniffs onto the truncating path, idempotence + never-grow enforced per
filter; earlier: security/stability hardening pass: constant-time
key + admin-password comparison, admin auth moved to the X-Admin-Password
header only (query strings leak into logs), fail-closed startup/reload on
non-loopback binds with no auth keys, 30s shutdown drain deadline,
upstream ResponseHeaderTimeout/TLSHandshakeTimeout so a stalled upstream
cannot leak byte-budget reservations, sniffer scan limit now actually
enforced, panics logged with stack traces; earlier: filed #19 — dashboard
console log viewer (9router-style): in-memory ring-buffer log sink,
password-gated /admin/logs endpoints, dashboard console pane; earlier:
/admin/health is now password-gated like /admin/usage — it carries the live
in-flight gauge; dashboard passes the password and scripts updated; earlier:
dashboard shows live in-flight request concurrency; codified ordered design
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
2. **Security** — bearer-key auth at the edge (constant-time compares),
   keys only in gitignored TOML, client `provider/model` strings rewritten
   so they never leak upstream, localhost bind by default, fail-closed on
   non-loopback binds without keys, header-only admin auth; per-key
   policies (#3) extend this.
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
- Response/semantic caching — storing an answer in the gateway and replaying
  it instead of calling the upstream — plus guardrails framework, MCP/A2A
  gateways, realtime/audio endpoints: out of the minimal-gateway scope,
  revisit only on demand. Distinct from **upstream prompt caching**, which
  onegw does not implement but must not break: see
  [Prompt caching (upstream)](#prompt-caching-upstream).
- Response/semantic caching, guardrails framework, MCP/A2A gateways,
  realtime/audio endpoints — out of the minimal-gateway scope; revisit only
  on demand. (Narrow STT/TTS/embeddings passthrough with no translation
  landed via #9; full audio sessions remain a non-goal.)

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
| `types`    | Unified internal request/response model + wire formats                |
| `translat` | Bidirectional OpenAI ↔ Anthropic ↔ Gemini translation (body + SSE), plus the OpenAI Responses-API upstream wire (opencode gpt/grok/muse-spark) |
| `provider` | Provider interface, registry, per-provider HTTP client, adapters      |
| `router`   | Model resolution, combo fallback chains, per-account round-robin      |
| `saver`    | RTK-style tool_result compression filters (prefix sniffing, idempotent, loss profile test-pinned)|
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
- Sessions are **stateless**: no conversation cache; nothing accumulates per
  session, so massive session counts cost nothing. Note: `session_id` keys
  nothing today either — `ChatRequest.SessionID` is parsed from `user` /
  `metadata.user_id` (`openai.go:192`, `anthropic.go:123`) but read nowhere,
  and rollups key on day/hour/provider/model/api_key. Forwarding it as a
  cache-affinity hint is open work (#34, #36).
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

### Prompt caching (upstream)

Research 2026-09-07/08: official vendor docs, live probes against the
configured endpoints, and 9router source. Prompt caching is **upstream-side** —
onegw stores no response (that stays a non-goal). What onegw controls is
whether the request bytes and identity fields upstream caches key on survive
the trip.

**Verdict.** Every configured upstream is `kind = "openai"` (`b-ai`, `glm`,
`kilocode`, `xai`) plus `opencode`. On that path the body is mutated three
times — `saver.ApplyRaw` (`server.go:272`), `adaptAlwaysThinking` (`:588`),
`normalizeRoles` (`:589`) — but all three are deterministic and idempotent
(pinned by `saver/loss_profile_test.go:222-243`), so the prefix stays
byte-identical across turns and client-sent `prompt_cache_key` / `session_id` /
`cache_control` reach upstream untouched (`rewriteModel` rewrites only
`model`). **The entire cache-loss surface is cross-format translation**, where
typed structs drop those fields.

| Family | Mechanism | OpenAI-wire knob | Min tok | Write / read |
| --- | --- | --- | --- | --- |
| Anthropic | explicit breakpoints (+ top-level automatic) | own compat layer: unsupported | 512–4096 | +25%/+100% write; 10% read |
| OpenAI | implicit + explicit (GPT-5.6+) | `prompt_cache_key`, `prompt_cache_options` | 1024 / 2048 | 1.25x write (5.6+); 10% read |
| Gemini / Vertex | implicit auto, **no knob** + `cachedContents` resource | `extra_body.google.cached_content` | 2048–4096 implicit; explicit undocumented | none; 10% read (+storage) |
| xAI Grok | implicit + sticky routing | `prompt_cache_key`; header `x-grok-conv-id` | undocumented | none; 15–25% read |
| Zhipu GLM | implicit only | **none** — probe: all knobs 200-and-ignored | undocumented | none; ~19% read |
| DeepSeek | implicit only | **none** — `cache_control` documented "Ignored" | none documented | none; ~3% read |
| Qwen / DashScope | implicit + explicit | **`cache_control` works on the OpenAI wire** | 1024 | 125% write / 10% read explicit; 20% implicit |
| Kimi / Moonshot | implicit | `prompt_cache_key` | 256 | none; 10–20% read |
| Aggregators | upstream-dependent | OpenRouter `session_id` → 10m sticky (kilo mirrors it: inferred); b-ai ignores all | — | per upstream |

Undocumented cells are genuinely absent from vendor docs — no number is
substituted. Vendors **silently skip caching below the minimum and return no
error**, so savings are verified from reported usage, never assumed.

**Working today:** implicit prefix caching end-to-end on the live routes
(GLM probe: 1528/1529 tokens cached on prefix reuse; b-ai `qwen3.8-flash`:
0 → 1024 → 1664), and cache-read accounting on passthrough, where `sniff.go`
recognizes five of the six vendor usage shapes.

**Broken / open:**

- #31 — no cache-inclusive vs cache-exclusive convention, so every translated
  route mis-reports prompt size; over-reports on the live Anthropic-client ×
  OpenAI-upstream path, and `usage_rollup.input_tok` is not comparable across
  providers.
- #32 — cross-format requests drop `cache_control` / `prompt_cache_key` /
  `session_id`.
- #33 — DeepSeek `prompt_cache_hit_tokens` invisible to sniffer and decoders
  (latent: no direct DeepSeek provider configured today).
- #36 — `x-grok-conv-id` is never forwarded (`Do()` builds a fresh request),
  losing xAI's per-server sticky routing on the live `grok-4.6` route.
- #34 — no anchoring for the families where markers do work (DashScope on the
  OpenAI wire; Anthropic-native).
- #35 — `ApplyRaw`'s global all-or-nothing gate is the one non-monotonic
  mutation and can flip a whole body raw↔compressed, busting the prefix.

**Rules adopted:** never invent a knob for a provider that ignores it; anchor
**last**, after every body mutation (9router's ordering discipline); never
strip client markers on same-format passthrough; treat combo fallback hops as
cold-cache by construction and measure the cost on the existing `cache_read`
column rather than assuming it.

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
| Prompt caching | Response + semantic caching (in-memory/disk/Redis/S3/GCS; Qdrant/Redis/Valkey semantic) — needs an external store; prompt-caching passthrough for 7 provider families | Conversation-state cache (history-hash key, TTL + eviction, per executor) **and** active breakpoint engineering (`anchorClaudeCache`, `prompt_cache_key` injection) | Upstream cache **accounting** only; no gateway-side cache by design; preservation/anchoring gaps tracked as #31-#36 |
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
  enforcement, gateway-side response/semantic caching, guardrails/MCP/A2A,
  runtime dashboard config as the primary path (config stays file-based; #11
  is optional convenience), advanced LB strategies beyond round-robin + combos
  (latency/cost-based routing — LiteLLM platform features, not
  minimal-gateway scope). Cache-affinity *identity* forwarding (#34, #36) is a
  distinct, stateless concern and is pursued.

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
- **Never invent a cache knob.** Providers that ignore a field (GLM, DeepSeek,
  b-ai — verified by live probe) must not receive it; adding bytes that buy
  nothing is worse than adding nothing. Anchoring is opt-in per provider and
  runs after every body mutation.

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
| #19 | Dashboard console log (9router-style): in-memory log sink + `/admin/logs` endpoints + dashboard console pane | user request |
| #31 | Fix cache-inclusive/exclusive usage semantics across translation | research 2026-09-08 |
| #32 | Preserve `cache_control` / `prompt_cache_key` / `session_id` across translation | research 2026-09-08 |
| #33 | Parse missing vendor cache-usage shapes (DeepSeek hit tokens); pin with tests | research 2026-09-08 |
| #34 | Per-provider cache profiles: breakpoint anchoring, anchor-last ordering | research 2026-09-08 |
| #35 | Saver's global gate can flip the request prefix and bust implicit caches | research 2026-09-08 |
| #36 | Forward `x-grok-conv-id` — live sticky-routing loss on xai | research 2026-09-08 |
| #41 | Dashboard revamp: 9router/LiteLLM-style multi-page admin UI + grouped admin API (read-mostly; umbrella over #19/#11; boundary: config stays file-based) | user request |

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
- **Dashboard**: password-gated persisted rollups with a usage range filter
  (today / 7 days / 1 month / all time — default all time, selection kept in
  the `?range=` query so it survives reload), aggregated per provider+model;
  header totals show reqs, in/out/**sum**/saved tokens — token counts use a
  K/M/B compact ladder (≥ 1e9 → `B`, trailing `.0` trimmed), saver "saved" column,
  health/mem strip, live in-flight concurrency gauge (counter incremented
  across the proxy pipelines, shown as `live` and exposed as `inflight` in
  `/admin/health`), 401 flow verified in browser.
- onegw runs as a supervised persistent service on 127.0.0.1:8080 with
  autoresume: the supervisor restarts it on abnormal exit (crash, OOM,
  SIGKILL; bounded backoff) — kill-tested live; deliberate stops stay
  stopped. Deploys remain rolling: pre-build, atomic binary swap, graceful
  drain via SIGTERM. Restarts and drains go through the supervisor by name
  or by explicit PID — never `pkill -f`: the pattern matches the
  replacement too (identical command lines), and a graceful SIGTERM exit 0
  is a "deliberate stop" the supervisor intentionally does not autoresume
  (2026-09-07 outage root cause).

## Docs

- `docs/ARCHITECTURE.md` — package detail, memory contract, config reference.
- `README.md` — project overview, quick start, benchmarks; `LICENSE` (MIT).
- GitHub issues — the durable task record (see Open work above).
- `docs/prd-task-tracker.md` — historical done-list + issue snapshot mirror.
- `bench/memory.sh` — RSS measurement harness; `scripts/smoke.sh` —
  end-to-end surface tests; `cmd/mockupstream` — fake provider.
