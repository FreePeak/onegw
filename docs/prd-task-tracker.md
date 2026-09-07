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
- [x] Dashboard: auth-aware admin calls (password in localStorage),
      `Cache-Control: no-store`, cumulative since-start totals alongside the
      live flush window; visually verified in a real browser
- [x] `bench/memory.sh`: 30 concurrent 800 KB streams → 67 MiB peak RSS,
      flat after load; budget gate verified

- [x] `cmd/import9r`: 9router data.sqlite importer — API-key connections →
      onegw TOML (accounts, upstream model discovery, gateway [auth] keys);
      known builtin base URLs resolved, unknown skipped with warning
- [x] B.AI live: 7 imported accounts, URL-join fix (base_url with /v1
      suffix no longer doubles), body model rewrite on same-format
      passthrough; combo fallback verified live (req fell through to second
      target); streaming + store rollups verified
- [x] Dashboard table reads persisted store rollups (source=store), totals
      line live-window; browser-verified against real B.AI traffic

- [x] pi CLI integration: `onegw` provider added to ~/.pi/agent/models.json
      (combos + b-ai models), ONEGW_KEY in ~/.zshrc; full agent loop tested
      (read/edit/bash) against B.AI via onegw
- [x] Dashboard completeness pass: `developer`→`system` role normalization
      for same-format passthrough (pi payloads), saver token counts now
      recorded (were discarded), version string, aggregated per
      provider+model rows, cache-read column, 401 auth hint; verified in
      browser incl. 401 path
- [x] joinURL generalized: any `vN` version-suffix base (GLM
      /api/paas/v4) joins without doubled segments; glm route live-verified
- [x] onegw runs as supervised persistent service (hub-managed) on :8080

## Open (v2 candidates)

- [ ] SIGHUP hot reload of config
- [ ] OAuth device flows (Claude Code/Codex/Cursor subscription providers)
- [ ] Per-key rate limits and model restrictions
- [ ] Streaming request bodies (client→upstream) without full read
- [ ] Prometheus metrics endpoint
- [ ] Multi-node usage rollup export
