# Throughput metrics: every tok/s number on this box and how it is calculated

*Research 2026-09-13. Answers "how is throughput (tok/s) per model calculated" for
both layers on this machine: the omp CLI the user drives, and the onegw gateway it
talks to. All formulas below are quoted from source — omp's from the running
native binary (`omp/18.1.19`, which embeds its own unminified TypeScript), onegw's
from this repo. Live numbers are from the serving gateway on 2026-09-13.*

## 1. onegw's three clocks

The gateway never computes one "throughput". Each completed request is folded into
**three independent EWMA samples**, and which number you see depends on the surface:

| # | Answers | Key | Numerator | Denominator (window) | Code |
|---|---|---|---|---|---|
| A | How fast does this lane **decode**? | (provider, upstream model) + per-account copy | upstream-reported output tokens | `time.Since(res.FirstByte)` — response headers → relay end; **prefill excluded** | `internal/provider/speed.go:37` |
| B | How fast did the **client** receive its answer? | client model string (`dev`, `b-ai/qwen3.8-flash`, …) | winning attempt's output tokens | whole request wall: handler entry → relay end; **failed attempts, rotation, backoff, prefill all included** | `internal/server/client_speed.go:82` |
| C | How long before the **first byte** on a big prompt? | (provider, model, size bucket) | upstream-reported **input** tokens | attempt start → headers (`res.Prefill`) | `internal/provider/prefill.go:70` |

All three share the same smoothing:

```
v_raw = tokens / window.Seconds()
first sample or >10 min since last:  v = v_raw        (stale reset re-seeds)
otherwise:                          v = 0.25*v_raw + 0.75*v   (speedAlpha = 0.25)
0 means "no data" — every consumer treats 0 as keep-the-configured-order
```

Noise gates (why a number can be *absent* rather than 0):

- A: rejects `out < 4` tokens or window `< 200 ms` (`minStreamTokens`, `minStreamTime`;
  the 200 ms floor re-appears in the ring column as `speedFloor`, `server.go:841` — a
  buffered non-stream reply lands sub-millisecond and once produced a 1.38M tok/s row).
- B: same α, same ≥4-token gate, **deliberately no time floor** — a 30 s reply that
  produced 20 tokens is exactly the collapse B exists to surface.
- C: `≥512` input tokens and `≥50 ms`; folded **only on success** (a header-budget
  abort is a censored sample and must not steer).

Where each is consumed vs. merely surfaced:

- **A per-model** steers: `router.reorderBySpeed` sorts combo legs by `ModelTPS`
  (`internal/router/task.go:377`) — only for combos with `strategy = "fastest"`.
- **A per-account** steers: `accountPool.pickSlot` takes the fastest OPEN slot
  (round-robin among equals/no-data; `provider.go:2506`).
- **C + A** predict wall time for large prompts: at `inputSize ≥ PrefillMattersAt`
  (32 K tokens) and ≥3 samples in the size bucket, legs are ranked by
  `PredictSeconds = inTokens/prefillTPS + 256/decodeTPS` (`prefill.go:204`) — the fix
  for "best decode number, last to answer" (232 K-token prompt: 0.96 s decode,
  73.7 s wall). Reorders log a `speed_order` ring row.
- **B never steers**; it surfaces on `/metrics`, the ring (`e2e_ms`/`dtps`) and the
  Console Log delivered column.

Surfaces (`/metrics` value ÷100 — the registry is int64, gauges carry `_x100`):

```
onegw_provider_tokens_per_second_x100{provider}                  # A, provider-wide
onegw_provider_prefill_tokens_per_second_x100{provider,model,bucket}   # C
onegw_client_delivered_tokens_per_second_x100{model}             # B — the ONLY per-model-keyed tok/s series
onegw_client_tokens_to_first_byte_ms{model}                      # B's TTFT sibling
```

Dashboard: Overview throughput card = A per provider (provider-wide EWMA ranking);
provider-card tok/s pills = A per account; ring rows carry both A (ms/tps) and B
(e2e_ms/dtps) per request. There is **no per-model decode dashboard column** — the
`byModel` EWMA feeds steering (`ModelTPS`), not a gauge.

## 2. omp's own numbers (the client side)

omp computes its own tok/s; it does not read onegw's metrics.

**Status-line `tok/s:` pill** — `calculateTokensPerSecond(messages, isStreaming,
nowMs)` in `packages/coding-agent/src/utils/token-rate.ts`:

```
scan newest→oldest for the LAST message with role***REMOVED***="assistant",
  typeof timestamp***REMOVED***="number", typeof usage.output***REMOVED***="number"
out  = usage.output                       (provider-reported; ≤0/non-finite → null)
win  = (duration finite && >0) ? duration            // finished turn: whole-turn span
       : isStreaming ? nowMs - timestamp : null      // live turn: clock since turn start
null when win***REMOVED***=null || win < 100 ms
tok/s = out * 1000 / win
```

- A **raw quotient off the last assistant message only** — no EWMA, no window.
  omp's analogue of onegw's 200 ms floor is the 100 ms constant.
- **Both denominator branches include TTFT/prefill** (the message `timestamp` is the
  turn start), so omp's pill is comparable to onegw's **delivered** gauge (B), never
  to the decode EWMA (A).
- Three consumers, only one additive: the badge (`usageStats.tokensPerSecond =
  this.#Ve()`), the vibe-worker aggregation (`Gxn(ownerId)` — **sums** the leaf across
  every streaming child session the agent owns; children may run different models),
  and the RPC `get_state` (plain single-session value).
- **Badge cache**: on a null leaf tick (sub-100 ms window, `usage.output` not yet
  populated), the display layer **replays the previous rate for the same turn**
  (`if (this.#ge ***REMOVED***= lastAssistantTimestamp) return this.#me`) instead of blanking;
  it blanks only when the last assistant message changes or none exists. The leaf is
  honest; the badge can be a stale-but-same-turn value — treat mid-turn as advisory.

**`omp bench`** — the only genuinely **per-model** calculator on the client. Per run:

```
durationMs p = message.duration ?? clock;   ttftMs f = message.ttft ?? firstContentEvent - start ?? p
generationMs h = max(0, p - f);             promptTokens g = usage.input + cacheRead + cacheWrite
tokensPerSecond = out*1000/p   // whole request wall (prefill included)
generationTps   = out*1000/h   // decode window; 0 when the reply arrived fully buffered
prefillTps      = g*1000/f     // prompt tokens over the TTFT window
```

Aggregated per `{provider, model}` × thinking level, split by challenge kind
(chat/prefill/generation/prompt-cache), each **rate** metric summarized as
`{mean, min, p50, p95, max}` with **nearest-rank percentiles** (`t[ceil(q·n)-1]` on a
sorted copy); token/cost columns are **per-run means over ok runs** (not sums). The
table ranks on the **p50** of `tokensPerSecond` — medians, explicitly so one queue
hiccup cannot reorder rows. The in-flight progress line shows the arithmetic **mean**
instead, so a live number can legitimately disagree with the number it ends with.

Caveats: the `ttft ?? durationMs` fallback means a fully-buffered run reports
`generationTps = 0` (documented) **but** `prefillTps = prompt/total-wall` —
decode-contaminated; comparable only within a challenge kind whose legs stream. And
`f` is the first **content-bearing** delta (text/thinking/toolcall), so thinking
output starts the clock on reasoning models.

## 3. Numerator semantics (shared by both layers)

- **Never a local tokenize of relayed text.** onegw sniffs the SSE stream for
  `output_tokens|completion_tokens|candidatesTokenCount` with max-merge across chunks
  (`internal/usage/sniff.go`, `types.Usage.Merge` — "streams may repeat counts", so
  a vendor's trailing zero-valued alias duplicates cannot shadow the real value).
  omp reads the provider-reported `usage.output` from the assistant message.
- **Inclusive of hidden reasoning.** `ReasoningTokens` is a reported subset of the
  output total (`translat/openai.go:823`, `responses.go:468`, `grok_stream.go:143`),
  so thinking-heavy models report inflated tok/s on both layers.
- **No usage → no sample.** A stream ending before its final usage chunk gets
  `Estimated=true` with only `InputTokens = reqBodyLen/4` filled (`server.go:1012`);
  output stays 0 → the decode and delivered folds are both skipped. Buffered /
  usage-less models therefore have **no tok/s row, never a fabricated one**.
  Synthetic results (`DoPassthrough`, cursor, searxng) carry no `FirstByte` and are
  never speed-observed, by design.

## 4. Known distortions (read numbers with these in mind)

1. **`samples` is lifetime-cumulative.** `speed.go:42-48` and `client_speed.go:104-111`
   increment the counter unconditionally, including on a stale re-seed. After a
   >10 min idle gap the displayed rate rests on ONE fresh sample while `samples`
   still reads dozens. Judge steering by new-window deltas, not the counter.
2. **Staleness is write-time only (#87, open).** `tps()`/`ModelTPS`/`ProviderTPS`/
   `ModelPrefillTPS` return the stored value with no read-time check, so a leg idle
   for hours keeps advertising yesterday's EWMA into `reorderBySpeed` and
   `pickSlot`, outranking a fresh no-data leg that correctly reports 0. The
   "yesterday's speed must not steer today" contract holds only once traffic returns.
3. **State is per-process.** Any restart wipes the EWMAs and the ring seq; a config
   **reload** rebuilds the pool (fresh Defs → fresh EWMAs) but keeps the request
   ring and delivered tracker (built once in `New()`). After either, the steering
   needs re-warming (3 prefill samples at ≥32 K before size-aware ordering engages).
4. **Decode vs delivered can legitimately differ by an order of magnitude** —
   that is the design, not a bug. Live 2026-09-13: b-ai decodes 47.7 tok/s (gauge),
   ring p50 decode 57.2, while delivered p50 on `b-ai/qwen3.8-flash` is **7.8 tok/s**
   — the gap is queue + prefill + rotation, which decode is defined to exclude and
   delivered is defined to include.

## 5. How to calculate it yourself (tested recipes)

**Read the live EWMAs** (gauges are ×100):

```bash
curl -s :8080/metrics | grep tokens_per_second
# onegw_client_delivered_tokens_per_second_x100{model="dev"} 571  → 5.71 tok/s delivered
```

**Per-model p50/p95 RIGHT NOW, no code** — from the #19 request ring (last 512
requests, 7-day window), using omp bench's own nearest-rank formula so script and
bench agree to the digit:

```bash
PW=$(sed -n 's/^admin_password *= *"\(.*\)"/\1/p' onegw.toml | head -1)
curl -s -H "X-Admin-Password: $PW" \
  "http://127.0.0.1:8080/admin/api/v1/logs?limit=512" -o /tmp/ring.json
python3 - <<'EOF'
import json, math
from collections import defaultdict
d = json.load(open("/tmp/ring.json"))["entries"]
groups = defaultdict(lambda: {"tps": [], "dtps": []})
for e in d:
    if e.get("code") != 200: continue
    g = groups[(e.get("provider") or "?", e.get("model") or "?")]
    if e.get("tps"):  g["tps"].append(e["tps"])
    if e.get("dtps"): g["dtps"].append(e["dtps"])
def q(v, p):                       # omp bench s9e: nearest-rank percentile
    t = sorted(v)
    return t[max(0, min(len(t)-1, math.ceil(p*len(t))-1))]
for (p, m), g in sorted(groups.items(), key=lambda kv: -len(kv[1]["tps"])):
    print(f"{p}/{m}: n={len(g['tps'])} decode p50={q(g['tps'],.5):.1f} p95={q(g['tps'],.95):.1f}"
          f" | delivered p50={q(g['dtps'],.5):.1f} p95={q(g['dtps'],.95):.1f}")
EOF
```

Output on the serving gateway, 2026-09-13 (375 of 395 ring rows were 200s):

```
b-ai/qwen3.8-flash:            n=374 decode p50=57.2 p95=90.4 | delivered p50=7.8 p95=33.7
tokenrouter/z-ai/glm-5.3-free: n=1   decode p50=64.7            | delivered p50=7.1
```

**Probe a lane directly** (per-leg ground truth, when the ring is cold): fire
simultaneous bounded streaming probes at each combo leg and divide
`max_tokens`-delivered by wall seconds — see the `onegw-throughput-steering-ops`
skill for the tested loop. Judge by decode AND delivered: an up-but-crawling first
leg never falls through in an order-locked combo.

## 6. omp ↔ onegw, one table

| window | omp (per run / per turn, raw quotient) | onegw (EWMA α=0.25, 10-min stale) |
|---|---|---|
| whole request | bench `tokensPerSecond`; status-line pill | delivered gauge (B) — same philosophy, plus smoothing |
| decode only | bench `generationTps` (0 when buffered) | per-(provider,model) decode EWMA (A) — feeds steering |
| prefill | bench `prefillTps` (contaminated when buffered) | prefill EWMA per size bucket (C) — feeds size-aware ordering |

omp's denominator starts at *its* request send and ends at the terminal SSE event,
so an omp read sits slightly above onegw's handler-entry→relay-end wall by the
network + client parse time. Neither layer has a per-model **distribution** in the
gateway — that is #90.

## 7. Open work this doc files

- **#90** — per-model p50/p95 from the request ring (~30 lines over
  `reqlog.latest`, no new state): the metric omp bench ranks on, computed from
  state onegw already keeps. Tested recipe in §5 is its prototype.
- **#87** — read-time staleness in `speed.go` getters (the two-line fix: return 0
  when `time.Since(s.last) > staleAfter`).

## Docs

Companion to `docs/b-ai-free-tier-limits.md` (the measured lane-variance evidence
behind §4's spread) and the `onegw-throughput-steering-ops` skill (operational
diagnosis). Evidence chain: onegw source as cited inline; omp source extracted from
the running `omp/18.1.19` binary (`// packages/coding-agent/src/...` path comments,
`src/utils/token-rate.ts` and `src/cli/bench-cli.ts`), cross-checked against the
shipped `dist/types` contracts.
