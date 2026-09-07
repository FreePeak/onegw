# onegw task tracker

Linked from `docs/PRD.md`. Status as of 2026-09-07.

## Done

- [x] Unified request/response types (`internal/types`) with JSON tags
- [x] OpenAI ↔ Anthropic ↔ Gemini body translation + SSE event-wise
      streaming translation (`internal/translat`), round-trip tests
- [x] Provider registry, kinds (openai/anthropic/gemini), account pools with
      weighted round-robin + quota cooldown (`internal/provider`)
- [x] Router: `provider/model`, combo fallback, retry policy, non-retryable
      fail-fast (`internal/router`), tests
- [x] Token saver: content sniffing, diff/grep/log/tree/generic filters,
      surgical raw application (`internal/saver`), tests
- [x] Usage tracker: 16-shard atomic counters, periodic flush (`internal/usage`),
      tests; bounded usage sniffer for passthrough streams
- [x] SQLite store: WAL, upsert rollups, range query, prune (`internal/store`),
      tests
- [x] Config: TOML + env overrides (`internal/config`)
- [x] Server: OpenAI/Anthropic/Gemini surfaces, /v1/models, auth, admin API,
      embedded dashboard (`internal/server`)
- [x] Byte-budget semaphore (mutex + poll), MaxBytesReader, 503 saturation
      response, tests
- [x] `cmd/onegw` binary with GOMEMLIMIT/GC tuning; `cmd/mockupstream`
- [x] `scripts/smoke.sh`: 8 end-to-end checks green (passthrough, both
      cross-format directions, usage persistence, 404s)
- [x] `bench/memory.sh`: 30 concurrent 800 KB streams → 67 MiB peak RSS,
      flat after load; budget gate verified

## Open (v2 candidates)

- [ ] SIGHUP hot reload of config
- [ ] OAuth device flows (Claude Code/Codex/Cursor subscription providers)
- [ ] Per-key rate limits and model restrictions
- [ ] Streaming request bodies (client→upstream) without full read
- [ ] Prometheus metrics endpoint
- [ ] Multi-node usage rollup export
