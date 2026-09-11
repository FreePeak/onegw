# B.AI (b.ai) free tier — throughput & rate limits

*Research date: 2026-09-11. Vendor: B.AI (`b.ai`, `chat.b.ai`, `api.b.ai`, `bankofai.io`),
operator BAI Inc / GitHub orgs `BAI-labs` + `BofAI`. Provider name in onegw: `b-ai`.*

Scope: what B.AI's **free tier** actually is, what throughput/rate limits it enforces, and
how those are (not) communicated. Onegw-relevant conclusions are at the end.

> Not to be confused with TheB.AI (`theb.ai`), an unrelated vendor that also abbreviates
> to "b.ai" — OmniRoute's provider reference explicitly distinguishes them
> ([PROVIDER_REFERENCE.md](https://github.com/diegosouzapw/OmniRoute/blob/HEAD/docs/reference/PROVIDER_REFERENCE.md)).

---

## 1. The headline

**B.AI publishes no numeric throughput limit — for the free tier or any other tier.**
There is no RPM, TPM, concurrency, per-model cap, or tier table on any vendor surface: not in
the docs, not in the docs source repo, not on the pricing page, not in `/v1/models`, not in
the web app. The only published statement about limits is one row in the API error table:

| Status | Description | Handling |
|---|---|---|
| `429` | `Rate limit triggered` | `Retry with exponential backoff and reduce concurrency.` |

Source: <https://docs.b.ai/llmservice/api/> (`Error Responses → HTTP Status Codes`). The
Chinese twin (verbatim, from the same page): `429触发速率限制使用指数退避重试并降低并发。`

**And "free tier" is not a rate tier at all — it is price-free models.** B.AI's free access is
promotional `0 Credits/Token` billing on specific models, with no separate allowance, bucket,
or quota attached to it.

### How the docs were mined
`docs.b.ai` is Docusaurus v2.4.3 (en at `/llmservice/…`, zh at `/zh-Hans/llmservice/…`).
All **180 pages** from the sitemap were fetched in both locales and grepped for
`rate limit / 限流 / 限速 / 频控 / 并发 / RPM / TPM / 吞吐 / quota / 429 / per minute`.
Result: exactly one 429 row, one `413 Request body exceeds the platform limit` row, and no
numbers. The docs source repo is public and confirms this is the complete text:
<https://github.com/BofAI/docs> (`docs/llmservice/api/API.md`).

---

## 2. What the free tier actually is

### 2.1 Promotional `0 Credits` models
Credits are the platform currency: **`1 USD = 1,000,000 Credits`**, so a `0.15 Credits/Token`
price = `$0.15 / 1M tokens`.

Currently-free (`0 Credits` — no input, cache-write, cache-read, or output fees):

| Model | Free since | Note |
|---|---|---|
| Qwen3.8-Flash | "currently billed at `0 Credits`" | API free; Chat free on launch |
| Hy3 | 2026-08-21 | Chat + API at `0 Credits` |
| MiMo-V2.5 | API 2026-08-24, Chat 2026-08-25 | phased release |
| GLM-5.3-Flash | "currently billed at `0 Credits`" (model page) | see the conflict below |

Sources: <https://docs.b.ai/llmservice/promotions-and-pricing-notices/> and the per-model pages
(<https://docs.b.ai/llmservice/models/qwen3-8-flash/>,
<https://docs.b.ai/llmservice/models/glm-5-3-flash/>).

**⚠ Free-window conflict for GLM-5.3-Flash (onegw's `free`/`dev` combo leg #1).** The model
page says it is *currently* `0 Credits`; the promotions page separately announces
*"The offer begins at 10:00 on September 12, 2026 (Singapore Time, UTC+8). During the offer,
GLM-5.3-Flash discounts are available at rates as low as 10% of the standard price."* The two
pages do not cleanly agree on whether tomorrow's change ends the free phase or layers a
discount on it. Standard reference price if/when it ends: `0.15` in / `0.15` cache-write /
`0.03` cache-read / **`0.50` output** Credits per token. Treat 2026-09-12 10:00 UTC+8 as a
pricing event to re-check. DeepSeek-V4.1-Flash gets the same 10% offer from the same moment.

Third-party sweeps of zero-balance accounts found **5 usable free models** — `qwen3.8-flash`,
`glm-5.3-flash`, `deepseek-v4-flash`, `deepseek-v4-flash-vision-exp`, `hy3` — with most premium
models returning `403 Deposit required to unlock premium models` or
`400 insufficient_user_quota`:
[openmake_llm#711](https://github.com/openmake/openmake_llm/pull/711),
[pi-bai](https://github.com/pgciq/pi-bai).

**Vendor's own roster — same set, from a promotional announcement.** B.AI's "Inclusive Large
Model Compute" campaign announcement claims a *"lineup of 6 cutting-edge top-tier models"*
under *"zero threshold, unlimited"* free access — specifically **GLM-5.3-Flash
(Ox Alpha), Qwen3.8-Flash, Hy3, MiMo-V2.5** retained free, while **DeepSeek-V4-Flash and
DeepSeek-V4-Flash-Vision-Exp moved to tiered discounts from 2026-09-03** (50% off peak, 25% of
peak off-peak). Source: [TechFlow / TRON Eco News, 2026-09-03](https://www.techflowpost.com/en-US/article/33731)
— **vendor PR, not independent reporting**. ⚠ The piece is internally inconsistent by a factor
of 10: its headline/OG title reads *"Daily Token Throughput Surpasses 13.3 Trillion"* while the
body says throughput *"broke through the 1.33 trillion mark"* (and repeats 1.33 trillion twice
more). This doc uses the **body** figure; if you cite this piece, do not inherit the headline
number. It still corroborates the docs-derived free list and that free windows close on the
vendor's own schedule.

> **The vendor describes the free tier as "unlimited" while enforcing undocumented per-account
> caps** (concurrency ≈1, ~5 req/min/key — §3.2). "Zero threshold, unlimited" should be read as
> *priced at zero*, not *unmetered*.

### 2.2 Signup/free Credits
Invitation and bonus Credits are time-boxed, not perpetual: `300,000 Credits` (= $0.30) on
invitation registration, and top-up bonus Credits, both **valid 30 days** then expiring
(<https://docs.b.ai/llmservice/invitation-rewards/>). Third-party pool repos quote
`500,000` and `100,000` credits for the wallet signup bonus
([Free-BAI](https://github.com/BuluBulugege/Free-BAI),
[bankofai-pool](https://github.com/BuluBulugege/bankofai-pool)) — they disagree, so the exact
signup figure is **unverified**.

### 2.3 The only *published* allowance numbers are subscription tiers
Not free tiers, and expressed in messages rather than tokens, with load-dependent release:

| Plan | Price | Published allowance |
|---|---|---|
| Plan Pro | $200/month | *approximately 50–500 messages per 12 hours* |
| Plan Max | $2,000/month | *approximately 500–5,000 messages per 12 hours* |

> "Subscription usage is measured within a rolling 12-hour window, and capacity is gradually
> released over time. Under lower system load, more subscription capacity remains available.
> When subscription capacity is fully used, the system automatically begins consuming top-up
> Credits."

Source: <https://docs.b.ai/llmservice/pricing-and-usage/>. Note the two design consequences:
capacity is a **soft, load-dependent** ceiling, and exhausting it **spills into paid credits
rather than returning 429**.

---

## 3. What is actually enforced (the real limits)

None of this is documented; all of it is observable. The API node runs **one-api** — proven by
the response header `x-oneapi-request-id: 20260911084327360003369c955d568nO4HRdch` on a live
authenticated `GET https://api.b.ai/v1/models` (HTTP 200, 2026-09-11, real account key).
Observed wall classes:

| Class | Verbatim signal | Scope |
|---|---|---|
| Per-account rate limit | `429` code `1302` — `您的账户已达到速率限制，请您控制请求频率` | one key |
| **Empty-body 429** | HTTP 429 with a **completely empty** body — no `Retry-After`, no window, no limit text | shared lane |
| Account concurrency | `B.AI: Too many pending requests` | one account |
| Relayed upstream lane wall | `The request rate exceeds the current model Concurrency limit 1200. Please reduce the request frequency or contact Tencent Cloud support to request a higher limit.` and the same shape with `TPM limit 340000000` | **every** key on that model |
| Channel-pool exhaustion | `503` `No available channel for model glm-5.3-flash under group default (distributor)` | every key on that model |
| Edge failure | `502` with an nginx HTML body (`<title>502 Bad Gateway</title>`), all accounts simultaneously | provider-wide |
| Prefill timeout | `504` `http2: timeout awaiting response headers` on large prefills | per request |
Sources: onegw's live ring buffer — a 400-entry mixed-provider window captured 2026-09-11, in
which b-ai rows were 187×HTTP 200, 56× empty-body 429, 16× `Concurrency limit 1200`, 21× 504,
2× client-cancel 499 and 1× HTML-502 — plus `docs/PRD.md`, this research's live probes, and
independent teams:
[hermes-agent#102789](https://github.com/NousResearch/hermes-agent/pull/102789) (*closed*, P3),
[Comodor#8](https://github.com/ifekri/Comodor/pull/8),
[openmake_llm#717](https://github.com/openmake/openmake_llm/pull/717),
[quant_agent#392](https://github.com/songlinhe5-lab/quant_agent/pull/392).

### 3.1 There is no observability surface at all
- **No rate-limit headers on any successful response.** Two live 200s (the `/v1/models` GET and
  a streaming `/v1/chat/completions` call, 2026-09-11, real account key) carried exactly:
  `date`, `content-type`, `transfer-encoding`, `connection`, `cache-control`,
  `x-accel-buffering`, `x-oneapi-request-id` — **zero** `X-RateLimit-*`, `Retry-After`, or
  quota headers. Grep for `rate|retry|limit|quota` across the header set: none.
- **No quota/usage endpoint is reachable.** Guessed non-inference paths
  (`/v1/quota`, `/v1/usage`, `/v1/rate_limits`, `/v1/dashboard/billing/usage`,
  `/v1/subscription`) are refused by an explicit path allowlist — the body names the permitted
  routes, so this is one-api's by-design inference-only node rather than a throttling signal:
  `403 {"message":"HTTP node only allows access to inference API paths
  (/v1/chat/completions, /v1/messages, /v1/responses, /v1/models, /v1/images/*)","success":false}`
  (live probes, 2026-09-11).
- **`/v1/models` carries no tier flag** — just `{id, object, created, owned_by,
  supported_endpoint_types}` for 47 ids.

**Consequence: a client cannot know its rate state until it has already been rejected**, and
B.AI's rejection may be a bodyless 429 with no retry guidance. Retrying quickly makes it worse:
retries land inside the still-closed window; a **closed, P3-labelled** PR proposed instead a
60→90→120→180 s long-backoff table applied from attempt 1.

### 3.2 Measured free-key ceilings
| Quantity | Value | Evidence |
|---|---|---|
| Sustained request rate per key | **≈4.3–6.5 attempts/min** onset on `glm-5.3-flash` at 150–300K-token inputs (7 keys, 200 requests, 284 s) | onegw's **own production observation** — the onset that motivated its `rpm = 5` per-account governor (onegw #56); **not** independent third-party evidence |
| Concurrency per key | **≈1** — 5 parallel calls → 5/5 HTTP 429; ~15 simultaneous → all 429 | openmake_llm#717, quant_agent#392 |
| Decode rate on fast lanes | 40–76 tok/s measured on b-ai legs | onegw PRD |
| Lane variance | **7×** — 3 identical concurrent 120-token requests took 2.39 s / 17.33 s / 2.67 s | this research, 2026-09-11 |

That last row is the mechanism worth internalising: **identical concurrent requests do not get
identical service.** B.AI is a reseller whose `owned_by` field leaks the real backend pool —
`azure` (GPT), `mixai` (Claude/MiMo/Qwen), `vertex-ai` (Gemini), `ali`, `bttinfergrid`,
`deepseek`, `minimax`, `unknown` (GLM/Hy3) — so throughput per request is inherited from
whichever upstream channel and lane the distributor assigns, not from a documented B.AI cap.

**The vendor states this trade-off itself.** The same announcement describes *"a tiered API
system characterized by 'official stability guarantees and self-selected lowest-priced
options'"* plus a *"smart routing"* feature — i.e. B.AI openly positions its routes as trading
**stability against price**, which is exactly the mechanism this 7× spread measures. Scale
context for why the shared lanes saturate (vendor PR figures, 2026-09-03): **1.33 trillion
tokens/day**, **10.86 million API calls/day**, 8.19 trillion cumulative over 15 days, 220k new
API users, **2.3 million total users**.

---

## 4. Gaps and intentional absences

- **No numeric free-tier limit exists in public.** Not in the docs repo, the live docs, the org
  repos, `/v1/models`, or the web app. This is an absence, not a missed search.
- **No free-specific bucket** was found: the walls observed are per-account and per-model-lane,
  and a free key is throttled by the same machinery a funded key is `[INFERENCE]`.
- **No evidence of deliberate decode-throttling of free users.** The degradation observed is
  silent *capacity* loss (channel exhaustion, lane crowding, edge flaps, prefill timeouts),
  not a rate cap on tokens/s.
- **The independent third-party axis is index/egress-limited, not empty `[INFERENCE]`.** No
  reachable general index carries this vendor: Bing's index has no pages for it (quoted
  `"bankofai.io"` returned Ctrip; quoted `"chat.b.ai"` returned ChatGPT), HN Algolia returns
  **0 hits** for `chat.b.ai` / `api.b.ai`, and linux.do (Discourse `/search.json`) and Reddit
  are walled *outright* — 403 / "blocked by network security" — **including through a
  third-party reader's egress**, so this is not local IP blocking. `b.ai` is additionally a
  contested token (theb.ai and others). The only third-party signal that surfaced at all is in
  GitHub PR threads, not blogs or forums; the two code-search hits for `"api.b.ai"`
  (`Rescenix/Yosuri`, `kslamph/multikey`) carry no rate constants — just an `io.LimitReader` and
  a `hostOf(baseUrl) === "api.b.ai"` pool check — so no client in the reachable index holds
  B.AI's numbers. Treat "no community reports found" as *unreachable*, not *nonexistent*.
- **The `Concurrency limit 1200` / `TPM limit 340000000` numbers are relayed, not B.AI's own** —
  the wording names Tencent Cloud support `[INFERENCE]`.
- Unreadable: `status.b.ai` does not resolve; X (`x.com/BAI_AGI`) and Telegram
  (`t.me/BAI_agi`) announcement channels could not be read for limit-change notices.

---

## 5. What this means for onegw

1. **Treat b-ai throughput as undeclarable, not merely unknown** — no headers, no quota API, no
   documented cap; only a hit-the-wall signal. Adaptive probing (speed EWMA + bench) is the
   only viable strategy, and it is already what `strategy = "fastest"` does.
2. **The empty-body 429 is the dominant wall** — 56 hits in one ~10-minute window, 3.5× the
   textual `Concurrency limit 1200` class — so it must stay classified as
   shared/wall-within-window; per-key rotation cannot help.
3. **Lane variance is per-request, not per-model** — a good EWMA can be stale within seconds.
   Keep the per-attempt timeout tight (b-ai 504s are prefill-timeouts, not dead lanes).
4. **Per-key ≈5/min at large prefills is a *governed operating point*, not a vendor-published
   cap** — onegw's own `rpm = 5` per account, derived from the 4.3–6.5/min onset it observed.
   Treat it as the planning number for how many concurrent sessions b-ai can absorb before its
   own accounts become the bottleneck.
5. **Re-check GLM-5.3-Flash pricing after 2026-09-12 10:00 UTC+8.** If the free phase ends, the
   `free`/`dev` combos' leg #1 becomes paid at `$0.50 / 1M output` — `strategy = "fastest"`
   ordering plus the shared wall already limits the exposure, but the assumption "leg #1 is
   free" may silently expire.

---

## 6. Optimization plan — speed & success on the b-ai legs

Grounded in live counters and ring rows from the serving gateway (2026-09-11) plus a code
inventory. **Where the time actually goes first:**

| Measurement | Value |
|---|---|
| `b-ai/glm-5.3-flash` attempts | 645×200 / **327×429** (≈33% walled) / 6×504 |
| `b-ai/qwen3.8-flash` attempts | 483×200 / 104×429 / **99×504** (≈17% timeout) |
| Decode speed (provider EWMA) | **63.6 tok/s** |
| Input size per request | **800–262,000 tokens**; real requests 156K–262K, ~98% `cache_read` |

Ring rows on successful (200) requests — decode time vs whole-request wall:

| model | input tok | `ms` (decode) | `e2e_ms` | delivered tok/s |
|---|---|---|---|---|
| glm-5.3-flash | 210,926 | 2,390 (54 tok/s) | **84,610** | 1.5 |
| glm-5.3-flash | 211,919 | 3,847 | **91,285** | 2.0 |
| qwen3.8-flash | 233,066 | 961 | **73,704** | 1.6 |
| qwen3.8-flash | 233,246 | 2,811 | 13,549 | 15.1 |
| qwen3.8-flash | 163,375 | 5,745 | 12,156 | 42.4 |
| qwen3.8-flash | 807 | 9,024 | 12,836 | 61.3 |

**The bottleneck is prefill + retry wall-time on 150–260K-token requests, not decode.** A
request that answered in 84.6 s decoded for 2.39 s of it, and one at 73.7 s decoded for 0.96 s.
Optimizations must target *prefill latency, timeout burns, and lane contention*.

### Ranked changes

| # | Change | Type | Why (evidence) | Expected effect | Risk |
|---|---|---|---|---|---|
| 1 | **Input-size-aware leg ordering** — `strategy = "fastest"` sorts on decode tok/s (`reorderBySpeed` → `ModelTPS`, `router/task.go:336`), but at 150–260K input the e2e term is prefill, not decode. Fold an **e2e/prefill EWMA bucketed by input size** (e.g. <32K, 32–128K, >128K) and order by *predicted total time for this request*: `prefill_est + out/decode_rate` | code | 92 tok/s decode still finished in 47 s (232,621 in); a 54 tok/s leg took 84.6 s | picks the leg that actually finishes first for the dominant traffic class | needs ≥4 samples per bucket before it outranks configured order |
| 2 | **Ratio-based lane health** — `noteHeaderTimeout` benches only on 3 strikes inside a *tumbling* 3-min window (`provider.go:449-451`), which a 17%-timeout lane never trips | code | 99×504 on qwen3.8-flash, **no storm bench ever fired** | stops re-burning 75 s per request on a bad lane | over-benching a recovering lane (mitigate: ≥3 events + short bench) |
| 3 | **Post-200 stall watchdog** — no post-header deadline exists; a 200 streaming at 1.3 tok/s is `io.Copy`'d to completion (`server.go:911`) | code | the crawl is *unobserved by anything* | converts a 60 s crawl into a fail-over | retry needs a replayable body (whole-body prefix) |
| 4 | **Per-account concurrency ≈1** — `max_concurrency` is provider-wide (`provider.go:520`) and unset; set `max_concurrency = 7` on b-ai ≈ 1 per account | config | vendor wall is concurrency ≈1/account (5 parallel → 5/5 429; our 33% 429 rate) | fewer queued 429s, less rotation latency | caps total b-ai in-flight at 7 |
| 5 | **More free lanes in combos** — add `b-ai/hy3` + `b-ai/mimo-v2.5` (both advertised *and* free); only 2 b-ai lanes are in play today | config | different models sit in different concurrency buckets (PRD-verified) | fewer shared walls, more parallel capacity at 0 Credits | none material |
| 6 | **Default reasoning effort** — glm-5.3-flash thinks at `max`; coercion only *fixes invalid* values, with no knob to lower an unset effort (`coerceEffort`, `server.go:1378`) | code + config | reasoning = 137k of 243k output tokens (**56%**) | ~½ output tokens + lower TTFT | quality drop where deep reasoning was wanted (opt-in per model glob) |

### ⚠ Pre-first-byte budget: do NOT ship a blunt provider-wide override

The obvious "shorten b-ai's 75 s budget" is a **regression trap**. The live config's own comment
records why 75 s is where it is:

> `# Pre-first-byte upstream budget (dial+TLS+upload+prefill). 60s aborted`
> `# 100-300K-token glm-5.3-flash prefills (http2: timeout awaiting response`
> `# headers, 2026-09-08 ~22:13).`

Because `glm-5.3-flash` **is** leg #1 of both the `free` and `dev` combos, a provider-wide
override cannot be scoped to "the flash lanes" — it would reintroduce that abort on the busiest
leg. And qwen3.8-flash is not a safe shortcut either: the ring shows *it* also serves
232K–262K-token requests. If this is ever tuned, the budget must be **per (provider, model)
glob** or **input-size-aware** (`base + k × input_tokens`), with glm-5.3-flash held at ≥75 s and
any new value derived from measured prefill times — not guessed. Until then, item 2 (lane
health) is the correct fix for the timeout storm, because a genuinely bad lane should be
benched rather than given a shorter leash on valid large prefills.

### Already handled — do NOT re-implement

Per-account RPM governor (`rpm = 5`), cross-account burst → shared-wall park, wording walls, 503
channel-empty fall-through, **header-timeout storm bench**, decode-speed EWMA reordering,
`always_thinking` coercion / `no_thinking` strip, prompt caching (98% `cache_read` without any
`cache_profile`, because the empty profile keeps the body byte-identical), and
`NoSameTargetRetry` fall-through after a spent first-byte budget.

### Apply order

**4 and 5 are config-only** (a few lines, effective on config reload) — cheapest wins.
**1, 2, 3 and 6 are code** and belong in one change with tests. Do **2 before 3**, and treat the
pre-first-byte budget as a *measurement*, never a guess: instrument prefill time per
(provider, model) first, then decide whether any budget change is justified at all.

### Implementation status (2026-09-11, restored on the rotation master)

Lost in a shared-tree reset and recovered whole from the peer auto-snapshot
(`wip/peer-snapshot-20260911-182333`, 741d0ac), then cherry-picked onto the rotation master
(provider.go conflicts resolved to the committed strategy-based pick). Uncommitted, pending
review. Shipped in tree:

- `internal/provider/prefill.go` — prefill EWMA per (model, size bucket); the bucket is keyed
  on the same body-size estimate the router orders on, because a mismatch silently disables
  the steering (caught live: 27,814 actual tokens vs a 40K estimate).
- Size-aware combo ordering — `reorderBySpeed` scores by predicted seconds (`prefill_est +
  nominal decode`) once a leg has ≥3 samples at ≥32K tokens, else decode-only. Logs a
  `prefill-order` ring row whenever it changes a chain (`speed_order` kind).
- `onegw_provider_prefill_tokens_per_second_x100{provider,model,bucket}` gauge.
- `default_effort` — opt-in per provider; applied only to always-thinking models and only when
  the client sent no knob.

Evidence on the scratch gateway (rebased binary, live b-ai accounts): the ring row
`prefill-order in~40441tok: b-ai/glm-5.3-flash > b-ai/qwen3.8-flash` on a combo configured
qwen-first; gauge shows glm 17,589 vs qwen 12,887 tok/s (×100) prefill in the same bucket;
steer probes served glm at 2.1s, and when glm's lane 429ed the chain fell through to qwen
(`attempts=2`, 14.3s/5.7s) — composing with the in-flight key pick per §5. Dropped on
evidence: the post-200 stall watchdog (measured headers→first-chunk 0.07–1.38s). Unchanged
and still forbidden: any pre-first-byte budget change (the peer's occupancy pick is the fix
for the timeout storm; the 75s budget is documented on purpose). Live A/B of `default_effort`
is inconclusive on today's lanes (the vendor node now reports `reasoning_tokens: 0` for every
effort, measured) — the earlier 50-vs-392/395 measurement stands, and the knob stays off
unless configured.
