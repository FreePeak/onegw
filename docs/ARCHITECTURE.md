# onegw Architecture

*Linked from `docs/PRD.md` — details that don't fit the PRD's HLD scope.*

## Request pipeline

```
client ──HTTP──▶ authorize ─▶ acquireForBody ─▶ readBody (MaxBytesReader)
                                                 │
                    ┌────────────────────────────┤
                    ▼                            ▼
   client fmt == upstream fmt          client fmt != upstream fmt
                    │                            │
        saver.ApplyRaw (same-format           Decode → unified → Encode
        surgical JSON walk,                   (translat)
        fields preserved)                           │
                    │                            ▼
                    └──────────┬─────────── provider.Def.Do
                               ▼
              upstream fmt == client fmt?
                 │yes              │no
                 ▼                 ▼
   byte-copy + usage sniffer   TranslateStream (event-wise)
   (64 KiB rolling window)     TranslateStream per-event
                 │                 │
                 └────────┬────────┘
                          ▼
              usage.Observe → shards → flush → SQLite
```

## Memory contract (the 100 MB budget)

| Mechanism | Detail |
| --- | --- |
| Byte-budget semaphore | `ByteBudget` (mutex + 2 ms poll): buffered work reserves 4×body + 64 KiB against `buffered_budget_bytes` (48 MiB). All-or-nothing; mutex+poll instead of a token channel because multi-unit channel take deadlocks (partial holders starve each other). |
| Saturation | Budget exhaustion → 503 + `Retry-After` in the client's wire format. |
| Streaming | Never acquires budget; byte-copy with `http.Flusher` per event. |
| Usage sniffer | Rolling window capped at 64 KiB, trimmed to 256 B between matches; regexes run only when a trigger substring appears. |
| Usage counters | 16 shards × fixed counters (atomics); map key = (provider, model, api_key, day, hour); flush resets counters, keys persist. Memory is independent of request/session volume. |
| GC | `GOMEMLIMIT=90 MiB` (soft), `GOGC≈60`, GOMAXPROCS capped at 4 when unset. |
| Benchmarks | `bench/memory.sh` with `GODEBUG=madvdontneed=1` (macOS MADV_FREE otherwise overstates RSS). Measured: 17 MiB baseline → 67 MiB peak under 30×800 KB concurrent streams, flat. |

## Translation matrices

Stop reasons map through the unified enum
(`end_turn | max_tokens | stop_sequence | tool_use | content_filter`).

| Wire | stop | length | tool_calls / tool_use | SAFETY/refusal |
| --- | --- | --- | --- | --- |
| OpenAI `finish_reason` | `stop` | `length` | `tool_calls` | `content_filter` |
| Anthropic `stop_reason` | `end_turn` | `max_tokens` | `tool_use` | `refusal` |
| Gemini `finishReason` | `STOP` | `MAX_TOKENS` | (STOP + functionCall) | `SAFETY` |

Usage maps: OpenAI `prompt/completion_tokens(+details)` ↔ Anthropic
`input/output_tokens(+cache_*)` ↔ Gemini `prompt/candidatesTokenCount(+cache,
thoughts)`.

Thinking/reasoning: Anthropic `thinking` blocks ↔ OpenAI `reasoning_content`
(DeepSeek-style deltas) ↔ Gemini `thought: true` parts.

Stream event model (`translat.StreamEvent`): `start | part_start | delta |
part_stop | stop | ping | error` — decoders per upstream format, encoders per
client format, composed by `TranslateStream` (N+M implementations instead of
N×M).

## Surfaces

| Client sends | Path | Upstream kinds |
| --- | --- | --- |
| OpenAI | `POST /v1/chat/completions` | openai (passthrough), anthropic, gemini |
| Anthropic | `POST /v1/messages`, `POST /anthropic/v1/messages` | anthropic (passthrough), openai, gemini |
| Gemini | `POST /v1beta/models/{m}:generateContent[?alt=sse]` | gemini (passthrough), openai, anthropic |
| OpenAI | `GET /v1/models` | config-defined model + combo list |

## Config reference

See `onegw.toml.example`. Provider keys may come from
`ONEGW_PROVIDER_<NAME>_KEY`; gateway keys from `ONEGW_KEYS`; admin password
from `ONEGW_ADMIN_PASSWORD`. `data_dir = "memory"` disables persistence.

## Store schema

Single table `usage_rollup` (day, hour, provider, model, api_key) with
request/token counters, PRIMARY KEY upsert accumulation, WAL mode, one
writer. Prune deletes by day cutoff.
