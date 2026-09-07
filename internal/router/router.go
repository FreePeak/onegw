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
	// maxAttempts per target before falling to next (network/5xx).
	MaxAttempts int
}

type directRoute struct {
	provider string
	model    string
}

func New(pool *provider.Pool) *Router {
	return &Router{
		pool:        pool,
		models:      map[string]directRoute{},
		combos:      map[string]*Combo{},
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
	if c, ok := r.combos[strings.ToLower(model)]; ok {
		return &Resolution{Targets: append([]Target(nil), c.Targets...), IsCombo: true}, nil
	}
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
			if !err.Retryable() {
				break
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
