# Dashboard config-coverage audit (goal: all file configs visible in the UI)

Audited 2026-09-14 against master (`5c07080`, v0.31.0 lineage). Source of truth
for the config surface: `internal/config` (Server/Auth/Saver/Usage/ProviderCfg/
Acct/ComboCfg/OAuth/Rotation/Update + Aliases). UI surface: the five editor
templates + the admin API in `internal/server`. 9router patterns borrowed from
`src/app/(dashboard)/dashboard/**` (its provider catalog `AI_PROVIDERS` +
auth-method field confirmed the preset design; its per-row status badges match
the OAuth pills).

## Coverage table

| Config key | UI today | Status |
| --- | --- | --- |
| `[[providers]]` name/kind/base_url/api_key/models/subscription_quota/max_concurrency/sticky/quota_window/quota_limit_* | Providers modal | ✅ covered |
| `[[providers]].strategy` (combo) — see below | — | ✅ fixed this pass (Combos modal) |
| `[[providers]] rpm` (provider-wide) | file-only, preserved on save | ⚠️ show+edit TODO (slice B) |
| `[[providers]] responses_models` | modal (add/edit) | ✅ covered |
| `[[providers]] extra_headers, always_thinking, no_thinking, echo_reasoning, default_effort` | file-only, preserved on save | ⚠️ TODO slice B (advanced disclosure) |
| `[[providers.accounts]] base_url, weight` | file-only, preserved | ⚠️ TODO slice B |
| `[[combo]] name/targets` | Combos modal | ✅ covered |
| `[[combo]] strategy, round_robin_limit` | — | ✅ fixed this pass (select + conditional limit, prefill from live config) |
| `[[oauth.accounts]] provider/account/service/owner` | via provider editor account rows | ✅ covered |
| `[[oauth.accounts]] device_url/token_url/client_id/scope` | file-only, preserved | ⚠️ TODO slice B (advanced fields on the account row) |
| `[auth] keys` (flat + policy tables) | PATCH API only, no UI | ⚠️ TODO slice C |
| `[aliases]` | PATCH API only, no UI | ⚠️ TODO slice C |
| `[server] listen, data_dir` | Settings read-only display | ✅ visible (editing deliberately stays file-only: socket rebinding + store identity) |
| `[server] admin_password` | Settings change card | ✅ covered |
| `[server] max_body_bytes, buffered_budget_bytes, access_log, stream_requests, response_header_timeout, task_routing, idempotency_ttl, idempotency_cache` | not shown | ⚠️ TODO slice A (Settings "Runtime" card, apply=SIGHUP-visible, listen-style caveats noted per field) |
| `[rotation] cooldown_base, cooldown_cap, flap_threshold, flap_open, model_bench_ttl, billing_parole` | not shown | ⚠️ TODO slice A |
| `[usage] flush_interval, retention_days, export_url, export_password` | not shown | ⚠️ TODO slice A (never echo export_password) |
| `[saver] enabled, external.*, inject.*` | Saver page read-only stats | ⚠️ TODO slice A + inject-rules list editor |
| `[update] check_interval, auto, repo` | buttons only, no settings | ⚠️ TODO slice A |
| provider presets (kind/url/models per vendor) | — | ✅ new this pass: presets catalog in sqlite + code built-ins, prefills the editor |

## Slice plan
- **A — Settings sections**: one `PUT /admin/config/sections` (whitelisted
  section→key→scalar table, same validate-before-write splice as providers),
  Settings page gets Runtime / Rotation / Usage / Saver / Update cards.
- **B — advanced fields**: provider modal + account row disclosures for the
  file-only provider/account/oauth keys above.
- **C — keys & aliases UIs**: cards on Settings over the existing PATCH
  endpoints (write-only secrets, masked ids).

Done: combo strategy (+pill), presets catalog (+auto-mirror of saved
providers), sign-in auto-open of the OAuth URL — all on
`feat/dashboard-config-coverage`.
