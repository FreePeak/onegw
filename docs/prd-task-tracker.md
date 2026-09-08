# onegw task tracker

Linked from `docs/PRD.md`. Durable task record = GitHub issues
(https://github.com/FreePeak/onegw/issues); this file only mirrors their
status. Status as of 2026-09-08.

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
- [x] import9r bearer-token import: OAuth connections whose upstream accepts
      the token as-is import as single-account providers (`bearerTokenProviders`
      map: xai → https://api.x.ai, kilocode → https://api.kilo.ai/api/openrouter),
      per-endpoint models URL, JWT exp parsed → rotate-me comment under 48 h.
      kilocode (371-model catalog, exp 2031) + xai (12 models, token expires
      2026-09-07T14:24Z — rotate in 9router and re-import) imported into
      onegw.toml; `dev2` combo adds kilocode/kilo-auto/free + xai/grok-4.6
      fallback. Live-verified: kilo free-model chat + SSE stream +
      Anthropic-surface translation, xai grok-4.6 chat, combo fallback.
      commandcode / grok-cli / cursor skipped — custom wire formats, now
      tracked as GitHub issue #12.
- [x] Dashboard header totals now computed from the same store rows as the
      table (previously process-lifetime counters — after a restart the
      header showed 36 reqs while the table summed 109); table window is the
      viewer's local calendar day (browser timezone), not UTC
- [x] Zero-drop rolling restart: gateway binds with SO_REUSEPORT and drains
      in-flight requests on SIGTERM/SIGINT before exit (flushes usage first);
      future restarts = start new binary alongside, health-check, stop old

## Open — tracked as GitHub issues

- [ ] #1 SIGHUP hot reload of config
- [ ] #2 OAuth device flows for subscription providers (Claude Code, Codex,
      Cursor) — biggest gap vs 9router
- [ ] #3 Per-key rate limits and model restrictions
- [ ] #4 Prometheus metrics endpoint
- [ ] #5 Output-side token savers (prompt injection / compression modes)
- [ ] #6 Model aliases in config
- [ ] #7 Quota reset-window tracking and per-provider spending limits
- [ ] #8 Streaming request bodies (client→upstream) without full read
- [ ] #9 Audio and embeddings surfaces (STT/TTS/embeddings passthrough)
- [ ] #10 Multi-node usage rollup export
- [ ] #11 Runtime config surface (dashboard/API writes to config)
- [ ] #12 Custom wire formats: commandcode (NDJSON), grok-cli (OpenAI
      Responses), cursor (protobuf)
- [x] Auto-update: `onegw update` command, background release checks,
      auto-apply opt-in, `/admin/update` endpoints, zero-drop self-handoff
      with rollback; container mode checks + prints host commands; CI
      version stamping for release binaries and Docker images — done
      2026-09-08
- [x] #13 Web-search provider (SearXNG integration) — done 2026-09-08
      (9bd3594): `kind = "searxng"` virtual provider answering `search/query`
      with a SearXNG JSON search as a synthetic OpenAI completion; fail-open
      (retryable 503) in combos; unit + E2E tests. Enabled live 2026-09-08
      (#46/#47): local SearXNG compose stack + `search` provider +
      `search-or-llm` fail-open combo, all surfaces live-verified.

Deliberately NOT tracked (PRD non-goals): cloud sync, billing/budgets as an
enforcement feature, guardrails/MCP/A2A gateways, response caching.
