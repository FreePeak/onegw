// Package router resolves client model strings to concrete provider/account
// targets and drives fallback across ordered combo targets.
package router

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"onegw/internal/provider"
	"onegw/internal/types"
)

// Target is one leg of a route: provider + model.
type Target struct {
	Provider string `toml:"provider"`
	Model    string `toml:"model"`
}

// Combo is an ordered fallback chain of targets.
type Combo struct {
	Name     string   `toml:"name"`
	Targets  []Target `toml:"targets"`
	Strategy string   `toml:"strategy"`
	// RoundRobinLimit (strategy = "round-robin" only): how many
	// consecutive successes keep a target first before rotation moves on
	// (#82). 0 = default 3; clamped to 1..1000.
	RoundRobinLimit int `toml:"round_robin_limit"`
}

// Router resolves model strings and executes calls with fallback.
type Router struct {
	mu      sync.RWMutex
	pool    *provider.Pool
	models  map[string]directRoute
	combos  map[string]*Combo
	aliases map[string]string

	// Sticky round-robin state per round-robin combo (#82): combo name →
	// counter + currently-winning target. In-memory by design (a reload
	// rebuilds the router; at most one sticky run is forgotten).
	rrMu sync.Mutex
	rr   map[string]*comboRR

	// PoolEmptyError, when set, builds the error Execute reports when a
	// target's whole account pool is cooling and the upstream attempt is
	// skipped. The default (DefaultPoolEmptyError) answers 429
	// provider_rate_limited with the pool's soonest recovery as
	// Retry-After; the server overrides it to surface richer semantics
	// (quota-window exhaustion answers 503 provider_quota_exhausted with
	// the window end — that gate lives inside the Caller, which Execute
	// skips when it never picks an account).
	PoolEmptyError func(def *provider.Def, ready time.Time) *types.APIError

	// Task routing (issue #54): taskRoutingOn gates the per-request
	// combo reorder inside Execute; TaskLog receives one #19-ring
	// decision line per request whose target order actually changed.
	// Both are set by the server in apply() from config (server.apply
	// builds a fresh Router on every reload, so plain fields set once
	// before serving are race-free). Default off = byte-identical
	// routing; TaskLog nil = silent.
	taskRoutingOn bool
	TaskLog       func(model, detail string)

	// SpeedLog receives one #19-ring decision line whenever the
	// size-aware (prefill) steering changes a combo's serving order.
	// Independent of task routing: the measured prefill rate is always
	// live once requests have folded samples, so its decision is worth
	// recording even when task routing is off. nil = silent.
	SpeedLog func(model, detail string)

	// maxAttempts per target before falling to next (network/5xx).
	MaxAttempts int
}

// MaxAliasHops caps alias chain resolution; Validate enforces the same cap
// on the config side.
const MaxAliasHops = 8

type directRoute struct {
	provider string
	model    string
}

func New(pool *provider.Pool) *Router {
	return &Router{
		pool:        pool,
		models:      map[string]directRoute{},
		combos:      map[string]*Combo{},
		aliases:     map[string]string{},
		rr:          map[string]*comboRR{},
		MaxAttempts: 2,
	}
}

// DefaultPoolEmptyError is the plain rate-limit answer for a cooling pool:
// 429 with the soonest recovery as Retry-After.
func DefaultPoolEmptyError(def *provider.Def, ready time.Time) *types.APIError {
	cool := time.Until(ready)
	if cool < 0 {
		cool = 0
	}
	return &types.APIError{Status: 429, Type: "provider_rate_limited",
		Code:       "rate_limit_exceeded",
		RetryAfter: strconv.FormatInt(int64(cool.Seconds())+1, 10),
		Message: fmt.Sprintf("provider %s: all accounts rate-limited upstream; retry after %ds",
			def.Name, int64(cool.Seconds())+1)}
}

// poolEmptyError returns the configured builder, defaulting to the plain
// 429 (nil-check on the instance so tests can build routers directly).
func (r *Router) poolEmptyError(def *provider.Def, ready time.Time) *types.APIError {
	if r.PoolEmptyError != nil {
		return r.PoolEmptyError(def, ready)
	}
	return DefaultPoolEmptyError(def, ready)
}

// SetModels replaces the direct-route table (parsed from config).
func (r *Router) SetModels(routes []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := make(map[string]directRoute, len(routes))
	for _, route := range routes {
		prov, model, ok := strings.Cut(route, "/")
		if !ok {
			continue
		}
		m[strings.ToLower(route)] = directRoute{provider: prov, model: model}
	}
	r.models = m
}

// SetAliases replaces the alias table (alias → "provider/model", combo
// name, or another alias). Chains resolve iteratively in Resolve.
func (r *Router) SetAliases(m map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := make(map[string]string, len(m))
	for k, v := range m {
		if k == "" || v == "" || k == v {
			continue
		}
		t[strings.ToLower(k)] = v
	}
	r.aliases = t
}

// SetCombos replaces the combo table.
func (r *Router) SetCombos(list []*Combo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := make(map[string]*Combo, len(list))
	for _, c := range list {
		m[strings.ToLower(c.Name)] = c
	}
	r.combos = m
}

// SetTaskRouting enables task-aware combo reordering (issue #54). The
// server calls it in apply() from [server] task_routing; off (default)
// keeps Execute byte-identical to the pre-#54 behavior.
func (r *Router) SetTaskRouting(on bool) {
	r.mu.Lock()
	r.taskRoutingOn = on
	r.mu.Unlock()
}

// KnownModel reports whether model is a name the current route tables can
// resolve: a configured "provider/model" route, a combo name, an alias
// (chains followed like Resolve), or a bare model — either advertised in a
// models table or servable by the bare-model fallback, which Resolve routes
// to the first configured provider. Consumers of raw client model strings
// (metrics labels) use it to keep cardinality bounded: unroutable model
// strings come from unauthenticated client traffic and must not create a
// new label series per distinct string.
func (r *Router) KnownModel(model string) bool {
	if model == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	lookup := model
	if _, isCombo := r.combos[strings.ToLower(lookup)]; !isCombo {
		hops := 0
		for hops < MaxAliasHops {
			nxt, ok := r.aliases[strings.ToLower(lookup)]
			if !ok {
				break
			}
			lookup = nxt
			hops++
		}
		if hops >= MaxAliasHops {
			lookup = model
		}
	}
	if _, ok := r.combos[strings.ToLower(lookup)]; ok {
		return true
	}
	if _, ok := r.models[strings.ToLower(lookup)]; ok {
		return true
	}
	if prov, _, ok := strings.Cut(lookup, "/"); ok {
		if _, exists := r.pool.Get(prov); exists {
			return true
		}
		// Unknown provider prefix: still known when the string is
		// advertised by some provider's models table — combo targets like
		// "z-ai/glm-5.3-flash" (commandcode) carry a slash yet are config
		// models. Their failure rows must not collapse to "unresolved"
		// (live 2026-09-09: commandcode 520s logged as commandcode/unresolved).
		// Junk "foo/bar" strings stay unknown: cardinality stays bounded.
		for _, dr := range r.models {
			if strings.EqualFold(dr.model, lookup) {
				return true
			}
		}
		return false
	}
	// Bare model: advertised in a models table, or the fallback route to
	// the first configured provider applies.
	for _, dr := range r.models {
		if strings.EqualFold(dr.model, lookup) {
			return true
		}
	}
	return len(r.pool.Names()) > 0
}

// Resolution is the ordered plan for one model string.
type Resolution struct {
	Targets []Target // provider/model pairs, fallback order
	IsCombo bool
	Model   string // the client-facing model string that resolved (task-log label)
	// RoundRobin (combo strategy = "round-robin"): Execute rotates the
	// chain so the sticky/counter-selected target is FIRST, and records
	// the winner per combo name so stickiness can expire (#82).
	RoundRobin bool
	Combo      string // canonical combo name, empty for direct routes

	// rrLead is the pre-rotation index of the target leading this
	// round-robin resolution (internal; 0 when not rotated).
	rrLead int

	// SpeedOrder marks a combo whose config declares strategy = "fastest":
	// Execute stable-sorts the targets by recent decode speed before the
	// first attempt (unknown speeds keep the configured order).
	SpeedOrder bool
}

// Resolve maps a client model string to an ordered target list.
//   - "provider/model" → single direct target
//   - combo name       → ordered combo targets
//   - bare "model"     → matched against any provider's advertised models
func (r *Router) Resolve(model string) (*Resolution, *types.APIError) {
	if model == "" {
		return nil, &types.APIError{Status: 400, Type: "invalid_request", Message: "missing model"}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Aliases first: they name combos or provider/model routes. Chains
	// resolve iteratively with a hop cap; a cycle just falls through to
	// the normal lookup (and Validate rejects cycles anyway).
	lookup := model
	if _, isCombo := r.combos[strings.ToLower(lookup)]; !isCombo {
		hops := 0
		for hops < MaxAliasHops {
			nxt, ok := r.aliases[strings.ToLower(lookup)]
			if !ok {
				break
			}
			lookup = nxt
			hops++
		}
		if hops >= MaxAliasHops {
			// Exhausted the hop cap (Validate rejects cycles upstream
			// anyway): treat as no alias so the normal lookup answers
			// with context.
			lookup = model
		}
	}
	if c, ok := r.combos[strings.ToLower(lookup)]; ok {
		res := &Resolution{Targets: append([]Target(nil), c.Targets...), IsCombo: true, Model: lookup, Combo: c.Name}
		switch c.Strategy {
		case "fastest":
			res.SpeedOrder = true
		case "round-robin":
			res.RoundRobin = true
		}
		return res, nil
	}
	model = lookup
	if dr, ok := r.models[strings.ToLower(model)]; ok {
		return &Resolution{Targets: []Target{{Provider: dr.provider, Model: dr.model}}}, nil
	}
	if prov, mdl, ok := strings.Cut(model, "/"); ok {
		if _, exists := r.pool.Get(prov); exists {
			return &Resolution{Targets: []Target{{Provider: prov, Model: mdl}}}, nil
		}
		return nil, &types.APIError{Status: 404, Type: "unknown_provider", Message: "unknown provider " + prov}
	}
	// Bare model: try providers in order that could serve it. Disabled
	// providers (paused via the dashboard toggle) never win this
	// fallback — a paused provider must not silently absorb passthrough
	// traffic that an enabled provider could serve.
	for _, name := range r.pool.Names() {
		if d, ok := r.pool.Get(name); ok && !d.Disabled {
			return &Resolution{Targets: []Target{{Provider: name, Model: model}}}, nil
		}
	}
	return nil, &types.APIError{Status: 404, Type: "model_not_found", Message: "no provider for model " + model}
}

// Request identity rides the context: handlers tag each request with the
// client session id or auth-key label so sticky account pools can pin it.
type identityKey struct{}

// WithIdentity tags ctx with the request's affinity identity ("" = none).
func WithIdentity(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, identityKey{}, id)
}

// inputSizeKey carries the request's estimated input size (tokens) so the
// combo ordering can weigh the phase that dominates large requests. Set by
// the server before Execute; absent for headerless/test callers, which keep
// decode-only ordering.
type inputSizeKey struct{}

// WithInputSize attaches the request's estimated input token count.
func WithInputSize(ctx context.Context, tokens int64) context.Context {
	return context.WithValue(ctx, inputSizeKey{}, tokens)
}

func inputSizeFrom(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	n, _ := ctx.Value(inputSizeKey{}).(int64)
	return n
}

// IdentityFrom extracts the identity tagged by WithIdentity.
func IdentityFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(identityKey{}).(string)
	return id
}

// Execute runs the resolution: for each target pick an account and call; on
// retryable failure try again, then fall through to the next target. A
// pre-first-byte budget exhaustion (NoSameTargetRetry) skips the same-target
// retry — the pre-first-byte demand is fixed, only fall-through can help.
// onResult receives the successful result.
func (r *Router) Execute(ctx context.Context, res *Resolution, call Caller, onResult func(any)) *types.APIError {
	// Task-aware combo reordering (issue #54): stable re-sort of the
	// target list before any account selection. No-op unless task
	// routing is on and the caller tagged request signals; the full
	// fallback chain is preserved — only the order changes.
	r.applyTaskRouting(ctx, res)
	// Throughput steering (combo strategy = "fastest"): a stable re-sort of
	// the targets by the leg that is expected to finish THIS request first;
	// legs with no data for the request's size keep the configured order and
	// nothing is removed. For requests from PrefillMattersAt tokens upwards
	// the ranking uses measured prefill (provider/prefill.go) — decode speed
	// alone cannot predict a 200K-token request, where ring evidence shows
	// the pre-first-byte phase carrying ~97% of the wall time.
	if res.IsCombo && res.SpeedOrder {
		r.reorderBySpeed(ctx, res)
	}
	// Sticky round-robin (combo strategy = "round-robin", #82): rotate the
	// chain so the target that should serve this request is first — the
	// sticky winner while its success run is under the limit, else the
	// counter position. A successful call records the winner for the next
	// request; failures just fall through the chain as usual.
	if res.IsCombo && res.RoundRobin {
		r.rotateRoundRobin(res)
	}
	var lastErr *types.APIError
	id := IdentityFrom(ctx)
	for _, t := range res.Targets {
		def, ok := r.pool.Get(t.Provider)
		if !ok {
			lastErr = &types.APIError{Status: 404, Type: "unknown_provider", Message: "unknown provider " + t.Provider}
			continue
		}
		if def.Disabled {
			// Paused provider (dashboard toggle, persisted as
			// `disabled = true` in its [[providers]] block): the target
			// is skipped so a combo falls through to the next leg; a
			// direct route surfaces an honest 503, not a 404 that would
			// claim the route never existed.
			lastErr = &types.APIError{Status: 503, Type: "provider_disabled", Message: "provider " + t.Provider + " is disabled"}
			continue
		}
		if benched, ready := def.ModelBenched(t.Model); benched {
			// Per-model lockout (BenchModel): Do benched this
			// (provider, model) pair after a model-scoped upstream
			// refusal (Zhipu model_access_denied 403, model_not_found
			// 404). Skip the target with ZERO upstream attempts — the
			// refusal indicts the model, not the account, so burning
			// the pool re-discovers the same verdict once per key. A
			// combo falls through to the next leg; a direct route
			// surfaces an honest 503 whose Retry-After names the bench
			// expiry (mirrors the disabled-provider contract above and
			// DefaultPoolEmptyError's recovery hint).
			lastErr = &types.APIError{Status: 503, Type: "provider_model_benched",
				RetryAfter: strconv.FormatInt(int64(time.Until(ready).Seconds())+1, 10),
				Message: fmt.Sprintf("provider %s: model %s benched until %s",
					t.Provider, t.Model, ready.Format(time.RFC3339))}
			continue
		}
		benched := 0 // gated 403s rotated this target (each benches one account)
		// cause remembers why this target's pool drained: the last real
		// upstream answer before pool-empty. A pool emptied by per-model
		// 403s (Zhipu model_access_denied, live 2026-09-09) is NOT a rate
		// limit, and the default pool-empty 429 message would lie ("all
		// accounts rate-limited"). The pool-empty error still governs
		// status and Retry-After (the #48 contract tests pin them); only
		// the MESSAGE names the actual last upstream cause.
		var cause *types.APIError
		for attempt := 0; attempt < max(1, r.MaxAttempts) || benched > 0; {
			acct, poolReady := def.NextAccount(id)
			if acct == nil {
				// Whole account pool cooling from upstream 429s or
				// premium-gating 403s: an upstream call now is a doomed
				// ~1s attempt that only digs the limit deeper. Fall
				// through to the next combo target immediately; as the
				// last target it becomes the configured pool-empty error
				// (default: 429 whose Retry-After tells the client when
				// the pool reopens).
				pe := r.poolEmptyError(def, poolReady)
				// Rewrite only the DEFAULT generic message ("all accounts
				// rate-limited"), which can lie about the drain cause: a
				// custom pool-empty error (e.g. the #80 unfunded answer)
				// already names its own truth, and overwriting it would
				// discard the specific remedy the message carries.
				if cause != nil && pe.Type == "provider_rate_limited" {
					pe.Message = fmt.Sprintf("provider %s: all accounts benched after upstream %d (%s); retry after %ss",
						def.Name, cause.Status, cause.Type, pe.RetryAfter)
				}
				lastErr = pe
				break
			}
			out, err := call(ctx, def, acct, t.Model)
			if err == nil {
				onResult(out)
				// Sticky round-robin bookkeeping (#82): a success extends
				// the leader's run, and a spent run advances the counter
				// past the target that just served.
				r.recordRRSuccess(res, t)
				return nil
			}
			lastErr = err
			cause = err
			def.Unpin(id) // a failed attempt must not keep its pin
			if !(err.Retryable() || err.RegionLocked() || err.Fallbackable) {
				if err.Status == 404 && err.ModelScoped() {
					// Catalog-level verdict (tokenharbor 2026-09-10:
					// deepseek-v4.1-flash left their live catalog mid-day —
					// every key of the pool re-discovers the same 404): Do
					// already benched the (provider, model) pair, so rotating
					// accounts or retrying can only re-burn the pool. The
					// refusal indicts THIS target only — fall through to the
					// next combo leg; a direct route ends the chain here and
					// surfaces the upstream 404 honestly. The 403
					// model_access family deliberately stays on the
					// Fallbackable rotation path below: a sibling account can
					// still serve the same model (#48).
					break
				}
				if err.ReasoningEchoRequired() {
					// DeepSeek thinking-mode echo refusal (2026-09-10
					// tokenharbor seqs 2455/2621/2812; 2026-09-11 opencode
					// seqs 198/2666). attempt() grants ONE Fallbackable
					// retry when the refusal newly teaches the model the
					// echo contract — that retry carries synthesized
					// reasoning_content, new information the first attempt
					// lacked. Reaching this break means the contract was
					// already known (config glob or earlier learn) and the
					// filled body STILL refused: the body is byte-
					// deterministic again, same-target retries can only
					// re-burn the pool, and the sibling combo legs serve the
					// same model family — fall through like the 404 verdict
					// above; a direct route surfaces the 400 honestly.
					break
				}
				if err.ContextWindowExceeded() {
					// Context-window overflow (live 2026-09-12, `dev` combo: a
					// 283,915-token advisor request reached z-ai/glm-5.3-free
					// and got 400 "The input (283915 tokens) is longer than
					// the model's context length (262144 tokens)"). The body is
					// deterministic and EVERY account of this provider holds
					// the same window, so a same-target retry or an account
					// rotation only re-uploads ~1 MiB for the same verdict.
					// Sibling combo legs with LARGER windows are the correct
					// next hop (same-day ring proof that they exist and serve
					// this size: kilocode/kilo-auto/free 200 at 271,204 input
					// tokens, opencode/deepseek-v4.1-flash 200 at 283,064).
					// Deliberately NOT a model bench: this model serves every
					// smaller body fine, and benching it would exile a healthy
					// leg for one oversized request. A direct route (no next
					// target) still surfaces the 400 honestly.
					break
				}
				return err
			}
			if err.Fallbackable && err.Status == 403 {
				// Gated account (issue #48): Do benched it on the
				// ladder, so rotation is bounded by the POOL — every
				// hit benches exactly one account, so the next pick is
				// a different one and a fully benched pool surfaces as
				// pool-empty instead of a raw 403 the client would
				// treat as terminal. Spending the retry budget here
				// would leak the 403 while a healthy key still waits
				// in the pool. The +len(Accounts) guard is belt-and-
				// braces against a Fallbackable 403 that never benches.
				if benched++; benched > len(def.Accounts) {
					break
				}
				select {
				case <-ctx.Done():
					return &types.APIError{Status: 499, Type: "client_closed", Message: ctx.Err().Error()}
				default:
				}
				continue
			}
			if err.NoSameTargetRetry || err.SharedConcurrency() {
				// NoSameTargetRetry: pre-first-byte budget spent (the
				// gateway's own ResponseHeaderTimeout — e.g. a slow
				// aggregator queue): the request's pre-first-byte
				// demand is fixed, so a second attempt can only burn
				// a second full budget. Fall through to the next combo
				// target now; a direct route (no next target) surfaces
				// the 504 as-is.
				//
				// SharedConcurrency: the 429 is the upstream's
				// model-wide wall shared by ALL of the provider's
				// traffic — a different key hits the same wall, so
				// rotating accounts is pointless, and a 1s in-target
				// backoff only pays when the window happens to clear.
				// A different MODEL (the next combo target) sits in a
				// different concurrency bucket, so fall through
				// immediately instead of retrying the same saturated
				// model. Direct routes surface the wall with an
				// honest Retry-After (2s, below).
				break
			}
			attempt++
			if err.RegionLocked() || err.Fallbackable {
				// next attempt: pool skips the parked account, or the
				// attempt now coerces upfront (learned always-thinking).
				// A Fallbackable error retries this target once — the
				// rewritten body has a chance to pass — then falls
				// through to the next combo target via MaxAttempts.
				continue
			}
			select {
			case <-ctx.Done():
				return &types.APIError{Status: 499, Type: "client_closed", Message: ctx.Err().Error()}
			case <-time.After(Backoff(attempt-1, err)):
			}
		}
	}
	if lastErr == nil {
		lastErr = &types.APIError{Status: 502, Type: "no_route", Message: "no route succeeded"}
	}
	if (lastErr.OverQuota() || lastErr.SharedConcurrency()) && lastErr.RetryAfter == "" {
		// The error is about to reach the client (mid-chain errors are
		// replaced by later targets' results, so stamping the final one
		// cannot leak a stale hint onto a successful response): give the
		// client's SDK an honest backoff instead of instant-failing.
		// Shared walls (model-wide concurrency AND engine admission
		// walls, including their 503 variants) self-clear in seconds;
		// unknown upstream 429s get the default suggestion.
		if lastErr.SharedConcurrency() {
			lastErr.RetryAfter = "2"
		} else if w := lastErr.RateWindow(); w > 0 {
			// The 429 body named its request-count window ("Maximum 8
			// requests within 1 minutes" — live tokenrouter 2026-09-09):
			// the honest hint beats the generic 10s, which sends the
			// client straight back into the still-closed window.
			lastErr.RetryAfter = strconv.Itoa(int((w + time.Second - 1) / time.Second))
		} else {
			lastErr.RetryAfter = "10"
		}
	}
	return lastErr
}

// Caller executes one attempt against a provider/account. Decoupled so the
// server can inject its request pipeline (translation, saver, usage).
type Caller func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError)

// Backoff returns how long to wait before the next attempt at the same
// target after a retryable failure. Shared model-wide concurrency
// windows (Tencent GLM resellers) get 1s steps: rotating keys is
// pointless — every key hits the same upstream wall — so the window is
// waited out. Ordinary quota/rate errors use the fast 250ms tier (the
// cooldown ladder does the waiting), everything else 100ms.
func Backoff(attempt int, err *types.APIError) time.Duration {
	if err.SharedConcurrency() {
		return time.Duration(attempt+1) * time.Second
	}
	if err.OverQuota() {
		return time.Duration(attempt+1) * 250 * time.Millisecond
	}
	return time.Duration(attempt+1) * 100 * time.Millisecond
}

// ---------------------------------------------------------------------------
// Sticky round-robin (combo strategy = "round-robin", #82)
// ---------------------------------------------------------------------------

// comboRR is one round-robin combo's rotation state.
type comboRR struct {
	counter int // index of the next target to lead
	winner  int // index of the target currently serving its sticky run, -1 = none
	run     int // consecutive successes on the winner
}

// rrDefaultLimit is the sticky run when a combo sets none (mirrors
// OmniRoute's stickyRoundRobinLimit default of 3).
const rrDefaultLimit = 3

func (c *Combo) rrLimit() int {
	if c.RoundRobinLimit > 0 {
		return min(c.RoundRobinLimit, 1000)
	}
	return rrDefaultLimit
}

// rotateRoundRobin moves the combo's next designated leader to the front
// (#82). Rotation rewrites the Targets slice (a per-resolution copy — the
// config's Combo is never touched) so Execute's existing ordered loop is the
// whole implementation.
func (r *Router) rotateRoundRobin(res *Resolution) {
	c := r.combos[strings.ToLower(res.Combo)]
	if c == nil || len(res.Targets) < 2 {
		return
	}
	r.rrMu.Lock()
	st := r.rr[c.Name]
	if st == nil {
		st = &comboRR{counter: 0, winner: -1}
		r.rr[c.Name] = st
	}
	limit := c.rrLimit()
	lead := -1
	if st.winner >= 0 && st.run < limit && st.winner < len(res.Targets) {
		lead = st.winner // sticky: keep serving the target mid-run
	} else {
		lead = st.counter % len(res.Targets)
	}
	// Remember which target leads so recordRRSuccess can count it even if
	// rotation reordered the slice.
	res.rrLead = lead
	r.rrMu.Unlock()
	if lead <= 0 {
		return
	}
	rot := append([]Target(nil), res.Targets[lead:]...)
	rot = append(rot, res.Targets[:lead]...)
	res.Targets = rot
	// The sticky winner index moves with the rotation: after this reorder the
	// winner is index 0; the counter's next position is tracked separately
	// (rrLead), so no index fixups are needed beyond this request.
}

// recordRRSuccess is called by Execute after a target served a request on a
// round-robin combo: the same leader continues while its run is under the
// limit, and once the run completes the counter advances past it (mirrors
// OmniRoute's rrStickyTargets: successCount >= limit → counter = served+1,
// clear sticky).
func (r *Router) recordRRSuccess(res *Resolution, served Target) {
	if res == nil || !res.IsCombo || !res.RoundRobin || res.Combo == "" {
		return
	}
	c := r.combos[strings.ToLower(res.Combo)]
	if c == nil || len(res.Targets) < 2 {
		return
	}
	idx := -1
	for i, t := range res.Targets {
		if t.Provider == served.Provider && t.Model == served.Model {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	r.rrMu.Lock()
	defer r.rrMu.Unlock()
	st := r.rr[c.Name]
	if st == nil {
		return
	}
	limit := c.rrLimit()
	// Rotation is a pure offset: rotated index j is original index
	// (j + rrLead) % n. st.winner and st.counter live in ORIGINAL
	// coordinates — the same coordinates rotateRoundRobin reads — so a
	// fallback leg that served must be translated before it is recorded.
	// Skipping that translation pins stickiness to the leg that FAILED and
	// advances the counter past the wrong target: the next request re-offers
	// the known-dead leader, which is the regression sticky-RR exists to
	// avoid.
	n := len(res.Targets)
	orig := idx
	if res.rrLead > 0 {
		orig = (idx + res.rrLead) % n
	}
	// A run continues only when the SAME original target served again while
	// it was the leader (rotated index 0); any other leg starts a new run.
	if idx == 0 && st.winner == orig {
		st.run++
	} else {
		st.winner, st.run = orig, 1
	}
	if st.run >= limit {
		st.counter = orig + 1
		st.winner, st.run = -1, 0
	}
}
