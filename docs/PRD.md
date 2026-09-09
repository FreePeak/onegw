*Last updated: 2026-09-10 (flap breaker refinements live, 6308d0a, pid 150: consolidated the
breaker strike to ONE site at Do's final error exit so plain JSON 502/503/504 bodies (the
common one-api shape) trip it — previously only HTML/empty/transport faults did; pinned by
TestFlapBreakerTripsOnPlainJSON502. edgeFault excludes NoSameTargetRetry (header-budget 504)
errors: they are REQUEST-shaped — one oversized prefill exceeded the gateway's own 120s
budget; a smaller request to the same provider succeeds — so 4 big prefills never park the
provider. Accepted tradeoff, documented once: a saturated lane (tokenrouter free) still costs
each request one full budget burn before fall-through — NoSameTargetRetry already skips the
same-target retry, and counting budget-504s would park providers under heavy prefills. Shared-
concurrency walls stay excluded. Mutation-checked (neuter exclusion → predicate test fails);
full suite green; zero-drop redeployed from archive HEAD. Attribution: breaker code body rode
the shared-tree sweep b6c08bc; classification refinement + tests 2f8c180; this consolidation
6308d0a. Earlier:)*
*Last updated: 2026-09-10 (b-ai HTML-502 flap breaker live, 2f8c180, pid 41359: deepdive of the
22:54:38–22:55:03 burst — every b-ai account (all 7) answered the STOCK nginx page
"<html><head><title>502 Bad Gateway</title>" simultaneously for ~25s while traffic before and
after served 100% 200. RCA: provider-WIDE origin-pool flap at api.b.ai's edge, not per-key
throttling (429 ladder can't see it) and not onegw (the HTML is their nginx, generated upstream
of any JSON API). Fixes: (1) Do classifies HTML error bodies as upstream_html_error with a
bounded one-line message (page title extracted) instead of leaking raw markup to clients/logs;
(2) provider-wide flap breaker on the account pool: 4 consecutive edge-class faults (HTML page,
empty body, transport unreachable, timeout, plain 502/503/504/52x — never auth-verify/parse-
reject rewrites, shared-concurrency walls, or 4xx) park the WHOLE pool for 15s, so Router.
Execute falls through to the next combo target with zero doomed upstream calls and direct
routes answer pool-empty 429 + Retry-After=window end; a success heals instantly, the window
half-opens exactly one probe, a failed probe re-arms. Cost today: ~1-2 requests pay the retry
before the breaker trips; sustained flaps pay ≤1 probe per 15s instead of a 7-account fan per
request (this burst: 17 doomed attempts → would have been ~5). Mutation-checked (neuter →
breaker suite fails); full suite green; zero-drop deployed from archive HEAD. Earlier:)*
*Last updated: 2026-09-10 (tokenrouter 8/min window — fix live, b6c08bc+aa91d07, pid 16156: ring
proof the "Maximum 8 requests within 1 minutes" budget is SHARED across keys (harvey 429ed with
~5 attempts in its trailing window while linh served 200s). Two changes: (1) `[[providers]] rpm`
is now a provider-wide shared token bucket gating every account (onegw.toml: rpm = 6, worst
rolling minute 2+6 = 8 = at-limit); drained budget answers pool-empty with an honest
Retry-After = refill instant instead of feeding the shared window doomed attempts. (2) A 429 body
stating a request-count window (APIError.RateWindow) benches the account verbatim for that window
(was: 10s ladder base that re-entered the closed window — ring: 429 @:46, retry @:57 429 again)
and rides the client Retry-After (60s, not generic 10). Mutation-checked ×3; archive HEAD
17/17 packages; live probes: 10/12 rapid hits governed locally (ms-fast 429s), 1 upstream
window 429 absorbed post-restart, 200s resume at 1/10s refill. Remaining ceiling: ≤1 window 429
per gateway restart (upstream window outlives process memory). See issue #67. Earlier:)*
*Last updated: 2026-09-10 (install.sh credential preservation, #66: the installer minted a
fresh gateway key on every run and pinned it as ONEGW_KEYS env — which replaces config-file
keys entirely — so reinstall/update rotated the API key out from under wired clients and a
stale-pid escape hatch could start a second config beside the live gateway under SO_REUSEPORT.
Fixed: config-first key resolution (flat + [[auth.keys]] forms), running-instance/port-busy
guards before any credential minting (clean exit 0, binary-swap-only beside a live gateway),
service-file env pinning only for keyless legacy configs, starter config on true first install
only. Live-proven in a sandbox: re-run beside a seeded config leaves it byte-identical, no
ONEGW_* env on the started process, old admin password + old client key both auth 200, wrong
key 401. The update-path half — `onegw update` dying on HTTP 401 Bad credentials from a stale
env GITHUB_TOKEN — is fixed by 96c1fd3 (anonymous public-repo fallback).)*

*Last updated: 2026-09-09 (ponytail inject mode live, 6ca71ab, issue #65: onegw now
  ships the [ponytail](https://github.com/DietrichGebert/ponytail) lazy-senior-dev
  ruleset (MIT, adapted) as `[[saver.inject]] mode = "ponytail"` — the gateway
  prepends the YAGNI → reuse → stdlib → native → dependency → one-line → minimum
  ladder to the system prompt of matching requests, so every coding agent behind
  the gateway (Claude Code, Codex, hermes, omp…) gets it with zero per-client
  installs. Idempotent via the existing `onegw-terse-directive` marker; a client
  that already runs the ponytail plugin (tagline "lazy senior dev" detected in
  the body) is NOT stacked a second time. Tests: ladder injection + client-plugin
  skip + config validation, all mutation-checked. Live: injected rule visible in
  /admin/config (pid 72826, hot-reloaded via SIGHUP; live binary = peer 401fix
  build which includes 6ca71ab, marker count 3), and end-to-end behavioral proof
  through the live gateway — model reasoning quotes the injected ladder's
  rung 3 verbatim. Live config note: during the session a peer redeploy rotated
  the live config from ~/.onegw/onegw.toml to the repo onegw.toml; the ponytail
  rule was added to BOTH files so the rule survives either config path.)*

*Last updated: 2026-09-09 (update check 401 fix, 96c1fd3: the environment's stale
  GITHUB_TOKEN made every release check fail with "HTTP 401 Bad credentials" even though
  FreePeak/onegw is PUBLIC and anonymous reads work. internal/update now sends the token
  when configured and, on 401, retries the SAME request once without credentials — both
  the release check and the binary download (issueGET). Public users need no token;
  private forks keep single-request valid-token reads; the error hint now says when the
  anonymous fallback itself failed (private repo behind a bad token). Regression tests
  TestLatestStaleTokenFallsBackToPublic + TestDownloadStaleTokenFallsBackToPublic
  (mutation-verified) + live proof against real GitHub with the actual stale token.
  Zero-drop deployed as v0.13.5-401fix under fresh hub record **onegw-live2** (the
  onegw-sup record was exited/wedged again): pid 72826, restart=on-failure, persist
  detached, env token vars scrubbed in the record, single listener verified, /admin/update
  last_error cleared (latest=v0.13.6, no 401). DEPLOY HANDOFF for the next session: the
  live record name is now **onegw-live2** with binary /tmp/onegw-401fix-bin — overlap-bind
  your new pid, poll health owner.pid == yours, SIGTERM 72826, re-create/adopt the record
  with YOUR binary path.)*

*Last updated: 2026-09-09 (live supervision: the gateway had NO auto-restart — every
  deploy.sh generation left a setsid orphan (PPID 1) and the hub daemon records
  (onegw/onegw-live) that could have supervised it were exited/wedged ("unacknowledged
  completion notifications"); a crash would have frozen all coding sessions indefinitely.
  Fixed zero-drop: fresh hub record **onegw-sup** (restart=on-failure, persist=detached,
  same binary+config) overlap-bound while the old orphan served, old pids drained by
  explicit SIGTERM, single listener verified (health owner.pid=70282 ×3, /v1/models 200,
  ~21 inflight across the swap). Crash semantics now: on-failure restarts crashes;
  graceful SIGTERM stays down by design (deploys deliberately stop it); reboot needs a
  launchd KeepAlive (not yet installed). Wedged names onegw/onegw-live abandoned. DEPLOY
  HANDOFF (the recipe for the next deploying session): onegw-sup is LIVE, not wedged —
  hub-start on that name fails until 70282 exits. Correct sequence after spawning your
  new binary overlap-bound: (1) poll /admin/health owner.pid == YOUR new pid; (2) SIGTERM
  the old 70282 explicitly; (3) `hub stop onegw-sup` clears the exited record (hub stop
  on an already-exited record can wedge — if it does, use a FRESH name with restart=
  on-failure persist detached, same spec); (4) hub-start under the cleared/fresh name so
  supervision survives this generation. Do NOT leave your replacement unsupervised: a
  PPID-1 setsid child has NO crash auto-restart. Binary path is volatile (/tmp/onegw-*
  per deploy generation): the hub record's retained spec re-execs the OLD path on
  auto-restart — re-create the record with the NEW path if the old /tmp file was
  replaced.)*

*Last updated: 2026-09-09 (docker anonymous-volume fork closed, 2d6f08d: dropped
VOLUME ["/data"] from the Dockerfile — it allocated an anonymous volume on every
plain docker run, so the documented pull+recreate update path silently re-homed
usage.db onto a fresh volume (the containerized variant of #62's data-loss class);
persistence is explicit (compose onegw-data named volume / -v onegw-data:/data).
ContainerGuidance now prints the exact volume-preserving recreate commands +
docker cp escape hatch. Local volumes re-verified all Postgres — this Mac's incident
ran through the native path fixed by 47e2984; issue #62 carries the full RCA +
docker note)*

*Last updated: 2026-09-09 (live ops: `[update] check_interval = "12h"` set in the live
config — 2×/day background checks, confirmed interval_seconds=43200 on /admin/update;
live gateway cut over zero-drop to **v0.13.1** (pid 75149) through its own
POST /admin/update apply path — the dashboard Version card's Update button, live-proven
end to end. Found + filed #63: the dashboard's PUT /admin/config/reload does not sync
the outer-mux /admin/update handler's config (SIGHUP does) — dashboard reload leaves
update endpoints 401ing until SIGHUP/restart — FIXED same day: Server.SetOnConfigReload
hook fired by Server.Reload, main passes curCfg.Store (regression test
TestDashboardReloadKeepsUpdateEndpointAuthed in cmd/onegw/update_admin_test.go).)*

*Last updated: 2026-09-09 (relative data_dir data-loss incident, fixed 47e2984: a
relative `data_dir` resolved against the process cwd, so when the update-feature
install.sh restart launched the gateway from ~/.onegw with a copy of the user's
config, it silently opened a FRESH EMPTY usage.db — usage history "disappeared".
Data was never deleted: repo data/usage.db held it all; merged 153 rollup rows
into the live ~/.onegw/data/usage.db (INSERT OR IGNORE; PK includes node_id so
old/new rows can't collide) while serving. Hardening: internal/config anchorDataDir
resolves a relative data_dir against the config FILE's directory (one config file
means exactly one data dir regardless of launcher cwd; absolute paths, "memory"
sentinel, and the absolute default untouched); both live configs pinned absolute;
zero-drop redeployed (pid 19071, archive-built 47e2984, /admin/update auth re-synced).
Recovery copy: /tmp/onegw-recover/usage.db. Filed as issue #62. Follow-up: peer WIP
check_markers in scripts/deploy.sh false-fails under pipefail (strings|grep -q SIGPIPEs on match).)*

*Last updated: 2026-09-09 (dashboard update button, #61: Settings page gains a
Version card — running vs latest release, Check now / Update now — backed by new
cookie-gated GET/POST /admin/api/v1/update in internal/server/admin_update.go
(same internal/update service as `onegw update` and auto-apply; /admin/update
stays the header-only CLI/probe surface). Local running: full zero-drop
self-handoff with live polling until the new build answers. Docker container:
the button reports status and, on apply, answers 409 with the host-side
docker pull + recreate guidance — the image owns the filesystem, by design.
Live-verified both modes on a throwaway gateway, incl. a real handoff to
v0.12.7.)*

*Last updated: 2026-09-09 (Docker release path audited for VPS deployments: latest image =
v0.12.7 = master tip — only docs commits landed after the tag — and the release run's docker
job pushed both tags. But the GHCR package is PRIVATE: anonymous `docker pull`/`manifest
inspect` → 403/denied and the package page 404s, so the documented container upgrade path
(PRD §self-update container guidance, internal/update/docker.go ContainerGuidance) fails for
users until org package visibility is flipped Public. Filed as issue #60; interim =
`docker login ghcr.io` with a read:packages PAT or `docker compose up -d --build` on the VPS.)*
*Last updated: 2026-09-09 (cursor provider live — KindCursor promoted from
fail-fast skeleton to a full AgentService+ChatService executor (issue #12
follow-up, commit 12fd081): Connect-RPC/protobuf port of 9router's cursor
executor, Jyh-cipher checksum (JS shift masking emulated), full-duplex
handshake over net/http (pre-answering the context question does NOT work —
live-proven), system text folded into the user turn (field 8 kills the turn,
live-proven 3/3), stop-frame termination (upstream never EOFs, 10s
keepalives). Live: binary 12fd081 zero-drop deployed (pid 8774 → 10264),
onegw.toml kind="cursor" provider hot-reloaded (9 providers), cursor/gpt-5.2
answers PONG with real upstream usage (11859/6) through the live gateway,
streaming verified; token from 9router DB (exp 2026-11-02, no refresh —
re-import when rotated). Tools path: Cursor's ChatService does not register
MCP tool defs upstream today (same as 9router production).)*

*Last updated: 2026-09-09 (README dashboard section synced to the editable
console — Providers/Combos in-page editing, Quota/Token Saver split out,
Settings maintenance card — overview screenshot re-shot after redeploying
master 46936d1 zero-drop (pid 91238 → 8774; old binary predated the
dashboard tabular-nums commit); docs-only, no code change.)*
*Last updated: 2026-09-09 (dashboard M.O.N.K.Y OS revamp 74fbff2 — flat ink/indigo/neon design,
branded sidebar, right-rail clock/status/ranking, overview chart — zero-drop deployed pid 91238 (rebased onto origin as 74fbff2).
Earlier: commandcode-520 + glm-empty-500 RCA 289cd47+d41d078.)*
**2026-09-09 — commandcode 520 terminal + glm empty-500 RCA (289cd47, test fix d41d078):** the
dashboard showed `commandcode/unresolved 520 server_error` (transient; "Upstream model provider is
temporarily unavailable. Please try again in a moment.") killing combo chains, and
`glm/glm-5.3-flash 500 upstream_error` rows with NO message. Root causes + fixes, all
regression-tested and mutation-checked: (1) Cloudflare 52x (520-527) missing from
`types.Retryable` — commandcode's own edge answers 520 while ITS model-provider pool flaps; a
terminal 520 ended the chain instead of falling through; now retryable
(TestExecuteFallsThroughOnCloudflare520). (2) Zero-byte upstream error bodies decoded to an EMPTY
message — live glm evidence: api.z.ai /api/v1 returned 500s with no payload during model
deprovisioning (key lost access mid-day: 123 ok at hour 06 → 500-empty burst → clean 403
model_access_denied). provider.Do now surfaces type=upstream_empty_body with an honest message.
(3) Router.Execute's pool-empty 429 keeps status/Retry-After (#48 contract) but names the last
real upstream cause in the message instead of claiming "rate-limited" when the pool was drained
by per-model 403s (TestExecutePoolEmptyMessageNamesCause). (4) KnownModel: slash-models
advertised in a provider models table ("z-ai/glm-5.3-flash") stay resolved in failure-row labels
— they collapsed to "unresolved" because the provider-prefix branch returned early (that is why
the console read commandcode/unresolved; TestKnownModelAdvertisedSlashModel). (5) Stale
TestBufferedPathSharedConcurrency429RetriesThenHints was red on pristine origin/master (pinned
pre-359e5a0 two-attempt behavior); renamed ...SurfacesOnceWithHint, hits=1 per the landed
contract (d41d078). glm/harvey key state is upstream-owned: 403 model_access_denied on /api/v1,
429 code 1113 insufficient-balance on /api/paas/v4 — combo targets fall through; direct requests
surface the pool-empty 429 with the honest cause until the key is re-provisioned.
**Deploy-loop incident + provenance recipe:** the live pid churned 38299→791→30474→89605→26764
in ~10 min while a peer session redeployed from /tmp/onegw-mk — a NON-git staging tree exported
~16:29 (before the 16:44-16:47 pushes) whose loop re-archives a pinned pre-fix snapshot each
cycle, overwriting refreshed sources and rebuilding a binary WITHOUT these fixes. Not a crash
loop: every cycle is a deliberate deploy.sh-style takeover (/tmp/onegw-new.log). Any session can
re-prove provenance in one command:
`strings "$(curl -s -H 'X-Admin-Password: <pw>' http://127.0.0.1:8080/admin/health | jq -r .owner.argv[0])" | grep -c upstream_empty_body`
— ≥1 = fixes live; 0 = the serving binary predates 289cd47, deploy origin tip (a stable
origin-tip binary is kept at /tmp/onegw-rca-tip-bin). Live serving verified: commandcode 200
through the gateway post-deploy.
Earlier:

*Last updated: 2026-09-09 (tokenrouter free-lane capped at rpm = 7 — user-stated limit,
one under the upstream's "Maximum 8 requests within 1 minutes" wall, same governor sizing
discipline as the b-ai keys; overflow rotates to commandcode/opencode without logging 429s.)*
*Last updated: 2026-09-09 (tokenrouter free-lane capped rpm = 8 — upstream "Maximum 8 requests
within 1 minutes" per their own 429 body; same governor mechanism as b-ai keys.)*
*Last updated: 2026-09-09 (live config: `[server] buffered_budget_bytes` raised 48→100 MiB (104857600) on
  the running gateway — global in-flight buffered-bytes budget, the "RSS contract" (not a hard RSS cap;
  total-process memory is GOMEMLIMIT, not config-exposed). Edit in gitignored onegw.toml; fresh pid 39967
  loaded it at startup after the peer's zero-drop deploy — verify-then-trust, no second redeploy; verified
  via /admin/config, double health, /v1/models 200. Earlier: shared-wall fall-through 359e5a0 + glm demotion/rpm cap live,
  pid 39967 — earlier: Merlin research #58 + RPM governor #56, below.)*

**2026-09-09 — shared-wall fall-through + glm direct demotion (359e5a0, deployed pid 39967):**
with >=20 sessions in flight the Tencent model-wide "Concurrency limit 1200" wall kept
surfacing on b-ai/glm-5.3-flash because Router.Execute burned a 1s in-target backoff and a
second attempt on the SAME model before falling through — a different key hits the same wall;
only a different MODEL sits in a different concurrency bucket. SharedConcurrency 429s now
fall through to the next combo target immediately (same shape as NoSameTargetRetry); direct
routes surface the wall once with an honest 2s Retry-After; ordinary per-account 429s keep
their same-target retry (account rotation does help there). 3 regression tests,
mutation-checked. Deploy-day pid discipline incident: the pid-43915 build was silently
replaced 4 minutes later by a peer's pre-359e5a0 binary (/tmp/onegw-504fix-bin, built 08:57Z
vs 359e5a0 pushed 09:07Z) — the fix was pushed but NOT live; re-provenance checked (governor
symbols newTokenBucket/refillAt present in both binaries) and origin tip re-deployed
(pid 39967). New top error source emerged under load: glm/glm-5.3-flash direct (single
z.ai key) — 48 empty-body 500s/10min, and 500s never bench, so the chain burned 2 attempts
per hit. Live config: glm demoted to LAST rung in dev (overflow only), glm/harvey capped
with rpm = 3 (Zhipu per-account concurrent ~1-2 at 150-300K inputs; no real capacity lost).
Post-fix 7-min window at ~24 inflight: glm 500s 0 (was 48/10min), b-ai per-account 429
1.3/min (was ~3.3/min), shared-wall 0.6/min, ring row success 81% (rest are mid-chain
fall-throughs). tokenrouter free-lane 429s (8-req cap on z-ai/glm-5.3-free) remain the
largest fall-through source — upstream-owned, absorbed.
*Last updated: 2026-09-09 (Merlin AI upstream research #58 published — wire contract live-verified,
implementation pending; earlier: b-ai per-account 429 RCA + RPM governor 729c190 + free/dev
rotation, zero-drop deployed pid 96925, closes #56 — earlier: four-lane wave
#54/#32/#34/#35/#55/#14, below.)*
**2026-09-09 — Merlin AI (getmerlin.in) upstream research (#58, research-only):** deep dive on the
pricing page + internet adapters; wire contract live-verified end-to-end (Firebase anonymous
signUp with Merlin's public Firebase web key (full value in issue #58) → 1h idToken →
`POST www.getmerlin.in/arcane/api/v2/thread/unified` SSE → free model glm-5.3-flash streamed
MERLIN-OK; guest hitting a paid model → in-band `PRO_ONLY_MODEL` error event). Pricing: Free $0
(5 free-tier models), Pro $29/mo or $19/mo yearly (regional promos $2-$8/mo annual-billed),
Teams $19/seat; discounted plans carry a $5/day + $20/month fair-usage cap. Deliverable in
issue #58: options A (native kind="merlin" — refresh-token account + ForcedStream SSE
translator, mirrors commandcode) vs B (self-hosted getmerlin-worker bridge as kind=openai).
Earlier:
**2026-09-09 — b-ai per-account 429 RCA + RPM governor (729c190, zero-drop deployed pid 96925):**
the console showed two distinct b-ai 429 classes: `gateway_error` "Concurrency limit 1200"
(Tencent GLM model-wide limit shared by ALL of the reseller's traffic — already handled by #52's
shared-wall backoff, honest per client) vs `upstream_error` 您的账户已达到速率限制 — the
PER-ACCOUNT limit, which the pool only handled reactively: the adaptive ladder benches a key
AFTER the upstream already rejected an attempt. Live log (200 requests, 284 s, 42.3 req/min,
92% glm-5.3-flash): every one of the 7 b-ai keys failed its per-account 429 at 4.3-6.5
attempts/min while the pool kept rotating into doomed attempts. Fix (proactive governor):
`Account.RPM` (toml `rpm`, 0 = uncapped) caps attempts/min with a refill token bucket
(burst capacity 2, rate rpm/60, take() the sole consumer, refillAt() hints without
consuming) — next() and sticky pins rotate past a drained bucket like a cooldown, so the
pool spreads load BEFORE the upstream limit strikes; weighted slots share one bucket; a
fully blocked pool reports the honest soonest-serve (max cooldown/refill) for fall-through
Retry-After (cooldown-only hint regression pinned). Live config: rpm = 5 on all 7 b-ai keys
(observed per-key ceiling: failures onset at ~6/min, so the cap runs at ~85% of the observed
limit), free/dev combos retargeted with model rotation — free: b-ai/glm-5.3-flash →
tokenrouter/z-ai/glm-5.3-free → b-ai/qwen3.8-flash → commandcode/z-ai/glm-5.3-flash →
commandcode/deepseek/deepseek-v4-flash → opencode/mimo-v2.5; dev: b-ai/glm-5.3-flash →
b-ai/qwen3.8-flash → glm/glm-5.3-flash → tokenrouter/z-ai/glm-5.3-free →
commandcode/z-ai/glm-5.3-flash (b-ai limits are model-scoped, so glm-5.3-flash saturation no
longer blocks qwen3.8-flash on the same key; xai/grok-4.6 — dead 403 terminal target —
removed from dev). Rate-limit research: b.ai publishes no numeric per-key limits ("retry
with exponential backoff and reduce concurrency"); Zhipu GLM coding plans gate by
per-account concurrent sessions, tiered — consistent with the observed ~6/min onset at
150-300K-token inputs. Verification: 7 governor regression tests mutation-checked; full
./... green on the rebased tree; deploy zero-drop; live burst 20/20 dev-combo requests OK
with the governor active. (closes #56)

*Last updated: 2026-09-09 (four-lane wave landed + merged on origin/master, full suite green on
the merged tree: #54 task-aware combo reordering (d94921d) — local stateless classifier
(light/standard/heavy/critical, no LLM) + config-declared model power ([[providers.tier]], 0-150)
+ stable re-sort of combo targets inside router.Execute before account selection (never removes
targets), `task_routing = off` default, decision rows to the #19 log ring only when order changes;
#32/#34/#35 (dd6d92b, reconciled from the earlier encode-layer WIP): cache_control /
prompt_cache_key / session_id survive cross-format translation and re-emit only on accepting
wires, never invented; per-provider `cache_profile` (claude-anchor | dashscope-marker |
sticky-key) anchors LAST at the attempt choke point with profiled providers herded off the
stream fast path; saver sticky gate no longer flips the request prefix (all-or-nothing global
gate only when the canonical form is already cache-stable); duplicate #50 coercion deleted in
favor of master's coerceAlwaysThinkingUnified + EncodeAnthropicRequest TopK-drop regression
restored; #55 VPS deploy foundation (bb023d3) — docs/vps-deploy.md runbook, scripts/deploy_vps.sh
zero-drop VPS analog (build from git archive HEAD, NEW-before-OLD takeover, live-tested on
scratch port incl. abort paths), hardened contrib/systemd/onegw.service; #14 follow-through
(7bd9d9b) — release workflow publishes SHA256SUMS, install.sh verifies downloads and proves
the install with `onegw version`, Dockerfile bounded GO_BUILD_JOBS. Integration merge be699f6
over peer's 987a849 dashboard revamp; conflicts resolved: server.go identity+task ctx wiring
unified, config.go/provider.go additive both-sides.)*
*Last updated: 2026-09-09 (dashboard revamp 987a849: ui-ux-pro-max design pass — slate
glassmorphism tokens, Fira Sans/Fira Code vendored, contrast-fixed both themes — PLUS the
provider/combo config editor: PUT /admin/config/providers and /admin/config/combos popup
modals that splice the TOML file (comments + foreign keys preserved), validate with
config.Load before the atomic write, then Load+Reload — save hot-reloads the live gateway
and publishes an SSE config event; secrets never leave the file (empty key = keep existing);
mutation-checked tests, staged-tree suite green, zero-drop deployed — earlier: live config: commandcode (GOAT plan, 48-model roster, GLM-5.3 always_thinking probes) + tokenrouter (new-api aggregator, 80 openai-type models, $0 balance) providers hot-reloaded into onegw.toml)*
**2026-09-09 — dashboard revamp + config editor (987a849, zero-drop deployed):** the admin
console was redesigned with the [ui-ux-pro-max](https://github.com/nextlevelbuilder/ui-ux-pro-max-skill)
design system (Real-Time/Operations pattern → slate glassmorphism, emerald interactive
accent, Fira Sans/Fira Code vendored as OFL latin woff2 subsets — old three fonts removed).
Accessibility per the skill's checklist: visible focus rings, reduced-motion guard, skip
link, aria-labelled icon buttons, 4.5:1 contrast verified by screenshot review in BOTH
themes (two rounds of contrast fixes), uPlot charts now read theme tokens and redraw on
dark/light flip. NEW capability (user request): the Providers and Combos pages have popup
editor modals — Add/Edit/Save writes `[[providers]]`/`[[combo]]` blocks through
`PUT /admin/config/{providers,combos}`: line-splice preserving comments and unmanaged keys
(extra_headers, always_thinking, session_header, passthrough, accounts not being edited),
pre-write `config.Load` validation (rejections answer 400 and leave the file byte-identical),
atomic rename, then the SIGHUP-equivalent Load+Reload — clicking Save hot-reloads the live
gateway and every open dashboard (SSE `config` topic). Secrets stay in the file: the UI
never receives key material and an empty api_key field on update preserves the existing
key per account name. Verification: new endpoint tests (add/update/secret-preserve/
reject-untouched/reload-observed) mutation-checked; headless-Chrome E2E drove login →
add-provider modal → save → auto-reload → edit prefill → combo modal on a scratch
instance; RSS bench parity with HEAD (borderline 100 MiB contract is pre-existing, not a
regression); deploy via scripts/deploy.sh zero-drop.
**2026-09-09 — commandcode + tokenrouter providers added live (config-only, hot-reload):** two user-provided
upstream keys wired into live onegw.toml, PUT /admin/config/reload (providers 6→8, zero downtime), live-verified:
`commandcode/deepseek/deepseek-v4-flash` and `commandcode/z-ai/glm-5.3-flash` 200 through the gateway.
**commandcode** (kind=openai, `https://api.commandcode.ai/provider/v1`) — user's GOAT plan ($10/mo, $70 credits);
48-model roster from the GOAT plan page (Gemini 3.1/3.5/3.6 + GPT-5.3/5.4/5.5 are Pro-gated with 403
MODEL_NOT_IN_PLAN; Claude ids serve Anthropic-only on /v1/messages — 400 on /chat/completions, probe-verified);
GLM-5.3 ids probed: accept `reasoning_effort medium`, reject `none` with 400 (low|medium|high|xhigh|max ladder),
ignore `thinking:{type:disabled}` → always_thinking = ["zai-org/GLM-5.3", "z-ai/glm-5.3-flash"]. Note: the legacy
`commandcode` KIND (NDJSON, #12) is unrelated — the new Provider API is standard OpenAI-compatible, so kind=openai.
**tokenrouter** (kind=openai, `https://api.tokenrouter.com/v1`, new-api aggregator) — 80 openai-endpoint-type text
models curated from /v1/models (openai-response-only ids like gpt-5.5/5.6/6-astra, Claude Anthropic-only,
gemini-only, image/video/embedding entries excluded); GLM-5.3 family speculative always_thinking; ACCOUNT BALANCE
$0.00 — all requests 403 insufficient_user_quota until topped up (live-probed; key authenticates, quota is empty).
Earlier:
research-delivered + step-1-shipped — follow-ups opened: #54 task-aware combo reordering (#44 step 2), #55
omp+onegw VPS deploy; open-work table synced with struck rows; #50 cross-format always-thinking
coercion landed 8b47d9d: coerceAlwaysThinkingUnified coerces the client-set ReasoningEffort and drops the
Anthropic thinking budget on prepareUpstreamBody's cross-format branches before encodeFor (the same-format
raw-body path was already covered — the unified decode/encode path was the last leak); regression test pins the
Responses wire (effort ladder, budget drop, no invention, passthrough), mutation-checked; isolation build from
git archive (tree carried untracked peer WIP in admin_config_edit.go), zero-drop deployed pid 74239 from the
archive binary, live smoke: Anthropic thinking:enabled → dev combo → glm-5.3-flash 200. earlier: #52 follow-up 51ccf1e: NormalizeInStreamError now also rewrites the
in-stream variant of the distributor parse-reject 400 (84fd1c9 shape) to retryable
upstream_parse_rejected — the helper had shipped claiming "the same transient-fault rewrites"
while only carrying auth-verify, leaving streaming paths surfacing that fault as a terminal
400; mutation-checked, isolation green, zero-drop redeployed pid 53121, live smoke verified;
smaller #52 follow-up 0a76838: the same transient-fault downgrades now also apply to IN-STREAM error objects — one-api proxies can deliver the auth-verify 401 as an error object inside a 200 body or a mid-stream chunk, and those paths built APIErrors directly via statusFromOAErr, bypassing the HTTP-level rewrite; translat.NormalizeInStreamError applies the identical 502 upstream_auth_verify_failed downgrade at all three in-stream construction sites (DecodeOpenAIResponse, decodeOpenAIStreamEvent, grok response.failed), real in-band invalid-key 401s stay terminal; test mutation-checked, isolation build green, zero-drop redeployed pid 36916 + live smoke verified; earlier: RCA + fix: third b-ai transient-fault class — distributor node parse-rejects of large
valid bodies no longer surface as terminal 400s (84fd1c9, zero-drop deployed, live-verified). Client-side omp dumps
(~/.omp/logs/http-400-requests) showed 22 "400 Invalid request body. (request id: …c955d568…)" (type=api_error)
in ~40 h, all combo free/dev to b-ai/glm-5.3-flash, bodies 228 KB-2.3 MB; forensics: every failing request id
carries the same backend-node marker, while byte-identical replays of three of those exact bodies served 200
through the gateway minutes later and direct-to-Zhipu replays flip 200/400/429 across runs — the distributor fans
requests to heterogeneous GLM nodes and one node's parse edge rejects large bodies. Router treated the 400 as
terminal (not Retryable/Fallbackable) so combo fall-through to qwen3.8-flash never ran. Fix mirrors the #52
auth-verify precedent: translat.UpstreamParseRejected (narrow: 400 + api_error + "Invalid request body" +
"request id:" trailer; genuine schema 400s keep failing fast, test-pinned) rewrites to retryable 502
upstream_parse_rejected with the upstream diagnostic preserved, no account bench — Router retries the target
(fresh node may serve) and combos fall through; client sees 200 or a retryable 502, never the lying 400.
Tests mutation-checked; full suite green on the merged tree; earlier: RCA + fix #52: b-ai transient faults no longer surface as terminal
client errors — 9fb6e69, zero-drop deployed, live-verified. Live dashboard showed two raw 429s
("model Concurrency limit 1200" — Tencent GLM's model-WIDE limit shared across all of the
reseller's traffic, not per-key) and a terminal 401 whose body was the upstream's own internal
auth/verify service failing (鉴权服务请求失败: read tcp ... connection reset). Fixes:
(1) types.APIError.SharedConcurrency signature (narrow "concurrency limit" phrasing) — those
429s skip the per-account ladder (benching healthy keys only shrank the serving pool while the
~2-5s upstream window self-cleared; live evidence: the window 429ed two keys, every other
request succeeded); (2) translat.UpstreamAuthVerifyFailed — the proxied verify-outage 401 is
rewritten to retryable 502 upstream_auth_verify_failed keeping the upstream diagnostic, real
invalid-key 401s stay terminal (test-pinned); (3) streaming fast path: whole-body requests
replay through the buffered pipeline (full retry + combo fall-through), unreplayable chunked
bodies get a managed retryable answer with honest Retry-After (a replay would be truncated —
middle bytes consumed by the transport); (4) Router.Execute 1s/2s backoff tiers for shared
concurrency (rotating keys is pointless against a shared wall) + surfaced OverQuota errors
carry Retry-After (2s shared / 10s default; upstream header wins verbatim); (5) coolDuration("")
flat-30s bug fixed — empty/garbage Retry-After now engages the documented adaptive ladder
(10s base doubling to 60s), which previously never ran for headerless HTTP 429s. All regression
tests mutation-checked; isolation build of the committed tree green; issue #52 opened + closed;
earlier: three-lane parallel implementation wave landed, merged with the
concurrent 502-storm RCA work (3e3e87b) — all attribution split by hunk in the merge commit
7e28aa0; full suite green on the merged tree before push: #48 b-ai premium-gating 403
(403 + access_denied/"Deposit required" signature) benches the ACCOUNT on the adaptive 429
ladder and falls through instead of surfacing (242f303); #36 session-affinity headers
forwarded verbatim + opt-in per-provider derived id (242f303); #31 cache-inclusive usage
convention with direction-pinned tests + #33 DeepSeek/Responses/Kimi cache shapes (f03dfcc);
#44 step 1 config-only tiny/planning combo examples (e797986); earlier:
RCA + fix: b-ai/glm-5.3-flash 502 storms — fixed 60s pre-first-byte
budget aborted massive thinking-model prefills (`http2: timeout awaiting response headers`);
`[server] response_header_timeout` knob (live: 120s) + transport-error classification
(504 upstream_timeout, retryable so combos fall through; client-hangup detection via request
ctx state — Go's header-timeout error also aliases context.DeadlineExceeded, probe-verified
h1+h2, Go 1.25); zero-drop deployed live, failures now 504-classified and fall through; branch
fix/upstream-header-timeout)*
`search` provider + `search-or-llm` combo dropped from live onegw.toml, gateway rebuilt from HEAD and zero-drop restarted;
earlier: docs: open-work table synced with GitHub — #2-#12 struck done,
close dates from the issue tracker, #17 verified landed via 7e935f2; earlier: issue #44
research published cleanly to the
issue — the body had been stored double-JSON-encoded and rendered on GitHub
as a raw JSON blob; reviewed and re-verified against sources (LiteLLM current
docs, 9router PR #2045 still unmerged, OmniRoute taskAwareRouting.ts
constants), dangling "implementation candidate below" reference fixed; earlier:
adaptive 429 cooldown ladder — b.ai's one-api
style upstream 429s per-key with an empty body and no Retry-After; live
metrics showed 65% of b-ai attempts failing (666×429 vs 352×200) because
plain round-robin re-picked spent keys every request. Shipped e820571,
live-verified: per-account adaptive cooldown (10 s base, doubling per
consecutive 429, 60 s cap, reset on success; upstream Retry-After wins
verbatim), `NextAccount` returns (nil, soonest-ready) when the whole pool
is cooling so Router/stream-fast-path/passthrough fall through to the next
combo target or answer 429 + Retry-After without a doomed upstream call;
mutation-checked tests at pool/router/Do levels. Follow-up 0bbe24f: the
fast-fail broke quota semantics (Execute picks the account before
attempt()'s quota gate, so a quota-cooled pool answered 429 instead of
503) — Router.PoolEmptyError is now pluggable and the server overrides it
with the quota check: exhausted windows answer the same 503
provider_quota_exhausted, genuine 429-limits keep the fast-fail. Caught
in the post-commit audit (3 quota tests red in-tree and on master at
e820571), fixed, full suite green; live post-deploy: ~70% success
sustained (130×200 / 56×429 vs 352×200 / 666×429 pre-fix), client-visible
429s now carry Retry-After; earlier: auto-update: `onegw update`/`version` commands, background checks, zero-drop self-handoff + rollback, container check-and-guide mode, CI version stamping; dashboard shipped (#41/#45/#19): full 9-page admin
console live — Go html/template + vendored htmx + uPlot (zero external
assets), cookie-session login, bounded SSE live events, #19 request-log ring,
grouped read-only /admin/api/v1, 7 agent-CLI preset cards, daily rollup
retention finally wired; unit-loss compact bug fixed (1.0B → "1" → "1B") and
today view charts hourly; README dashboard section with screenshots; RSS
bench pre/post ≈ 88/89 MiB peak; live-verified in-browser; earlier: SearXNG search disabled live 2026-09-08 (user request) — container + image removed, live onegw.toml no longer carries the `search` provider / `search-or-llm` combo; the repo keeps the feature (kind = "searxng" provider, profile-gated compose service, docker/searxng/ settings) for re-enablement; #46/#47 had enabled it live earlier (live-verified OpenAI/Anthropic/SSE surfaces + fall-through with the instance down); b-ai gated-account 403s found live → #48;
earlier: #42 ownership model landed: `internal/owner`
stamps `<data_dir>/owner.json` at startup and re-stamps on every successful
reload; `/admin/health` reports pid/listen/start/config mtime/argv/build
revision; README "Operations" section added; earlier: #13 docs sync: SearXNG web-search provider — shipped
2026-09-08 in 9bd3594 as `kind = "searxng"` virtual provider answering
`search/query` with a SearXNG JSON search as a synthetic OpenAI completion,
fail-open in combos; PRD gap list + issue table marked done, feature bullet
added, tracker ticked, README how-to filled; earlier: dashboard build-approach
deep dive (#45): stack
pinned — Go html/template + htmx + uPlot over a React bundle, stdlib SSE with
bounded fan-out, cookie-session login as the SSE auth prerequisite,
cursor-paginated grouped /admin/api/v1; new
[Dashboard build approach](#dashboard-build-approach-issues-4145) section +
docs/dashboard-deep-dive.md; earlier: model-tiering research (#44) surveyed
LiteLLM/9router/OmniRoute/omp and landed a layered adoption plan — config-only
role combos now, task-aware combo reordering as the feature; earlier:
always-thinking self-healing shipped: omp sent
`reasoning_effort: "xhigh"` to combo `free` and the raw GLM 400
(该模型始终思考，use low/high/max) surfaced to the client — two gaps:
`coerceEffort` passed unrecognized values through, and a 400 is
non-retryable so `Execute` never fell through the combo chain. Fixed:
xhigh→max + unknown→high coercion; `attempt()` now detects the signature
400 (code 1210 / 始终思考 message shapes), learns the model as
always-thinking on its `Def` (fresh on reload), marks the error
`Fallbackable` and `Execute` retries the target once with the coerced body
then falls through to the next combo target; the streaming fast path
learns too and routes future requests buffered. Mutation-checked; live
verified with a replay of the failing xhigh request. Earlier: orcarouter.ai
provider added live: `config.Validate`
— the executor layer shipped but config Load rejected them, a boot-crash-loop
trap (#39 pattern); four free-tier orcarouter models configured
(`z-ai/glm-5.3-flash-free`, `deepseek/deepseek-v4-flash-free`,
`tencent/hy3-free`, pooled `orcarouter/free` — free quota already exhausted
upstream) and the three concrete ones verified end-to-end through the gateway
streaming + buffered; `free` combo gained the glm + deepseek orcarouter
fallbacks; filed #41 — dashboard/admin-API revamp umbrella: 9router IA researched from source
(sidebar: Endpoint & Key / Providers / Combos / Usage / Quota / Token Saver / CLI Tools / Console Log),
LiteLLM Admin UI feature set, and frontend skills identified (anthropics/skills frontend-design +
web-artifacts-builder — single-bundled-HTML path preserves the no-external-assets constraint); scope pinned
read-mostly per the runtime-dashboard-config non-goal; earlier: grok via OpenAI Responses API: `FmtResponses`
item-id tool correlation + terminal-event guard — a Responses stream that
closes without `response.completed` is an error, not a clean finish);
`kind = "opencode"` routes per model — gpt-*/grok-*/muse-spark-* to
`/v1/responses`, the rest to `/v1/chat/completions` (probed live: grok
chat path 503s "Endpoint is unavailable"); default Go catalog refreshed to
the live 35-model listing; central fix: `attempt` now implements the
documented buffered non-streaming cross-format path (it previously emitted
a degenerate empty stream for every cross-format `stream:false` request)
and `EncodeOpenAIResponse` no longer drops text content; earlier:
prompt-caching research across eight provider
families — new [Prompt caching (upstream)](#prompt-caching-upstream) section
with the per-provider matrix; filed #31-#36. Verdict: the same-format path is
already cache-friendly — its three body mutations are deterministic and
idempotent, so the prefix is byte-stable across turns and client cache knobs
reach upstream untouched — while cross-format translation drops them outright;
earlier: dashboard token units: compact formatter gained
a B tier — sums ≥ 1e9 now render `1B`, not `1002.2M`; trailing `.0` trimmed
across K/M/B; earlier: opencode provider kind: OpenCode Zen Go
subscription integration — `kind = "opencode"`, default base
`https://opencode.ai/zen/go`, full Go catalog advertised by default
(muse-spark excluded, Responses-API-only), bearer auth, mandatory
`x-opencode-session` upstream (client header forwarded when present, else
stable per-key derived id), multi-key `keys = [...]` account shortcut for
round-robin + quota cooldown; earlier: dashboard usage range filter — today
/ 7 days / 1 month / all time, default all time, selection persisted in
`?range=` so a
reload keeps it; header totals now include sum (in+out) tokens; earlier:
saver defects fixed: ApplyRaw's re-encode HTML-escaped <,>,& across untouched
strings — tag-heavy bodies grew up to +87% (9511→17797 B) while /admin/usage
reported savings (now SetEscapeHTML(false), whole-body never-grow gate,
wire-delta accounting — NOTE: the saved column now reports the true wire
reduction, so pre-fix historic totals are not comparable); long-line byte-cut
split multi-byte runes into U+FFFD mojibake (now rune-boundary backoff); both
pinned by tests verified via mutation checking; earlier:
internal/saver/loss_profile_test.go — pins the RTK trade with runtime
evidence: generic path never truncates distinct lines, dedup-before-truncate
keeps mid-file errors in identical-run logs, truncating filters label the
elided middle with counts, long-line cuts carry byte markers, pretty JSON
sniffs onto the truncating path, idempotence + never-grow enforced per
filter; earlier: security/stability hardening pass: constant-time
key + admin-password comparison, admin auth moved to the X-Admin-Password
header only (query strings leak into logs), fail-closed startup/reload on
non-loopback binds with no auth keys, 30s shutdown drain deadline,
upstream ResponseHeaderTimeout/TLSHandshakeTimeout so a stalled upstream
cannot leak byte-budget reservations, sniffer scan limit now actually
enforced, panics logged with stack traces; earlier: filed #19 — dashboard
console log viewer (9router-style): in-memory ring-buffer log sink,
password-gated /admin/logs endpoints, dashboard console pane; earlier:
/admin/health is now password-gated like /admin/usage — it carries the live
in-flight gauge; dashboard passes the password and scripts updated; earlier:
dashboard shows live in-flight request concurrency; codified ordered design
priorities: fast > security > massive sessions > token saving > lowest RAM;
imported kilocode + xai bearer-token providers from 9router; custom
wire-format gaps tracked as #12; easier-setup roadmap — auto-release CI,
one-command install, one-click agent-CLI install, Docker deploy — tracked as
#14; always-thinking effort coercion for glm-5.3/glm-5.3-flash — 400 on
streaming requests, root cause and fix in #16)*

## Product

`onegw` — single-binary LLM gateway in Go, a resource-efficient alternative to
[9router](https://github.com/decolua/9router) (JS) and
[LiteLLM](https://github.com/BerriAI/litellm) (Python). One process fronts
OpenAI-, Anthropic-, and Gemini-compatible providers behind those same three
HTTP surfaces; routes by `provider/model`, applies fallback chains
("combos"), round-robins accounts, and tracks usage/quota. Hard targets:

- **RAM: ≤ 100 MB resident** under sustained load (default Go GC tuning; no
  per-request buffering of streams).
- **Throughput: 1–2 B tokens/day** (~12–23k tok/s sustained; bursts far higher
  because streaming is I/O-bound passthrough).
- **Sessions: millions of concurrent** — sessions are pass-through by design;
  no conversation state is held in memory.

### Design priorities (ordered — tie-break rubric for every design decision)

1. **Fast** — streaming passthrough, zero frameworks, translation is the
   only O(body) work and only on cross-format requests.
2. **Security** — bearer-key auth at the edge (constant-time compares),
   keys only in gitignored TOML, client `provider/model` strings rewritten
   so they never leak upstream, localhost bind by default, fail-closed on
   non-loopback binds without keys, header-only admin auth; per-key
   policies (#3) extend this.
3. **Long-running massive sessions** — stateless by design; no conversation
   cache; nothing accumulates per session, so uptime is unbounded and
   session count is irrelevant to memory.
4. **Save tokens** — input-side `tool_result` compression (saver); output
   side tracked as #5.
5. **Lowest RAM usage** — the 100 MB contract below; byte-budget
   backpressure; SQLite measured, not assumed.

### Non-goals

- OAuth device flows for the remaining subscription providers (Claude Code,
  Codex, GitHub Copilot, Cursor, Antigravity) — the framework landed with #2
  (RFC 8628 + Kilo dialect, token store, auto-refresh, `onegw-oauth` CLI);
  each remaining provider is a new entry in the provider registry plus its
  dialect quirks.
- Cloud sync, browser extension, electron tray.
- Billing / spend enforcement. Usage tracking is informational (cost estimates
  only).
- Response/semantic caching — storing an answer in the gateway and replaying
  it instead of calling the upstream — plus guardrails framework, MCP/A2A
  gateways, realtime/audio endpoints: out of the minimal-gateway scope,
  revisit only on demand. Distinct from **upstream prompt caching**, which
  onegw does not implement but must not break: see
  [Prompt caching (upstream)](#prompt-caching-upstream).
- Response/semantic caching, guardrails framework, MCP/A2A gateways,
  realtime/audio endpoints — out of the minimal-gateway scope; revisit only
  on demand. (Narrow STT/TTS/embeddings passthrough with no translation
  landed via #9; full audio sessions remain a non-goal.)

## Architecture (HLD)

```
CLI tools (Claude Code, Codex, Cursor, ...)          40+ upstream providers
        │                                                    ▲
        ▼                                                    │ SSE / chunked
  onegw :8080  ── openai │ anthropic │ gemini ── surfaces  ──┘
        │
   router:  model → adapter → account pool → retry/fallback chain
        │
   middleware: auth → token-saver → usage counters → translation → upstream
```

Single Go binary. Zero framework (net/http + httputil.ReverseProxy for
same-format passthrough). Packages:

| Package    | Responsibility                                                        |
| `types`    | Unified internal request/response model + wire formats                |
| `translat` | Bidirectional OpenAI ↔ Anthropic ↔ Gemini translation (body + SSE), plus the OpenAI Responses-API upstream wire (opencode gpt/grok/muse-spark) |
| `provider` | Provider interface, registry, per-provider HTTP client, adapters      |
| `router`   | Model resolution, combo fallback chains, per-account round-robin      |
| `saver`    | RTK-style tool_result compression filters (prefix sniffing, idempotent, loss profile test-pinned)|
| `usage`    | Lock-sharded atomic counters, batched periodic flush to SQLite        |
| `store`    | SQLite (usage rollups only — config lives in TOML)                    |
| `oauth`    | Device-flow logins (#2): RFC 8628 + Kilo dialects, token store, auto-refresh |
| `server`   | HTTP surfaces, `/v1/*`, `/v1beta/*`, `/anthropic/*`, admin, dashboard |
| `auth`     | Bearer-key auth, per-key model restrictions (planned, #3)             |

### Memory strategy (the 100 MB contract)

- **Streams are never buffered.** Same-format requests: byte passthrough
  (`httputil.ReverseProxy` semantics, flush-per-chunk). Cross-format: event-wise
  SSE re-encoders with a small bounded window, `http.Flusher` per event.
- Request/response bodies are parsed with `json.Decoder` streaming where
  practical; tool_result compression mutates only bounded pieces.
- Usage counters: fixed-size lock-sharded atomics (no maps per request); flush
  every N seconds to SQLite in one transaction.
- **Global in-flight byte bound.** Per-request body caps do not bound total
  RSS: the non-streaming cross-format path (parse whole request, parse whole
  response) holds O(body) bytes per request. A global semaphore sized in bytes
  (default 48 MiB) gates that path; streaming/passthrough paths never acquire
  it. Under saturation requests wait, memory stays flat.
- Sessions are **stateless**: no conversation cache; nothing accumulates per
  session, so massive session counts cost nothing. Note: `session_id` keys
  nothing today either — `ChatRequest.SessionID` is parsed from `user` /
  `metadata.user_id` (`openai.go:192`, `anthropic.go:123`) but read nowhere,
  and rollups key on day/hour/provider/model/api_key. Header affinity landed (#36, 242f303:
  x-grok-conv-id/x-grok-session-id/x-session-id/session_id forwarded verbatim, per-provider opt-in derived
  id); cache-affinity breakpoint/key-forwarding remains open work (#34).
- `GOGC=60` (set at startup if `GOGC` env unset); soft memory limit
  `GOMEMLIMIT=90MiB` set at startup if unset. Allocation-heavy JSON reuse in
  hot loops.
- **`sys` is a lifetime ratchet, not a leak (RCA 2026-09-08).** The dashboard's
  `sys` = Go `memstats.Sys`: cumulative arena reservations. Under repeated
  agent-class load (22 concurrent × 1.5 MB streaming bodies) a test instance
  plateaued at 41 MiB within one wave and held through 4 waves + idle; the live
  instance held 74 MiB flat for 6+ minutes while inflight went 6→22 and
  heap alloc 14→27 MiB. Growth reflects the largest concurrent burst seen
  (arena high-water), and Go returns freed arenas lazily (MADV_FREE on macOS
  also keeps RSS high). Usage aggregation is SQL-side (`GROUP BY` in SQLite);
  no unbounded in-memory maps. If the plateau exceeds the envelope, cap
  concurrent buffered-path bytes harder — no code fix indicated.
- **SQLite RSS is measured, not assumed.** `modernc.org/sqlite` is a generated
  C→Go port with nontrivial heap. M5 includes a benchmark asserting its
  resident cost under the flush workload; if it exceeds the budget the store
  falls back to append-only JSONL rollups with periodic compaction.
- SQLite is the only disk I/O besides logs; WAL mode, single writer goroutine.

### Throughput math

1–2 B tok/day ≈ 12–23 k tok/s avg. Typical SSE chunk ≈ 4–32 KB carrying tens of
tokens; per-chunk cost is a `bufio` copy + occasional JSON event rewrite —
Go http server handles this on ~1 core. Translation paths are the only O(body)
work and only run on cross-format requests; they stream event-by-event so
memory is O(event), not O(conversation).

### Model tiering / task-aware routing (issue #44)

Research 2026-09-08: LiteLLM docs, 9router README + source (0.5.70),
OmniRoute source (`open-sse/services/taskAwareRouting.ts`), omp harness docs
(`omp://models.md`). Question: cheap model for tiny tasks, strong model for
planning/brainstorming. Deliverable lives in
[issue #44](https://github.com/FreePeak/onegw/issues/44); re-verified on the
2026-09-08 reformat — LiteLLM claims match current docs, 9router PR #2045
still unmerged, OmniRoute numbers match `taskAwareRouting.ts` line-for-line
(the issue body had been stored double-JSON-encoded and rendered as a blob).

| | **LiteLLM** | **9router** | **OmniRoute** | **omp (client)** |
| --- | --- | --- | --- | --- |
| Tier selection | client-directed (caller picks the model group) | manual 3-tier combo convention (Subscription → Cheap → Free) | **gateway-side, per request** | client-side model roles |
| Difficulty signal | none — routing strategies are cost/latency/rpm, not content | none in 0.5.70 (task-aware is open PR #2045, unmerged) | `classifyTask`: light/standard/heavy/critical from prompt size, message/tool count, `max_tokens`, `reasoning_effort`, keyword regexes — **no LLM call** | harness knows the task type (tool role: titles vs planning vs scout) |
| Mechanism | model groups + fallbacks; `context_window_fallbacks` = failover to another group on context overflow | ordered combo fallback across tiers | `modelPowerScore` 0–150 per model; `reorderByTaskWeight` = stable re-sort of the combo's target list per request (never removes targets) | roles `smol`/`tiny` (background tasks), `slow`/`plan` (planning), `task`, `commit`; `compactionModel` (cheap summarizer); `contextPromotionTarget` (small→large context chain) |

**What exists where:** OmniRoute is the only gateway that ships the full
feature (ported from 9router PR #2045): classify cheaply locally, score each
combo target (`100 − |power − target|` with hard-miss penalties: vision
missing −10000, non-reasoning model on heavy task −120, prompt > 85% context
−200), reorder, keep full fallback. It also maps task types to auto-combo
intents (`auto/coding`, `auto/chat:cheap`) via an LLM intent classifier — the
part worth *not* copying. omp proves the complementary client-side shape:
named roles wired per use, not per request.

**Adoption plan (layered):**

1. **Now, config-only** — define cheap `tiny` and strong `planning` combos in
   `onegw.toml`; point omp's `smol`/`tiny` and `default`/`plan` roles at them
   in `models.yml`. Zero gateway code; the client knows the task type.
2. **Feature: task-aware combo reordering** — OmniRoute-style, stateless:
   classify (few regexes + integer score, no LLM, no per-request state),
   stable-sort the resolved combo targets inside `router.Execute` before
   account selection, keep the full chain as fallback. Config switch
   `task_routing = off` by default; decisions logged via `/admin/logs` (#19).
   Fits the priority rubric: fast (O(1) bookkeeping), massive sessions
   (nothing held per session), token saving (the point of the feature).
3. **Deferred** — context-overflow → bigger-context tier escalation
   (LiteLLM `context_window_fallbacks` / omp `contextPromotionTarget`
   analog); rides the existing error-class retry path if production hits it.

**Rejected:** LLM-based intent classification (OmniRoute's semantic task
types) — an LLM call to pick a model contradicts fast/low-RAM; local signals
are what the merged-quality implementations use.

### Dashboard build approach (issues #41/#45)

Research 2026-09-08 (full write-up:
[docs/dashboard-deep-dive.md](dashboard-deep-dive.md), issue #45): how to
build the #41 revamp.

- **Stack: Go `html/template` + htmx + uPlot** — server-rendered pages,
  htmx partial updates (~14 KB min.gz), uPlot Canvas-2D charts (~48 KB min
  vs Chart.js 254 KB / ECharts 1 MB), all vendored inline: zero external
  assets, zero Node toolchain. A bundled React+Tailwind single-HTML artifact
  (#41's original sketch) is the documented fallback if a page ever needs
  real client-side state — the read-mostly IA doesn't.
- **Live data: one stdlib SSE endpoint** `GET /admin/events?topics=…`
  (`http.Flusher`) with bounded fan-out (≤ 32 subscribers, ~8 KB ring each;
  slow subscriber → events dropped + `resync` refetch event, mirroring the
  byte-budget philosophy). Topics: health 1 s, usage 5 s, logs (#19 sink
  push), quota on change. Pages render fully server-side without JS.
- **Auth prerequisite**: `EventSource` cannot send headers → the planned
  session-cookie login (`POST /admin/login`, HttpOnly SameSite=Strict
  cookie, header callers keep working) lands before any SSE page.
- **API**: grouped `/admin/api/v1/` (usage with **cursor** pagination
  shared with export; read-only providers/combos/quota/saver; uniform JSON
  errors); flat `/admin/*` endpoints stay for scripts/smoke.sh, deprecated
  later, never broken in place.
- **RAM**: read-only embedded bytes + one small alloc per template render +
  bounded SSE fan-out — inside the 100 MB contract; verified with
  `bench/memory.sh` before/after each slice.
- **Landing order**: cookie login → shell (`internal/server/dashboard/`,
  `go:embed` FS replacing the `dashboardHTML` const, #41 sidebar) →
  Overview + health topic → Usage analytics + uPlot → Logs pane (#19) →
  read-only config views + CLI Tools cards → grouped API completion.

### Prompt caching (upstream)

Research 2026-09-07/08: official vendor docs, live probes against the
configured endpoints, and 9router source. Prompt caching is **upstream-side** —
onegw stores no response (that stays a non-goal). What onegw controls is
whether the request bytes and identity fields upstream caches key on survive
the trip.

**Verdict.** Every configured upstream is `kind = "openai"` (`b-ai`, `glm`,
`kilocode`, `xai`) plus `opencode`. On that path the body is mutated three
times — `saver.ApplyRaw` (`server.go:272`), `adaptAlwaysThinking` (`:588`),
`normalizeRoles` (`:589`) — but all three are deterministic and idempotent
(pinned by `saver/loss_profile_test.go:222-243`), so the prefix stays
byte-identical across turns and client-sent `prompt_cache_key` / `session_id` /
`cache_control` reach upstream untouched (`rewriteModel` rewrites only
`model`). **The entire cache-loss surface is cross-format translation**, where
typed structs drop those fields.

| Family | Mechanism | OpenAI-wire knob | Min tok | Write / read |
| --- | --- | --- | --- | --- |
| Anthropic | explicit breakpoints (+ top-level automatic) | own compat layer: unsupported | 512–4096 | +25%/+100% write; 10% read |
| OpenAI | implicit + explicit (GPT-5.6+) | `prompt_cache_key`, `prompt_cache_options` | 1024 / 2048 | 1.25x write (5.6+); 10% read |
| Gemini / Vertex | implicit auto, **no knob** + `cachedContents` resource | `extra_body.google.cached_content` | 2048–4096 implicit; explicit undocumented | none; 10% read (+storage) |
| xAI Grok | implicit + sticky routing | `prompt_cache_key`; header `x-grok-conv-id` | undocumented | none; 15–25% read |
| Zhipu GLM | implicit only | **none** — probe: all knobs 200-and-ignored | undocumented | none; ~19% read |
| DeepSeek | implicit only | **none** — `cache_control` documented "Ignored" | none documented | none; ~3% read |
| Qwen / DashScope | implicit + explicit | **`cache_control` works on the OpenAI wire** | 1024 | 125% write / 10% read explicit; 20% implicit |
| Kimi / Moonshot | implicit | `prompt_cache_key` | 256 | none; 10–20% read |
| Aggregators | upstream-dependent | OpenRouter `session_id` → 10m sticky (kilo mirrors it: inferred); b-ai ignores all | — | per upstream |

Undocumented cells are genuinely absent from vendor docs — no number is
substituted. Vendors **silently skip caching below the minimum and return no
error**, so savings are verified from reported usage, never assumed.

**Working today:** implicit prefix caching end-to-end on the live routes
(GLM probe: 1528/1529 tokens cached on prefix reuse; b-ai `qwen3.8-flash`:
0 → 1024 → 1664), and cache-read accounting on passthrough, where `sniff.go`
recognizes five of the six vendor usage shapes.

**Broken / open:**

- #31 — no cache-inclusive vs cache-exclusive convention, so every translated
  route mis-reports prompt size; over-reports on the live Anthropic-client ×
  OpenAI-upstream path, and `usage_rollup.input_tok` is not comparable across
  providers.
- #32 — cross-format requests drop `cache_control` / `prompt_cache_key` /
  `session_id`.
- #33 — DeepSeek `prompt_cache_hit_tokens` invisible to sniffer and decoders
  (latent: no direct DeepSeek provider configured today).
- #36 — `x-grok-conv-id` is never forwarded (`Do()` builds a fresh request),
  losing xAI's per-server sticky routing on the live `grok-4.6` route.
- #34 — no anchoring for the families where markers do work (DashScope on the
  OpenAI wire; Anthropic-native).
- #35 — `ApplyRaw`'s global all-or-nothing gate is the one non-monotonic
  mutation and can flip a whole body raw↔compressed, busting the prefix.

**Rules adopted:** never invent a knob for a provider that ignores it; anchor
**last**, after every body mutation (9router's ordering discipline); never
strip client markers on same-format passthrough; treat combo fallback hops as
cold-cache by construction and measure the cost on the existing `cache_read`
column rather than assuming it.

## Milestones

| M   | Scope                                                                 | Status |
| --- | --------------------------------------------------------------------- | ------ |
| M1  | Core types, OpenAI/Anthropic/Gemini translation, streaming            | done   |
| M2  | Router: combos, round-robin, retry/fallback                           | done   |
| M3  | Token saver filters, usage tracking, SQLite store                     | done   |
| M4  | Server surfaces (OpenAI/Anthropic/Gemini), auth, admin, dashboard     | done   |
| M5  | Benchmarks (RSS ≤ 100 MB @ load), smoke test, hardening               | done   |

All v1 milestones are complete and live-verified (B.AI traffic, pi CLI agent
loop, glm/openrouter routes). Post-v1 work is tracked as GitHub issues —
see [Open work](#open-work).

### Verified numbers (bench/memory.sh, mock upstream, macOS arm64)

- Baseline RSS 17 MiB; 30 concurrent 800 KB streaming requests → peak
  67 MiB, flat across rounds; 850 K tokens relayed in the 12 s window
  (~71 K tok/s, ~3× the 23 k tok/s needed for 2 B/day). Mock reports a
  synthetic 200 tokens/request, so the honest load proof is ~2 100 reqs ×
  800 KB ≈ 140 MB/s relayed at 67 MiB RSS.
- `GOMEMLIMIT=90 MiB` soft limit set at startup; `GODEBUG=madvdontneed=1`
  used in benchmarks because macOS MADV_FREE overstates RSS.
- Byte-budget gate: buffered path (body read + saver + translation) holds a
  4× body-size reservation against a 48 MiB global budget; saturation
  returns 503 + Retry-After.

## Competitive positioning

Compared against the two reference gateways ( LiteLLM README + docs,
9router README, as of 2026-09-07).

### Landscape

| | **LiteLLM** | **9router** | **onegw** |
| --- | --- | --- | --- |
| Runtime | Python + FastAPI; Postgres required for keys, Redis recommended in prod; benchmark spec 4 CPU / 8 GB per instance | Node.js 20 + Next.js 16; SQLite-backed config; no published RAM figures | Single static Go binary (`CGO_ENABLED=0`); optional SQLite for usage only; measured 17 MiB idle → 67 MiB peak |
| Scope | Platform gateway: SDK + proxy, 100+ providers, teams/budgets/billing, guardrails, MCP + A2A gateways | Localhost "free AI router & token saver": 40+ providers, subscription maximization, dashboard-first config | Minimal personal/team gateway: 3 wire surfaces, any-to-any translation, combos, token saver, usage rollups |
| Surfaces | OpenAI universal in; Anthropic `/v1/messages` in → any provider; Gemini pass-through only | OpenAI surface (+ Anthropic base URL); Cursor/Kiro/Vertex/Ollama/Responses translations | OpenAI, Anthropic, **and Gemini-native** surfaces in, any-to-any out, per-event SSE re-encoding |
| Fallback | Model-group fallback chains, latency/cost-based routing, session affinity | 3-tier smart fallback (subscription → cheap → free) + custom combos | Named combos (ordered retry chains), weighted round-robin account pools, quota cooldown |
| Config | `config.yaml` + DB overlay (`store_model_in_db`), UI writes models at runtime | Dashboard UI → SQLite; no file config | Single TOML file + env overrides; restart (hot reload: #1) |
| Auth | Virtual keys in Postgres, teams/roles, JWT, SSO (enterprise-gated) | Dashboard-generated keys; enforcement off by default | Static bearer-key list; per-key policy planned (#3) |
| Token saver | None in OSS | RTK (input) + Caveman/Ponytail (output prompt injection) + Headroom (external compress) | RTK-style input-side `tool_result` compression, bounded sniffer |
| Prompt caching | Response + semantic caching (in-memory/disk/Redis/S3/GCS; Qdrant/Redis/Valkey semantic) — needs an external store; prompt-caching passthrough for 7 provider families | Conversation-state cache (history-hash key, TTL + eviction, per executor) **and** active breakpoint engineering (`anchorClaudeCache`, `prompt_cache_key` injection) | Upstream cache **accounting** only; no gateway-side cache by design; preservation/anchoring gaps tracked as #31-#36 |
| Observability | Prometheus `/metrics`, Langfuse/LangSmith/etc callbacks, spend logs | Dashboard quota/analytics + reset countdowns | `/admin/health`, `/admin/usage`, embedded dashboard; Prometheus planned (#4) |
| Backpressure | Docs admit high init/per-request memory growth | Not published | Byte-budget semaphore: 48 MiB global bound, `503 + Retry-After` |

### Where onegw wins

- **Resource envelope**: measured 17 → 67 MiB RSS with no external
  Postgres/Redis; LiteLLM's own docs specify 4 CPU / 8 GB instances and
  admit "high memory usage during initialization and per request"; 9router
  publishes no memory figures and needs Node + PM2/Docker.
- **Gemini as a first-class translated surface** (LiteLLM: pass-through
  only; 9router: no Gemini-native client surface).
- **Memory contract with enforcement** — byte-budget backpressure instead of
  unbounded per-request growth.
- **Infra-as-file config** (TOML, reviewable, no DB overlay semantics).

### Gaps accepted or deferred (issue-tracked)

- OAuth subscription providers + token refresh — 9router's core
  "maximize-the-subscription" pitch; deferred from v1 → #2.
- Output-side token savers (Caveman/Ponytail/Headroom analogues) → #5.
- Quota reset-window tracking + per-provider spending limits → #7.
- ~~Model aliases → #6~~ (done 2026-09-08); per-key rate
  limits/restrictions → #3; Prometheus → #4; audio/embeddings surfaces → #9;
  streaming request bodies → #8; multi-node rollup export → #10; runtime
  config writes → #11; ~~web-search provider → #13~~ (done 2026-09-08).
- Install/ops friction: no prebuilt releases, manual build, manual
  agent-CLI wiring, no Docker image → #14.
- Not pursued (non-goals): cloud sync (9router-only), billing/budget
  enforcement, gateway-side response/semantic caching, guardrails/MCP/A2A,
  runtime dashboard config as the primary path (config stays file-based; #11
  is optional convenience), advanced LB strategies beyond round-robin + combos
  (latency/cost-based routing — LiteLLM platform features, not
  minimal-gateway scope). Cache-affinity *identity* forwarding (#34, #36) is a
  distinct, stateless concern and is pursued.

## Routing model

- Model string forms: `provider/model` (direct), `combo-name` (ordered
  fallback list).
- Combo: `[{provider, model}]`, tried in order; retry on 429/5xx/network
  error with backoff; non-retryable errors (4xx) fail fast; per-provider
  concurrency caps; weighted round-robin account pools with quota cooldown.
- Account = API key + optional base URL override + weight.
- Provider kinds: `openai`, `anthropic`, `gemini` — arbitrary upstreams via
  `base_url` override (40+ providers reachable: OpenRouter, GLM, Kimi,
  DeepSeek, Groq, ...). B.AI, GLM, and OpenRouter routes live-verified.

## Key decisions

- **net/http only, no web framework** — fewer deps, predictable memory.
- **modernc.org/sqlite** (pure Go, no cgo) — cross-compile single binary;
  for one-writer usage rollups; store RSS measured inside the budget in the
  bench (heap_sys 55 MiB peak with translation+store+streams, i.e. SQLite
  not the driver). `CGO_ENABLED=0`.
- **Translation via unified intermediate model** — correctness over
  cleverness; streaming re-encoders are separate from body translation.
  N+M decoders/encoders instead of N×M pairwise adapters.
- **Passthrough-first**: same-format traffic is byte-copied with a bounded
  usage sniffer (64 KiB rolling window); never parsed. Upstream model is
  rewritten into the passthrough body so client-side `provider/model`
  strings don't leak upstream.
- **Byte-budget semaphore (mutex + poll)**, not a token channel — a channel
  cannot express all-or-nothing multi-unit take without deadlock.
- **Config = single TOML file + env overrides**; usage state is the only
  SQLite content. SIGHUP reload: #1.
- **Migration tooling**: `cmd/import9r` imports 9router's data.sqlite
  (API-key connections → accounts/keys/combos TOML), lowering the switch
  cost from 9router.
- **Never invent a cache knob.** Providers that ignore a field (GLM, DeepSeek,
  b-ai — verified by live probe) must not receive it; adding bytes that buy
  nothing is worse than adding nothing. Anchoring is opt-in per provider and
  runs after every body mutation.
- **Public-repo update checks never require a GitHub token** (96c1fd3): the
  update client sends `ONEGW_GITHUB_TOKEN`/`GITHUB_TOKEN` when set, and on
  401 retries the same request once anonymously. FreePeak/onegw is public —
  anonymous reads answer — so a stale token in the environment (user shell,
  inherited child, poisoned restart spec) can never break release checks or
  binary downloads. Tokens remain for private forks: one valid-token request,
  and the error names "token rejected AND anonymous failed" when both paths
  die.

## Open work

All post-v1 tasks live as GitHub issues (https://github.com/FreePeak/onegw/issues):

| #  | Task                                                        | Source             |
| -- | ----------------------------------------------------------- | ------------------ |
| ~~#1~~ | ~~SIGHUP hot reload of config~~ — **done 2026-09-07**; config swaps as one atomic snapshot, bad file rejected, old usage tracker flushed | v2 tracker |
| ~~#2~~ | ~~OAuth device flows for subscription providers~~ — **done 2026-09-08** | 9router gap |
| ~~#3~~ | ~~Per-key rate limits and model restrictions~~ — **done 2026-09-07** | v2 tracker |
| ~~#4~~ | ~~Prometheus metrics endpoint~~ — **done 2026-09-07** | v2 tracker |
| ~~#5~~ | ~~Output-side token savers (prompt injection / compression)~~ — **done 2026-09-07** | 9router gap |
| ~~#6~~ | ~~Model aliases in config~~ — **done 2026-09-07** | 9router gap |
| ~~#7~~ | ~~Quota reset-window tracking and spending limits~~ — **done 2026-09-07** | v2 tracker |
| ~~#8~~ | ~~Streaming request bodies (client→upstream)~~ — **done 2026-09-07** | v2 tracker |
| ~~#9~~ | ~~Audio and embeddings surfaces (STT/TTS/embeddings)~~ — **done 2026-09-07** | 9router gap |
| ~~#10~~ | ~~Multi-node usage rollup export~~ — **done 2026-09-07** | v2 candidate |
| ~~#16~~ | ~~Always-thinking upstreams 400 on streaming medium/disable-thinking requests (glm-5.3 family)~~ — **done 2026-09-07**; per-provider `always_thinking` globs + same-format effort coercion/drop, knob documented in README + onegw.toml.example, regression tests (commit 40977cf) | production hit |
| ~~#11~~ | ~~Runtime config surface~~ — **done 2026-09-07** via #21 (masked view, no-shell reload, keys/aliases PATCH); dashboard stays read-mostly by design | LiteLLM gap |
| ~~#12~~ | ~~Custom wire formats: commandcode (NDJSON), grok-cli (Responses), cursor (protobuf)~~ — **done 2026-09-08** (cursor as skeleton, #29) | 9router gap |
| ~~#13~~ | ~~Web-search provider (SearXNG integration)~~ — **done 2026-09-08**; `kind = "searxng"` virtual search provider (9bd3594): `search/<x>` model requests answered with a SearXNG JSON search as a synthetic OpenAI completion, retryable-503 fail-open in combos, `max_results`/`timeout`/`extra_headers` knobs, unit + E2E tests on both client surfaces | 9router gap |
| #14 | Easier setup: auto-release CI, one-command install, one-click agent-CLI install, Docker deploy | user request |
| ~~#17~~ | ~~Self-healing thinking-dialect fallback~~ — **done 2026-09-08**; shipped as always-thinking self-healing (7e935f2): coerceEffort xhigh→max/unknown→high, signature-400 detection, learned per (provider, model) on the Def (fresh on reload), Fallbackable retry-once then combo fall-through, stream fast path learns; live-verified with the xhigh replay; issue closed with landed note | #16 follow-up |
| ~~#19~~ | ~~Dashboard console log~~ — **done 2026-09-08**; 512-entry in-memory ring fed from the same completion points as /metrics, `GET /admin/api/v1/logs?limit=N` + live SSE `logs` topic, console pane with colored status/token columns (a59c2a3); **extended 2026-09-09** (ab62468): every row carries the upstream account name (`@account` in the console line, `account` in the JSON), and entries older than 7 days are auto-cleared from the view (ring bounds memory, age window bounds staleness) | user request |
| ~~#31~~ | ~~Fix cache-inclusive/exclusive usage semantics across translation~~ — **done 2026-09-09** (f03dfcc): `Usage.InputTokens` = cache-INCLUSIVE total documented on the type; Anthropic decode folds read+write in, every Anthropic-format emitter denormalizes via `anthropicInputTokens` (clamped ≥ 0); sniffer normalizes Anthropic payloads at the same boundary; TotalTokenCount includes cache-write; 18 non-stream + 12 stream direction pairs pinned | research 2026-09-08 |
| #32 | Preserve `cache_control` / `prompt_cache_key` / `session_id` across translation | research 2026-09-08 |
| ~~#33~~ | ~~Parse missing vendor cache-usage shapes (DeepSeek hit tokens); pin with tests~~ — **done 2026-09-09** (f03dfcc): `prompt_cache_hit_tokens` in the sniffer's cache-read pattern; Responses `input_tokens_details` + Kimi top-level `cached_tokens` on the typed path; all six vendor shapes pinned through sniffer + typed decode (TestSniffVendorUsageShapes) | research 2026-09-08 |
| #34 | Per-provider cache profiles: breakpoint anchoring, anchor-last ordering | research 2026-09-08 |
| #35 | Saver's global gate can flip the request prefix and bust implicit caches | research 2026-09-08 |
| ~~#36~~ | ~~Forward `x-grok-conv-id` — live sticky-routing loss on xai~~ — **done 2026-09-09** (242f303): allow-list (x-grok-conv-id, x-grok-session-id, x-session-id, session_id) forwarded verbatim through Do/DoPassthrough for every provider; absent client values, stable per-key `ses_` id derived (generalized opencodeSession) only when the per-provider `session_header` knob opts in — nothing invented ungated | research 2026-09-08 |
| ~~#41~~ | ~~Dashboard revamp: 9router/LiteLLM-style multi-page admin UI + grouped admin API~~ — **done 2026-09-08**; full IA shipped (Overview/Usage/Providers/Combos/Quota/Saver/Logs/CLI Tools/Settings), variant-A stack, grouped `/admin/api/v1`, live-deployed + browser-verified + memory-benched (16f3dc9, a59c2a3); runtime dashboard *writes* stay #11 | user request |
| ~~#43~~ | ~~TestQuotaRebuildFromRollups red on master~~ — **done 2026-09-08**; not a regression but a midnight-UTC time-bomb in test seeding (00:00–01:00 UTC the −1h seed bucket crosses the daily window boundary); midday-anchored reference time, RCA comment + issue closed (1aa6a95) | #37/#39 follow-up |
| ~~#42~~ | ~~Ownership model for the live gateway + shared config~~ — **done 2026-09-08**; `internal/owner` stamps `<data_dir>/owner.json` at startup (pid, listen, start time, config path + mtime, argv, build stamp: module version/git revision/dirty flag) and re-stamps on every successful reload (SIGHUP or `PUT /admin/config/reload`); `/admin/health` reports the same record live; file is atomic and survives exit as crash evidence; reload-not-restart + deploy discipline in README "Operations"; optional younger-build start guard skipped — single-instance is operator discipline after the #38 revert | incident RCA |
| ~~#37~~ | ~~Zero-drop deploy runbook: start→verify→stop ordering~~ — **done 2026-09-08**; `scripts/deploy.sh` (build → overlap-bind → health-verify NEW → SIGTERM OLD → confirm single NEW listener; setsid isolates NEW from the deploying session's process group after the freeze RCA), 5 behavioral scenarios tested on scratch ports (d14441d, 9cabbf6) | incident RCA |
| ~~#38~~ | ~~Single-instance guard on data_dir~~ — **landed then deliberately reverted 2026-09-08**; heartbeat peer scan + `onegw_data_dir_peers` gauge shipped in cfce76f, reverted at user decision in 9049dd4 — single-instance stays an operator discipline, not a feature; issue stays closed | incident RCA |
| ~~#39~~ | ~~Keyless provider fails the whole boot~~ — **landed then deliberately reverted 2026-09-08**; loopback warn-and-skip shipped in 5d17c82, reverted at user decision in 9049dd4 — boot is strict `Validate` again (keyless provider fails any bind); issue stays closed | incident RCA |
| ~~#40~~ | ~~Buffered-path byte reservation leak across SIGHUP~~ — **done 2026-09-08**; leak fixed in dbe02bd, regression guard `TestRelayResponseBudgetSurvivesReloadMidAcquire` mutation-verified (fails at dbe02bd^) (e5ecff8) | incident RCA |
| ~~#44~~ | ~~Model tiering / task-aware routing research~~ — **done 2026-09-09**; research deliverable verified vs LiteLLM/9router/OmniRoute/omp (verdict: client-side roles now, gateway feature = task-aware combo reordering); step 1 config-only tiny/planning combos shipped (e797986); step 2 tracks as #54 | user request |
| ~~#45~~ | ~~Dashboard build approach for #41~~ — **done 2026-09-08**; decision held: Go html/template + vendored htmx 2.0.6 + uPlot 1.6.32, stdlib SSE with bounded fan-out, cookie sessions; health strip fixed to server-rendered HTML in fe2d532; RSS bench pre/post ≈ 88/89 MiB peak | #41 deep dive |
| #46 | Self-hosted SearXNG stack for the search provider (compose service + JSON-format settings) — shipped 2026-09-08 | #13 follow-up |
| #47 | Enable the SearXNG search provider in the live config; live-verify surfaces + fail-open combo — done 2026-09-08 | #13 follow-up |
| ~~#48~~ | ~~b-ai premium-gated accounts surface 403 "Deposit required" instead of cooling down~~ — **done 2026-09-09** (242f303): narrow gated-403 signature (403 + access_denied/"Deposit required") benches the ACCOUNT on the adaptive 429 ladder (deposit-clearing success resets via pool.ok), marks Fallbackable — buffered rotates accounts→combo targets, router rotates pool-bounded without spending retry budget, all-gated pools answer 429+Retry-After, stream fast path answers pre-body; non-gated 403s fail fast unchanged; quota-503 untouched | found live testing #47 |
| ~~#50~~ | ~~Cross-format encode path never coerces always-thinking effort (residual from #17)~~ — **done 2026-09-09** (8b47d9d): coerceAlwaysThinkingUnified in prepareUpstreamBody's cross-format branches — client-set ReasoningEffort coerced via coerceEffort (empty stays empty, never invented), Anthropic thinking budget dropped (no GLM-wire representation; budgetToEffort can emit outside low|high|max); streaming already herds into the buffered path; mutation-checked regression test on the Responses wire; zero-drop deployed pid 74239, live-verified | #17 residual |
| ~~#51~~ | ~~Baseline research: peer gateways + free-model inventory (omp+onegw VPS foundation)~~ — **done 2026-09-09**; deliverable in the issue body (six tools verified vs source/docs/live endpoints); follow-up #55 tracks the actual VPS deploy | user request |
| ~~#53~~ | ~~b-ai distributor nodes parse-reject large valid bodies as terminal 400~~ — **done 2026-09-09** (84fd1c9 + 51ccf1e): translat.UpstreamParseRejected (narrow 400 + api_error + "Invalid request body" + request-id trailer) rewrites to retryable 502 upstream_parse_rejected, no bench, Router retries / combo falls through; NormalizeInStreamError covers the in-stream variant; genuine schema 400s stay terminal (test-pinned); zero-drop deployed pid 53121, live-verified | #52 follow-up |
| #54 | Task-aware combo reordering: local difficulty classification + stable re-sort of combo targets (#44 step 2) | #44 follow-up |
| #55 | Deploy omp+onegw coding tool on personal VPS | #51 follow-up |
| #58 | Merlin AI (getmerlin.in) upstream integration — research done (pricing, Firebase-auth wire contract live-verified 2026-09-09 incl. guest free-tier chat, adapter landscape, native kind="merlin" vs bridge options); implementation pending | user request |

### Recommended implementation order (2026-09-08)

Tier 1 — reliability first (incident follow-ups) — **done 2026-09-08**:
~~#43~~ → ~~#39~~ → ~~#38~~ → ~~#37~~ → ~~#40~~ all landed and closed (the #39/#38
features were later deliberately reverted, 9049dd4); ~~#42~~ ownership model
landed 2026-09-08 (`owner.json` + `/admin/health` owner block). Tier 1 complete.

Tier 3 — remaining 2026-09-09: #50 (cross-format effort coercion, in progress) → #32 + #34/#35
(encode-layer cache-field preservation + per-provider cache profiles; candidates to merge into one
feature) → #54 (task-aware combo reordering, #44 step 2) → #55 (VPS deploy) → #14 remainder
(`onegw connect <tool>`, launchd/systemd unit).
#31/#33/#36/#48 landed 2026-09-09; #44 closed (research + step 1, step 2 → #54); #51/#53 closed 2026-09-09.

### Always-thinking effort coercion (#16, done 2026-09-07)

GLM-5.3 family models always reason; the upstream rejects explicit
disable-thinking requests with error 1210 ("use low, high or max"). New
per-provider config `always_thinking = ["model-glob", ...]` (path.Match
syntax) makes onegw rewrite such requests on the passthrough path instead
of forwarding them: `reasoning_effort` none|minimal|medium → low; thinking
`{type:disabled}` and `enable_thinking:false` are dropped (upstream default
thinking-on applies). Knobs are never invented — only explicit disable
requests are rewritten, so providers that legitimately accept "none"
(OpenAI gpt-5.x) are untouched unless listed. Regression tests in
`internal/server/thinking_test.go`; live glm route verified.

Snapshot mirror with done-history: `docs/prd-task-tracker.md`.

### Distribution and setup (#14)

Requested 2026-09-07; four independently shippable workstreams (detail in
the issue):

- **Auto-release CI/CD — done 2026-09-08** (v0.1.0 shipped). `.github/workflows/release.yml`:
  push to master → next semver from commit subjects (BREAKING CHANGE/type!→major,
  feat→minor, fix/perf→patch; any other non-merge type incl. free-form
  `dashboard:`/`saver:`/typeless→patch; `docs:`/`test:`/`chore:`/`ci:` skip;
  bootstrap first tag = v0.1.0) → tag pushed →
  4 static binaries (`linux`/`darwin` × `amd64`/`arm64`, `CGO_ENABLED=0`,
  `-trimpath -ldflags "-s -w"`) via build matrix → `gh release create --generate-notes`
  with binaries attached; `latest` tracks newest. Verified end-to-end: first run cut
  v0.1.0; darwin-arm64 artifact executed locally.
- **One-command local install — done 2026-09-08.** `scripts/install.sh`
  (`curl -fsSL .../scripts/install.sh | sh`): detects OS/arch, installs the
  latest release binary (prefix `/usr/local`, falls back to `~/.local/bin`),
  writes a starter `~/.onegw/onegw.toml` (loopback bind, generated admin
  password), starts the gateway and verifies via the unauthenticated
  dashboard root (`/admin/*` is password-gated). Env overrides: `ONEGW_BIN`,
  `ONEGW_VERSION`, `ONEGW_LISTEN`, `ONEGW_KEYS`, `ONEGW_START`. Sandbox-tested;
  launchd/systemd unit remains an optional follow-up.
- **One-command cloud/VPS deploy — done 2026-09-08.** Multi-stage `Dockerfile`
  (static CGO-free binary in alpine, non-root uid 100, `/data` volume,
  healthcheck on the unauthenticated dashboard root) published as
  `ghcr.io/freepeak/onegw:{latest,<tag>}` (linux/amd64+arm64) by the release
  workflow's `docker` job; `docker-compose.yml` VPS example (restart policy,
  resource limits, named volume, optional config mount); README one-command
  sections for both paths. Verified: local build 41.8 MB image, healthcheck
  healthy, usage.db persisted across restart, open-proxy guard refuses
  0.0.0.0 without keys, RSS 9.4 MB at boot.
- **One-click agent-CLI integration** — `onegw connect <tool>` writes
  provider/base-URL + key into omp.sh, pi.dev, Claude Code, opencode, and
  grok cli configs (pi `models.json` is the proven pattern).

## Current status (post-M5)
- **Upstream pre-first-byte timeouts on massive prefills — RCA + fix**
  (2026-09-08 evening, branch `fix/upstream-header-timeout`): the b-ai
  glm-5.3-flash dashboard showed recurring
  `502 upstream_error — http2: timeout awaiting response headers` on
  30-300K-token requests (0 in / 0 out, i.e. dead before first byte) while
  neighbors succeeded. Root causes: (1) the shared upstream HTTP transport
  had a fixed 60s `ResponseHeaderTimeout`, and cold-cache prefills of
  massive thinking-model sessions can legitimately exceed it — the abort
  was a gateway-side budget, not an upstream outage (upstream answered
  nothing; every failed attempt was ~60s of dead latency); (2) the failure
  logged as `502 upstream_error` because transport errors were not
  classified, hiding both the timeout nature and its retryability
  (`Retryable()` covers 504, and combos DID fall through, but the log gave
  no signal). Fix: `[server] response_header_timeout` knob (per-Def
  memoized HTTP client, SIGHUP-safe; live config = 120s) +
  `transportErr` classification (504 `upstream_timeout` /
  502 `upstream_unreachable` / 499 `client_closed`). Classification
  subtlety pinned by regression test: Go's header-timeout error BOTH
  implements `net.Error` `Timeout()=true` AND satisfies
  `errors.Is(err, context.DeadlineExceeded)` (probe-verified h1 + h2,
  Go 1.25), so client-hangup detection must consult the REQUEST CONTEXT,
  not the error chain — and timeout detection walks the whole unwrap chain
  (`errors.As` stops at the outermost `*url.Error`, whose `Timeout()`
  only type-asserts its direct child). Zero-drop deployed (pid verified);
  post-deploy the same failure class logs `504 upstream_timeout` and falls
  through the combo.
- **Budget-exhausted timeouts no longer burn a second budget — RCA + fix**
  (2026-09-09, this entry): a direct `tokenrouter/z-ai/glm-5.3-free`
  request logged `504 upstream_timeout — net/http: timeout awaiting
  response headers` (0 in / 0 out) after TWO silent 120s waits. Root
  causes: (1) the tokenrouter free lane's pre-first-byte latency is
  bimodal — live probes on identical ~296 KB streaming bodies measured
  17-37s TTFB, and the failing attempt saw >120s (aggregator queue
  saturation, upstream side, nothing to fix there); (2) the gateway
  treated that budget exhaustion like a cheap 5xx and re-attacked the
  SAME target (`MaxAttempts=2`), doubling the dead latency before the
  client saw the 504 — pointless, because the pre-first-byte demand is
  a property of the request, not of the attempt. Fix: `transportErr`
  flags the ResponseHeaderTimeout flavor (exact net/http abort text;
  dial/TLS timeouts stay retryable) with `NoSameTargetRetry`, and
  `Router.Execute` falls through to the next combo target immediately
  (a direct route surfaces the 504 after ONE budget). Regression tests
  pin both flavors; the ordinary 504 keeps its retry.
- **9router importer** (`cmd/import9r`): reads 9router's data.sqlite, imports
  API-key connections as onegw providers (accounts, upstream model discovery,
  gateway auth keys) and OAuth bearer-token connections whose upstream
  accepts the token as-is (`bearerTokenProviders`: xai → api.x.ai, kilocode →
  api.kilo.ai/api/openrouter; per-endpoint models URL). JWT `exp` parsed and
  surfaced as a rotate-me comment when under 48 h. Custom wire formats
  (commandcode NDJSON, grok-cli Responses, cursor protobuf) skipped with
  warning — #12. Real config lives in `onegw.toml` (gitignored).
- **Live providers**: B.AI (7 accounts, 48 models, round-robin + fallback
  verified), GLM (1 account), **kilocode** (1 bearer account, 371-model
  OpenRouter-style catalog incl. `kilo-auto/free`; free-model chat + SSE
  stream + Anthropic-surface translation verified live), **xai** (1
  bearer account, 12 models; `grok-4.6` chat verified live) and
  **orcarouter** (2026-09-08, user key; `openai-responses` kind — the first
  configured upstream on a #12 custom wire — base
  `https://api.orcarouter.ai/v1`, four free models incl. pooled
  `orcarouter/free`; the three concrete free models verified live buffered +
  streaming; upstream's pooled-free quota was already exhausted, so that id
  answers `free_quota_exhausted` until topped up). Fixes that
  fell out: URL version-segment join (`/v1`, `/api/paas/v4` bases), upstream
  model rewrite on same-format passthrough, `developer`→`system` role
  normalization (pi CLI payloads).
- **Sticky account round-robin** (2026-09-08): per-provider `sticky = "5m"`
  pins one upstream account to a request identity (client session header,
  else auth-key label) for the window so repeat calls reuse the same key
  (upstream prompt-cache friendly); identity rides the request context into
  `Router.Execute`, the first pick rotates and pins, repeats reuse the pin,
  and any failed attempt unpins (retries and combo fallthrough never
  re-stick to a dead key) — cooling pins rotate and re-pin. Map is bounded
  (4096, expire-swept). orcarouter runs 4 accounts (linh, harvey, clone2,
  clone1) with `sticky = "5m"`; unit + end-to-end failover tests in
  `internal/provider/sticky_test.go`, `internal/server/sticky_test.go`.
- **pi CLI wired**: `onegw` provider in `~/.pi/agent/models.json` + ONEGW_KEY
  env; full agent loop (read/edit/bash) tested through onegw.
- **Dashboard**: password-gated persisted rollups with a usage range filter
  (today / 7 days / 1 month / all time — default all time, selection kept in
  the `?range=` query so it survives reload), aggregated per provider+model;
  header totals show reqs, in/out/**sum**/saved tokens — token counts use a
  K/M/B compact ladder (≥ 1e9 → `B`, trailing `.0` trimmed), saver "saved" column,
  health/mem strip, live in-flight concurrency gauge (counter incremented
  across the proxy pipelines, shown as `live` and exposed as `inflight` in
  `/admin/health`), 401 flow verified in browser.
- **Claude Code wired + Anthropic SSE block synthesis fix** (2026-09-08,
  c8422d1): `~/.claude/settings.json` now points at onegw
  (`ANTHROPIC_BASE_URL=http://127.0.0.1:8080`, Bearer key, combos
  `dev`/`free`) — Claude Code speaks Anthropic `/v1/messages` with strict
  SSE block discipline, which exposed a translat bug: OpenAI/Gemini
  upstreams emit bare text/thinking deltas, and the Anthropic encoder
  streamed `content_block_delta` with no `content_block_start`, reusing one
  index across thinking and text — Claude Code accumulated an empty reply.
  The encoder now opens a block on the first delta, closes it on part-type
  change or at finish, and retires the client index on every close so a
  closed index is never reopened; regression test pins the full event
  sequence (thinking@0 → text@1).
- **Web search (SearXNG)** (#13, shipped 2026-09-08 in 9bd3594): `kind =
  "searxng"` virtual provider — clients send model `search/query`, the gateway
  answers with a SearXNG JSON search (last user message = query) formatted as
  a synthetic OpenAI completion through the normal pipeline (cross-format
  translation, combos, usage rollups). Instance failures are retryable 503
  `search_unavailable` errors, so combos fail open to the next model. Unit +
  E2E tests (OpenAI/Anthropic surfaces). **Disabled live 2026-09-08 (user
  request)**: the SearXNG container and image were removed and the live
  onegw.toml no longer carries the `search` provider or the `search-or-llm`
  combo (verify with `/v1/models` — no `search/…` entries); the compose
  service, settings, and provider kind remain in-repo for re-enablement.
  Live history: enabled 2026-09-08 (#46/#47), loopback 127.0.0.1:8888,
  fail-open combo ["search/query", "b-ai/mimo-v2.5"], verified buffered /
  Anthropic cross-format / SSE / fail-open fall-through.
- self-update is built in (`onegw update` / `onegw version` commands,
  2026-09-08): background release checks (`[update] check_interval`,
  default 24h; `auto` opt-in apply), `/admin/update` status/check/apply
  endpoints, and a zero-drop self-handoff — download (sha256-verified) →
  smoke-run → atomic swap with `.old` backup → SO_REUSEPORT overlap → the
  new process must answer `/admin/update` with its own pid → drain old
  pid; every failure rolls back with the old gateway still serving.
  Container deployments check + log and print host-side
  `docker pull`/recreate commands instead of self-applying (the image
  owns the filesystem). Release binaries and Docker images are
  version-stamped by CI so `onegw version` reports the tag.
  **Dashboard surface (2026-09-09, #61):** the Settings page has a
  Version card — `Check now` and `Update now` buttons over
  `GET/POST /admin/api/v1/update` (same admin gate as the rest of the
  dashboard: header or session cookie; `/admin/update` stays the
  header-only CLI/probe surface). Local runs: Update applies the
  zero-drop handoff and the page polls until the pid changes / the
  session 401s / the endpoint 404s, then reloads. Docker containers:
  apply answers 409 with the exact host-side `docker pull` + recreate
  commands (the image owns the filesystem), so the button works in both
  modes. Live-verified end-to-end on a throwaway instance: 202 →
  download → smoke → SO_REUSEPORT takeover → old pid drained ("updated
  to v0.12.7"), and the container 409 guidance rendered verbatim.
- onegw runs as a supervised persistent service on 127.0.0.1:8080 with
  autoresume: the supervisor restarts it on abnormal exit (crash, OOM,
  SIGKILL; bounded backoff) — kill-tested live; deliberate stops stay
  stopped. Deploys remain rolling: pre-build, atomic binary swap, graceful
  drain via SIGTERM. Restarts and drains go through the supervisor by name
  or by explicit PID — never `pkill -f`: the pattern matches the
  replacement too (identical command lines), and a graceful SIGTERM exit 0
  is a "deliberate stop" the supervisor intentionally does not autoresume
  (2026-09-07 outage root cause).

## Docs

- `docs/ARCHITECTURE.md` — package detail, memory contract, config reference.
- `README.md` — project overview, quick start, benchmarks; `LICENSE` (MIT).
- GitHub issues — the durable task record (see Open work above).
- `docs/prd-task-tracker.md` — historical done-list + issue snapshot mirror.
- `bench/memory.sh` — RSS measurement harness; `scripts/smoke.sh` —
  end-to-end surface tests; `cmd/mockupstream` — fake provider.
- `docs/dashboard-deep-dive.md` — dashboard build-approach research (#45,
  companion to #41): stack, SSE plumbing, auth prerequisite, API shape,
  landing order.

Dashboard Tailwind v4 revamp (0b6202d): professional restyle of all 9 admin
  pages to the UnoRouter design language (user-selected reference,
  unorouter.com/en/compare) — near-black canvas, hairline white/10 borders,
  sharp corners, inverted primary buttons, zinc muted grays, status
  emerald/amber/red, chart palette 1–5. Pipeline researched and pinned:
  Tailwind v4 standalone CLI (single static binary, no Node/npm; fits the
  no-package.json zero-external-assets constraint) compiles
  `internal/server/dashboard/admin.src.css` (@theme tokens + @layer
  components shared with Go-rendered SSE fragments) to the committed,
  go:embed'ed `static/admin.css` — regenerate via
  `scripts/dashboard-css.sh`. Vendored OFL variable fonts (Space Grotesk,
  Plus Jakarta Sans, JetBrains Mono latin woff2) served from
  `GET /admin/assets/fonts/{name}`. Sidebar grouped Monitor/Routing/
  Gateway; usage charts got themed uPlot axes (JetBrains Mono ticks,
  reserved gutters fixing left-clipped labels). Browser-verified dark/
  light on a scratch instance; targeted suite green in-tree and from a
  clean git-archive build of the commit. Peer metrics work (acct-labeled
  log entries) remains uncommitted in the tree by design.

Local-calendar dashboard windows (e39fb12): every admin-dashboard time
  surface now follows the gateway host's LOCAL calendar instead of UTC —
  usage page range/table/charts ("per hour (local)", "local day YYYY-MM-DD
  (+07)"), overview "today" stats + hourly chart, right-rail 7-day rank,
  saver all-time sum, settings config-mtime, export/daily API default
  windows. Rollups stay UTC-keyed on disk (retention, node transfer, quota
  rebuilds untouched); each local window maps onto its UTC-key superset and
  rows re-bucket by the instant each (day,hour) key represents, so local
  days straddling two UTC keys (e.g. UTC+7 00:00–06:59 = yesterday 17:00+
  UTC) no longer drop their edge hours — pinned by
  TestUsageTodaySpansUTCDayBoundary + TestChartDayModeLocalBuckets
  (mutation-verified red pre-fix) and a UTC/UTC+7/UTC−8/UTC+14 suite sweep.
  Zero-drop deployed from the origin/master archive (live page verified:
  xs[0] == local-midnight epoch).

Usage dashboard chart fix (9e1e5a4): the tokens chart legend/hover showed
  cumulative raw integers (stack accumulation without a per-series `value`
  formatter) — cache read/output/input read as wrong values with no K/M/B
  unit. Series now map back to their per-layer arrays via kmb; stacked
  visuals unchanged. Cards/table were always correct (Go compact).

---


*Last updated: 2026-09-09 (local-calendar dashboard windows, e39fb12: usage/overview/rank
  windows, charts, and labels now render the host's local time — "per hour (local)" — while
  rollups stay UTC-keyed; local windows map onto UTC-key supersets so straddling local days
  no longer drop edge hours; zero-drop deployed and live-verified; earlier: README dashboard
  docs refresh (71fc475): replaced the overview
  screenshot with a current dark-mode capture (Playwright+Chrome headless against the live
  admin, 2x scale) and updated the Overview description — hourly token chart, top-providers
  rail, Providers/Combos in-page editing, Settings maintenance card; added dark-by-default +
  persistent ◐ theme-toggle note; docs-only, no binary change; earlier: zai glm provider outage RCA + config fix: glm/glm-5.3-flash
  direct routes and the dev-combo last rung returned 500 upstream_empty_body from ~06:32Z
  after Z.AI repurposed https://api.z.ai/api/v1 into a dedicated Codex (OpenAI Responses
  protocol) endpoint for GLM Coding Plan keys (docs.z.ai/devpack/tool/codex) — chat
  completions there now 403 model_access_denied, or 500 + empty body under the SSE
  Accept header onegw sends, which masked the denial as upstream_empty_body (an earlier
  same-day peer session had named the empty-body symptom but attributed it to model
  deprovisioning); root-caused by direct-key probes: same coding-plan key 200 OK on
  https://api.z.ai/api/coding/paas/v4/chat/completions, 403 on /api/v1; fix = glm
  provider base_url → https://api.z.ai/api/coding/paas/v4 in BOTH the live
  ~/.onegw/onegw.toml (SIGHUP hot-reloaded, zero-drop) and the repo onegw.toml;
  end-to-end verified via /v1/messages stream + request ring 200 @harvey; the 22:33
  key rotation (15d3854e→ac679568) was a red herring — both keys are valid coding-plan
  keys; earlier: tokenrouter engine admission walls are shared, not per-key
  — issue #64, fix 5f365b2 pushed: the 21:37 client 503 "cache-only admission rejected a
  cold, unavailable, or overloaded request" is tokenrouter's z-ai engine cache-aware
  cold-prefill admission rejecting ~180K-token all-uncached requests when concurrent
  sessions exhaust the engine's outstanding-uncached budget; ring proves the wall is
  shared — 429 @harvey 15:01:19 → identical request 200 @linh in=159561 15:01:23, both
  accounts also served 200s; onegw misclassified the 429s as per-key (benched healthy
  keys on the ladder) and the 503s as ordinary retryable (same-target retry re-sent the
  same 214K uncached prefill into the saturated engine 100ms later); SharedConcurrency()
  extended to the admission family (429 BackendAdmissionRejected + 503 cache-only/
  gateway-overloaded variants): no benching, immediate combo fall-through, 2s Retry-After
  on direct routes, 503 preserved; tests in translat/provider/router, mutation-checked;
  zero-drop deployed live (pid 98253, ~14 inflight at swap); post-deploy ring: admission
  events now one attempt each — 429 @harvey 15:52:16 → 200 @harvey 15:52:26 (was: 1s-apart
  same-target retry), no pool-empty, healthy keys stay on the ladder; also fixed in the
  shared tree (uncommitted, peer #59 WIP): deploy.sh check_markers grep -q SIGPIPE false-fail
  (issue #62 follow-up); earlier: rate-limit error-log RCA + governor restoration deploy: deep-dive
  on the live request ring during a high-throughput burst (16:43-16:48, ~130 req/min) showed
  168 of 512 ring rows were 429s — 140 upstream per-account (up to 11 429s/min on ONE key,
  impossible under the #56 rpm=5 token bucket) + 16 Tencent shared-wall + 12 tokenrouter
  8/min-wall; root cause: FOUR successive peer deploy generations (pids 15538/89605/26764,
  /tmp/onegw-mk-binary) shipped binaries built from stale refs predating 729c190/9a3dbc0 —
  strings check: newTokenBucket/refillAt/upstream_empty_body all 0; fix: archive-built
  origin/master d41d078 and zero-drop deployed (scripts/deploy.sh --binary, pid 996);
  post-deploy verification: per-account attempts capped ≤6/min and ≤2/sec (burst-2 governor
  observable), 8 residual 429s in 3 min vs 168 in 5 — all residual rows are the two by-design
  classes (shared-wall fall-through per 359e5a0 + burst-edge per-account); no code change,
  ops-only; earlier: tokenrouter 504 budget fix: pre-first-byte
  exhaustion marked NoSameTargetRetry, Router.Execute falls through instead
  of a second silent 120s retry on the same target; dial/TLS timeouts stay
  retryable; live TTFB probes 6-37s nominal, bimodal free-lane queue events
  past 120s; earlier: request log diagnosability: rows carry the upstream
  account name (@account in the console line, account JSON field) + 7-day
  auto-clear, ab62468 — committed from a clean archive of HEAD (full suite green
  in /tmp), zero-drop deployed live (pid 67617), end-to-end verified: b-ai rows
  show per-request accounts clone3/clone2/harvey; earlier: dashboard Tailwind v4 revamp to the UnoRouter design
  language, 0b6202d — standalone-CLI pipeline, vendored OFL fonts, grouped sidebar,
  themed uPlot axes; browser-verified dark/light, commit green from clean archive,
  pushed; earlier: usage-chart hover fix: per-layer values + units,
  9e1e5a4, zero-drop deployed; earlier RCA: b-ai/glm-5.3-flash 502 storms — fixed 60s pre-first-byte budget aborted massive thinking-model prefills (`http2: timeout awaiting response headers`);
`[server] response_header_timeout` knob (live: 120s) + transport-error classification
(504 upstream_timeout, retryable so combos fall through; client-hangup detection via request
ctx state — Go's header-timeout error also aliases context.DeadlineExceeded, probe-verified
h1+h2, Go 1.25); zero-drop deployed live, failures now 504-classified and fall through; branch
fix/upstream-header-timeout; earlier: admin login lockout aligned to spec (24h after 5 failures,
6790dba) — master pushed through 6790dba and live gateway redeployed zero-drop from it (pid in
/admin/health); xai OAuth token still expired — re-auth in 9router then re-import)*
