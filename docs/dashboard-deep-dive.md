# Dashboard deep dive: how to build the #41 revamp (issue #45)

*Research 2026-09-08. Companion to issue
[#45](https://github.com/FreePeak/onegw/issues/45) (build approach);
[#41](https://github.com/FreePeak/onegw/issues/41) stays the IA/scope
umbrella; #19 (console log) is the first slice.*

## Constraints (PRD rubric, applied to the dashboard)

fast > security > massive sessions > token saving > lowest RAM. Concrete
consequences:

- **Zero external assets** — the dashboard stays embedded in the single
  binary (`go:embed`); no CDN script, no runtime-fetched framework.
- **Serving it must not move the 100 MB RSS contract** — embedded assets are
  read-only bytes; live-push fan-out must be bounded.
- **Read-mostly** — pages are views over live config + usage; no editors
  (#11 stays optional/deferred).
- The IA (pages, sidebar, what each page shows) is already pinned in #41;
  this research pins *how to build it*.

## Stack decision: Go templates + htmx + uPlot (no JS framework)

Two viable paths were weighed against the rubric:

| | **A: Go `html/template` + htmx + uPlot** | B: bundled React+Tailwind single HTML (#41 sketch) |
|---|---|---|
| Payload | ~30–40 KB inline JS+CSS | ~200–400 KB opaque HTML artifact |
| Toolchain | none — pure Go, templates in-repo | Node toolchain inside a Go-only repo (bundler step, artifact checked in or built in release CI) |
| Live updates | first-class: htmx SSE extension swaps HTML fragments from an `EventSource` | hand-rolled event → state → re-render |
| Fit | the IA is tables, read-only cards, one log pane, two charts — server-rendered HTML is the natural shape for read-mostly pages | pays off only with dense client-side state we deliberately don't have |
| Dependency surface | htmx (~14 KB min.gz, dependency-free) + uPlot (~48 KB min) vendored once | framework + build-plugin upgrade treadmill |

**Decision: path A.** Server-rendered pages with `html/template`, htmx for
partial updates, uPlot for charts. No `package.json`. Path B is the
documented fallback if a future page truly needs client-side state.

Verified sizes (2026-09-08): uPlot README + bench table — uPlot 47.9 KB min
(Canvas 2D) vs Chart.js 4: 254 KB, ECharts 5: 1 MB, dygraphs: 132 KB; htmx
~14 kB min.gz'd per htmx.org; htmx SSE extension is a separate ~2 KB script,
both bundleable locally (no CDN requirement).

## Live data: SSE from the Go stdlib, not client polling

- `GET /admin/events?topics=health,usage,logs,quota` — one SSE endpoint,
  stdlib `http.Flusher`, zero dependencies.
- Bounded fan-out: **max 32 subscribers**, small per-subscriber ring
  (~8 KB); a stuck dashboard gets events dropped plus a `resync` event
  telling it to refetch — same philosophy as the byte-budget semaphore
  (a slow consumer can never grow RSS).
- Topics: `health` (1 s: inflight, RSS, budget held/cap, uptime), `usage`
  (5 s rollup delta), `logs` (push from the #19 ring-buffer sink), `quota`
  (on change).
- **Progressive enhancement rule**: every page renders fully server-side on
  load; SSE only adds live updates. No JS enabled → still a useful page.

## Auth prerequisite: cookie session

`EventSource` cannot set custom headers, so the current `X-Admin-Password`
header cannot ride an SSE connection. The already-planned session-cookie
login is therefore a hard prerequisite for any live page:
`POST /admin/login` → `HttpOnly` `SameSite=Strict` short-TTL cookie; header
callers keep working via 401 (keeps scripts + smoke.sh intact). Landing
order: cookie session first, then SSE/htmx pages.

## Admin API shape

- New grouped surface `/admin/api/v1/`:
  - `GET /admin/api/v1/usage?from&to&groupby=provider|model|key&cursor` —
    **cursor pagination** (rollups are keyed day+provider+model; offset
    breaks as data shifts), shared with the export path.
  - `GET /admin/api/v1/{providers,combos,quota,saver}` — read-only views
    over live config, keys masked.
  - `GET /admin/api/v1/events` — the SSE endpoint.
  - Uniform JSON error `{"error":{"code","message"}}`.
- Existing flat `/admin/{health,usage,quota,config,logs}` stay (scripts,
  smoke.sh, and muscle memory depend on them); deprecate later, never break
  in place.

## Charts (uPlot)

1. **Tokens/day stacked area** — input/output/cache-read per provider from
   the existing SQLite daily rollups (`store.QueryRange`), honoring the
   existing `?range=` filter (today/7d/1m/all).
2. **Request outcome strip** — 200/4xx/5xx per day from the same rollups;
   per-request history arrives with #19.

## RAM budget check

Embedded assets: read-only `[]byte`, no runtime cost beyond serving.
Template execution: one small alloc per request. SSE fan-out: bounded
(32 × 8 KB). Chart data: a few hundred rollup rows JSON per request. All
inside existing headroom — verify with `bench/memory.sh` before/after each
slice (same harness as M5), plus a browser check of the changed page.

## Landing order (small independently-shippable slices)

1. Cookie session login (prerequisite).
2. Shell: `internal/server/dashboard/` package with `go:embed` FS replacing
   the single `dashboardHTML` const; sidebar IA from #41; dark/light;
   `Cache-Control: no-store`.
3. Overview page + `/admin/events` `health` topic (replaces polling gauge).
4. Usage analytics page + uPlot charts + cursor-paginated API.
5. Logs pane (#19 sink + `logs` topic).
6. Read-only Providers/Combos/Quota/Saver views + CLI Tools preset cards.
7. Grouped `/admin/api/v1` completion.

Each slice ships with `bench/memory.sh` before/after + browser verification
against the live gateway.
