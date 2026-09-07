// Package router resolves client model strings to concrete provider/account
// targets and drives fallback across ordered combo targets.
package router

import (
	"context"
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
	mu     sync.RWMutex
	pool   *provider.Pool
	models map[string]directRoute // "provider/model" passthrough
	combos map[string]*Combo
	// alias chains: chased iteratively inside Resolve under RLock.
	aliases map[string]string
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

// Execute runs the resolution: for each target pick an account and call; on
// retryable failure try again, then fall through to the next target.
// onResult receives the successful result.
func (r *Router) Execute(ctx context.Context, res *Resolution, call Caller, onResult func(any)) *types.APIError {
	var lastErr *types.APIError
	for _, t := range res.Targets {
		def, ok := r.pool.Get(t.Provider)
		if !ok {
			lastErr = &types.APIError{Status: 404, Type: "unknown_provider", Message: "unknown provider " + t.Provider}
			continue
		}
		for attempt := 0; attempt < max(1, r.MaxAttempts); attempt++ {
			acct := def.NextAccount()
			out, err := call(ctx, def, acct, t.Model)
			if err == nil {
				onResult(out)
				return nil
			}
			lastErr = err
			if !(err.Retryable() || err.RegionLocked()) {
				return err
			}
			if err.RegionLocked() {
				continue // next attempt: pool skips the parked account
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
