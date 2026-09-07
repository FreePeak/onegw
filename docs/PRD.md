# onegw PRD

*Last updated: 2026-09-07 (initial PRD)*

## Product

`onegw` — single-binary LLM gateway in Go, a resource-efficient alternative to
[9router](https://github.com/decolua/9router) (JS). One process fronts 40+ LLM
providers behind OpenAI-compatible, Anthropic-compatible, and Gemini-compatible
HTTP surfaces; routes by `provider/model`, applies fallback chains ("combos"),
round-robins accounts, and tracks usage/quota. Hard targets:

- **RAM: ≤ 100 MB resident** under sustained load (default Go GC tuning; no
  per-request buffering of streams).
- **Throughput: 1–2 B tokens/day** (~12–23k tok/s sustained; bursts far higher
  because streaming is I/O-bound passthrough).
- **Sessions: millions of concurrent** — sessions are pass-through by design;
  the gateway never materializes a full conversation in memory.

### Non-goals

- OAuth device flows for subscription providers (Claude Code, Codex, ...) — v1
  accepts API keys and bearer tokens only. OAuth adapters can slot in later via
  the same `Provider` interface.
- Cloud sync, browser extension, electron tray.
- Billing. Usage tracking is informational (cost estimates only).

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
| `store`    | SQLite (config: providers/keys/combos; usage rollups)                 |
| `server`   | HTTP surfaces, `/v1/*`, `/v1beta/*`, `/anthropic/*`, admin, dashboard |
| `auth`     | Bearer-key auth, per-key model restrictions                          |

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

## Routing model

- Model string forms: `provider/model` (direct), `combo-name` (ordered
  fallback list), `provider-alias/model`.
- Combo: `[{provider, model, weight?}]`, tried in order; retry on 429/5xx/
  network error with backoff; per-provider concurrency caps; sticky routing per
  API key for account pools (round-robin by default).
- Account = API key + optional base URL override + rate-limit budget.

## Milestones

| M   | Scope                                                                 | Status |
| --- | --------------------------------------------------------------------- | ------ |
| M1  | Core types, OpenAI/Anthropic translation, streaming, 3 provider kinds  | planned |
| M2  | Router: combos, round-robin, retry/fallback                           | planned |
| M3  | Token saver filters, usage tracking, SQLite store                     | planned |
| M4  | Server surfaces (OpenAI/Anthropic/Gemini), auth, admin API, dashboard | planned |
| M5  | Benchmarks (RSS ≤ 100 MB @ load), smoke test, hardening               | planned |

## Key decisions

- **net/http only, no web framework** — fewer deps, predictable memory.
- **modernc.org/sqlite** (pure Go, no cgo) — cross-compile single binary; fine
  for one-writer usage rollups. `CGO_ENABLED=0`.
- **Translation via unified intermediate model**, not string rewriting —
  correctness over cleverness; streaming re-encoders are separate from body
  translation and share the same mapping tables.
- **Passthrough-first**: if client format == upstream format, never parse the
  body. This is the common case (Claude Code → Anthropic provider) and must
  cost ~nothing.
- **Config = single TOML file + env overrides**, hot-reloadable (SIGHUP); no
  SQLite for config in v1 (keeps store append-only for usage).

## Docs

- `docs/ARCHITECTURE.md` — package detail, translation matrices, memory
  contract, config reference.
- `docs/prd-task-tracker.md` — live task tracker (local repo, not GitHub).
- `bench/` — load generator + RSS measurement harness.
