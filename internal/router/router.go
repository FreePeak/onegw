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
	Name    string   `toml:"name"`
	Targets []Target `toml:"targets"`
}

// Router resolves model strings and executes calls with fallback.
type Router struct {
	mu      sync.RWMutex
	pool    *provider.Pool
	models  map[string]directRoute
	combos  map[string]*Combo
	aliases map[string]string

	// PoolEmptyError, when set, builds the error Execute reports when a
	// target's whole account pool is cooling and the upstream attempt is
	// skipped. The default (DefaultPoolEmptyError) answers 429
	// provider_rate_limited with the pool's soonest recovery as
	// Retry-After; the server overrides it to surface richer semantics
	// (quota-window exhaustion answers 503 provider_quota_exhausted with
	// the window end — that gate lives inside the Caller, which Execute
	// skips when it never picks an account).
	PoolEmptyError func(def *provider.Def, ready time.Time) *types.APIError

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
		_, exists := r.pool.Get(prov)
		return exists
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
		return &Resolution{Targets: append([]Target(nil), c.Targets...), IsCombo: true}, nil
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
	// Bare model: try providers in order that could serve it.
	for _, name := range r.pool.Names() {
		if _, ok := r.pool.Get(name); ok {
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

// IdentityFrom extracts the identity tagged by WithIdentity.
func IdentityFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(identityKey{}).(string)
	return id
}

// Execute runs the resolution: for each target pick an account and call; on
// retryable failure try again, then fall through to the next target.
// onResult receives the successful result.
func (r *Router) Execute(ctx context.Context, res *Resolution, call Caller, onResult func(any)) *types.APIError {
	var lastErr *types.APIError
	id := IdentityFrom(ctx)
	for _, t := range res.Targets {
		def, ok := r.pool.Get(t.Provider)
		if !ok {
			lastErr = &types.APIError{Status: 404, Type: "unknown_provider", Message: "unknown provider " + t.Provider}
			continue
		}
		for attempt := range max(1, r.MaxAttempts) {
			acct, poolReady := def.NextAccount(id)
			if acct == nil {
				// Whole account pool cooling from upstream 429s: an
				// upstream call now is a doomed ~1s attempt that only
				// digs the limit deeper. Fall through to the next combo
				// target immediately; as the last target it becomes the
				// configured pool-empty error (default: 429 whose
				// Retry-After tells the client when the pool reopens).
				lastErr = r.poolEmptyError(def, poolReady)
				break
			}
			out, err := call(ctx, def, acct, t.Model)
			if err == nil {
				onResult(out)
				return nil
			}
			lastErr = err
			def.Unpin(id) // a failed attempt must not keep its pin
			if !(err.Retryable() || err.RegionLocked() || err.Fallbackable) {
				return err
			}
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
			case <-time.After(backoff(attempt, err)):
			}
		}
	}
	if lastErr == nil {
		lastErr = &types.APIError{Status: 502, Type: "no_route", Message: "no route succeeded"}
	}
	return lastErr
}

// Caller executes one attempt against a provider/account. Decoupled so the
// server can inject its request pipeline (translation, saver, usage).
type Caller func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError)

func backoff(attempt int, err *types.APIError) time.Duration {
	if err.OverQuota() {
		return time.Duration(attempt+1) * 250 * time.Millisecond
	}
	return time.Duration(attempt+1) * 100 * time.Millisecond
}
