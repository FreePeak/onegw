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

</div>


**Single-binary LLM gateway in Go.** One process fronts OpenAI-, Anthropic-, and
Gemini-compatible providers behind any of those three API surfaces — with
cross-format translation, fallback routing, token saving, and usage tracking —
inside a ~100 MB RAM envelope.

A resource-efficient alternative to JavaScript gateways like
[9router](https://github.com/decolua/9router): no runtime, no per-request
buffering, no conversation state.

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
  and quota cooldown.
- **Token saver.** RTK-style `tool_result` compression (prefix sniffing,
  idempotent, same-format surgical JSON walk) cuts prompt tokens before they
  reach the upstream.
- **Usage tracking.** Lock-sharded atomic counters flushed to SQLite on a
  timer; per `provider / model / key / day / hour` rollups; admin API and
  built-in dashboard.
- **Byte-budget backpressure.** The non-streaming cross-format path holds a
  4× body-size reservation against a 48 MiB global budget; saturation returns
  `503 + Retry-After` in the client's wire format instead of growing RSS.

## Quick start

```bash
go build -o onegw ./cmd/onegw
cp onegw.toml.example onegw.toml   # add provider keys
./onegw                            # listens on :8080
```

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

## Configuration

Single TOML file (`-config` flag, `./onegw.toml`, or `$ONEGW_CONFIG`), plus
env overrides:

| Env | Purpose |
| --- | --- |
| `ONEGW_PROVIDER_<NAME>_KEY` | API key for provider `<NAME>` |
| `ONEGW_KEYS` | Comma-separated client keys accepted by the gateway |
| `ONEGW_ADMIN_PASSWORD` | Admin/dashboard password |
| `GOMEMLIMIT`, `GOGC`, `GOMAXPROCS` | Honored if set; otherwise tuned at startup (90 MiB soft limit, GOGC 60, ≤ 4 procs) |

See [`onegw.toml.example`](onegw.toml.example) for the full reference:
providers (`kind = "openai" | "anthropic" | "gemini"`, optional `base_url`,
models, multiple `[[providers.accounts]]`), combos, server limits, saver and
usage settings. `data_dir = "memory"` disables persistence.

## Surfaces

| Client speaks | Endpoint | Upstream kinds |
| --- | --- | --- |
| OpenAI | `POST /v1/chat/completions`, `POST /v1/completions` | openai (passthrough), anthropic, gemini |
| Anthropic | `POST /v1/messages`, `POST /anthropic/v1/messages` | anthropic (passthrough), openai, gemini |
| Gemini | `POST /v1beta/models/{model}:generateContent[?alt=sse]` | gemini (passthrough), openai, anthropic |
| — | `GET /v1/models` | Config-defined model + combo list |
| — | `GET /` | Built-in dashboard |
| — | `GET /admin/health`, `GET /admin/usage` | Admin (password-protected) |

Translation maps stop reasons, usage fields (including cache and thinking
tokens), and tool calls across all three formats. Stream events are re-encoded
per event — N+M decoders/encoders instead of N×M pairwise adapters.

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
- [`docs/prd-task-tracker.md`](docs/prd-task-tracker.md) — live task tracker.

## License

MIT — see [`LICENSE`](LICENSE).
