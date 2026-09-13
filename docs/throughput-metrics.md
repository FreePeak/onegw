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
  — for combos with `strategy = "fastest"`, and only for those below the
  size gate described next.
- **A per-account** steers: `accountPool.pickSlot` takes the fastest OPEN slot
  (round-robin among equals/no-data; `provider.go`).
- **C + A** predict wall time for large prompts: at `inputSize ≥ PrefillMattersAt`
  (32 K tokens) and ≥3 samples in the size bucket, legs are ranked by
  `PredictSeconds = inTokens/prefillTPS + 256/decodeTPS` — the fix for "best
  decode number, last to answer" (232 K-token prompt: 0.96 s decode, 73.7 s
  wall). Reorders log a `speed_order` ring row. `strategy = "size-aware"` is
  this regime ONLY: below 32 K tokens, or with nothing measured for the bucket,
  the configured chain order is left untouched, so a paid-but-quick-to-prefill
  leg cannot take the cheap turns on decode speed alone. In both regimes a leg
  with no sample sorts behind every measured leg (decode: 0 tok/s; prefill:
  `-Inf`) and keeps its configured place among the other unmeasured legs.
- **B never steers**; it surfaces on `/metrics`, the ring (`e2e_ms`/`dtps`) and the
  Console Log delivered column.

Surfaces (`/metrics` value ÷100 — the registry is int64, gauges carry `_x100`):

```
onegw_provider_tokens_per_second_x100{provider}                  # A, provider-wide
onegw_provider_prefill_tokens_per_second_x100{provider,model,bucket}   # C
onegw_client_delivered_tokens_per_second_x100{model}             # B — the only per-model series in OUTPUT tok/s (C is INPUT tok/s)
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
3. **A reload is a partial reset; a restart is a total one.** `apply()` rebuilds the
   provider pool on every config reload (`server.go:164,172`), so a SIGHUP or a
   dashboard Save wipes the per-model, per-account and provider-wide decode EWMAs, the
   prefill EWMAs, the model benches and the learned always/no-thinking sets — all of it
   lives on the rebuilt `Def`/`accountPool` ("a SIGHUP reload rebuilds the pool with
   fresh Defs", `provider.go:339-341`). What SURVIVES a reload: the delivered tracker
   and the request ring, both built once in `New()` (`server.go:118-119`). A restart
   clears everything including those. After either, the steering re-warms from zero
   (≥3 prefill samples at ≥32 K before size-aware ordering engages) — which is why a
   `speed_order` row can vanish right after a routine, otherwise uneventful reload.
4. **Decode vs delivered can legitimately differ by an order of magnitude** — that is
   the design, not a bug. Same instant, 2026-09-13 16:51 +07 (§5 quotes exactly this
   command's output): b-ai's gauge decode EWMA **70.6** tok/s against the same leg's
   ring delivered p50 **4.9** — a ~14× spread. The gap is queue + prefill + rotation +
   failed attempts, which decode is defined to exclude and delivered is defined to
   include.
5. **EWMA digits drift by the minute on a busy gateway, so quote an instant, not a
   value.** The `dev` delivered gauge on this box read 5.71 mid-morning, then 2.94,
   6.39, 5.14 and 9.75 across the following hours — same code, same lane, real
   traffic. A number lifted from this doc or from `/metrics` is a snapshot: carry its
   timestamp, or re-scrape. The durable content is the ratio, the keys and the gates.

## 5. How to calculate it yourself (tested recipes)

**Read the live EWMAs** (gauges are ×100; one command, 2026-09-13 16:51 +07):

```bash
curl -s :8080/metrics | grep tokens_per_second
onegw_provider_tokens_per_second_x100{provider="b-ai"}         7063  → 70.6 tok/s decode
onegw_provider_tokens_per_second_x100{provider="tokenrouter"}  6918  → 69.2 tok/s decode
onegw_client_delivered_tokens_per_second_x100{model="dev"}      514  →  5.1 tok/s delivered
onegw_client_delivered_tokens_per_second_x100{model="free"}    1427  → 14.3 tok/s delivered
```

**Per-LEG p50/p95 RIGHT NOW, no code** — off the #19 request ring, using omp bench's own
nearest-rank formula so script and bench agree to the digit. Two facts bound the window,
and both are hard limits rather than knobs: `logRingCap` = 512 requests, and `latest()`
walks newest→oldest and **stops at the first row older than `logMaxAge` (7 days)** — so an
idle gateway's window is short, not week-long (`admin_pages.go:257,282-307`).

Two filter rules the numbers depend on:

- Keep `code ***REMOVED*** 200` rows **without** a decision `kind`: `speed_order`, `task_routing` and
  `key_invalidated` rows carry no usage at all.
- An **absent** `tps`/`dtps` means *not observed* (sub-floor window, buffered reply,
  synthetic/passthrough result), never 0 — the fields are `omitempty`. Folding them as
  zero would report `p50 = 0` on a busy gateway, which is the exact failure this recipe
  must not have.

And the honesty rule: **p95 only at n ≥ 20.** Below that a p95 is one sample wearing a
distribution's clothes (the steering path has the same instinct —
`MinPrefillSamplesForOrdering = 3`, `prefill.go:218`). With a 512-row ring shared across
every leg, a low-traffic lane can never honestly produce a p95; read its row as an
anecdote.

```bash
PW=$(awk -F'"' '/^admin_password/{print $2; exit}' onegw.toml)
curl -s -H "X-Admin-Password: $PW" \
  "http://127.0.0.1:8080/admin/api/v1/logs?limit=512" -o /tmp/ring.json
python3 - <<'EOF'
import json, math
from collections import defaultdict
MIN_P95 = 20            # below this a p95 is one sample wearing a distribution's clothes
d = json.load(open("/tmp/ring.json"))["entries"]
groups = defaultdict(lambda: {"tps": [], "dtps": []})
for e in d:
    # 200 AND no decision-kind row: tps/dtps are omitempty (absent = NOT observed,
    # never 0), and speed_order/task_routing/key_invalidated rows carry no usage.
    if e.get("code") != 200 or e.get("kind"): continue
    g = groups[(e.get("provider") or "?", e.get("model") or "?")]
    if e.get("tps"):  g["tps"].append(e["tps"])
    if e.get("dtps"): g["dtps"].append(e["dtps"])
def q(v, p):                       # omp bench s9e: nearest-rank percentile
    t = sorted(v)
    return t[max(0, min(len(t)-1, math.ceil(p*len(t))-1))]
def fmt(v):
    if not v: return "no samples"
    s = f"n={len(v)} p50={q(v,.5):.1f}"
    if len(v) >= MIN_P95: s += f" p95={q(v,.95):.1f}"
    return s
# Grouped by the UPSTREAM (provider, model) leg — not the client alias the
# delivered gauge's {model} label carries.
print(f"ring: {len(d)} rows in window")
for (p, m), g in sorted(groups.items(), key=lambda kv: -len(kv[1]["tps"])):
    print(f"{p}/{m}: decode[{fmt(g['tps'])}] | delivered[{fmt(g['dtps'])}]")
EOF
```

Output on the serving gateway — captured by the same 2026-09-13 16:51 +07 command as
the gauges above, verbatim:

```
ring: 512 rows in window
b-ai/qwen3.8-flash: decode[n=246 p50=64.1 p95=90.3] | delivered[n=246 p50=4.9 p95=23.3]
tokenrouter/z-ai/glm-5.3-free: decode[n=130 p50=49.2 p95=242.1] | delivered[n=163 p50=7.9 p95=29.1]
```

The two `n`s disagree on the second row (130 decode vs 163 delivered) for the reason in
§1: the decode fold carries the 200 ms `speedFloor`, the delivered fold deliberately
carries no time floor — so short replies have a delivered rate and no decode rate. That
split is the gates showing themselves, worked: 33 replies on that leg finished their
decode phase in under 200 ms (so `tps` was dropped) but still produced a delivered rate.

**This grouping is per LEG.** `logEntry` has no client-model field: on a success row
`Model` is the resolved *upstream* model threaded through `Execute` → `attempt(…, m, …)`
→ `relayResponse` (`server.go:639-640,834`), while the client's own string lives only in
`delivery.model` (`boundedModel`, `server.go:605`). So this table is **not** the same
quantity as `onegw_client_delivered_tokens_per_second_x100{model="dev"}` (5.1 tok/s in
the snapshot above) or `{model="free"}` (14.3) — same order of magnitude as the per-leg
4.9/7.9, different key: one answers "how does this lane serve whoever asks", the other
"what did the client named X actually get". A client-alias distribution therefore
cannot come from the ring; its cheap home is a bounded sample slice inside
`deliveredSample` (`client_speed.go:60-64`, which today keeps only `tps/ttftMs/n/last`)
— 64 float64s × ≤128 keys ≈ 64 KB, under the existing `maxDeliveredKeys` bound,
inheriting the existing ≥4-token gate. The durable-window ceiling and the histogram
upgrade path live in #90.

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

omp's denominator starts at *its* request send and ends at the terminal SSE event, so an
omp read sits above onegw's handler-entry→relay-end wall by the network + client parse
time — and that tail is not observable from the gateway, so onegw's delivered gauge is a
**proxy** for omp's number, never parity. omp's bench is the only per-model
**distribution** on this box; the gateway keeps EWMAs plus per-request ring rows, and its
three per-model keys differ from each other (bench `{provider,model}`, ring
`(provider, upstream model)`, delivered gauge `{client alias}`). #90 adds the
distribution to the gateway from state it already holds.

## 7. Open work this doc files (both tracked as issues; no shadow backlog here)

- **#90** — per-model throughput distributions, two halves sharing one honesty rule
  (p95 only at n≥20): (a) per-LEG p50/p95 from the request ring (~30 lines over
  `reqlog.latest`, no new state) — the metric omp bench ranks on, computed from state
  onegw already keeps; the §5 script is its prototype, and `usage_rollup`'s
  counter-only schema (`store.go:45-63`) is why anything durable needs a bucketed
  histogram column rather than per-request rows; (b) per-CLIENT-ALIAS p50/p95 from a
  bounded sample slice inside `deliveredSample` — the omp-comparable key, which the
  ring cannot answer because `logEntry` has no client-model field.
- **#87** — read-time staleness in `speed.go` getters (the two-line fix: return 0
  when `time.Since(s.last) > staleAfter`). Already open since 2026-09-11; not re-filed.

## Docs

Companion to `docs/b-ai-free-tier-limits.md` (the measured lane-variance evidence
behind §4's spread) and the `onegw-throughput-steering-ops` skill (operational
diagnosis). Evidence chain: onegw source as cited inline; omp source extracted from
the running `omp/18.1.19` binary (`// packages/coding-agent/src/...` path comments,
`src/utils/token-rate.ts` and `src/cli/bench-cli.ts`), cross-checked against the
shipped `dist/types` contracts.
