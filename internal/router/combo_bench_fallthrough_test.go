package router

import (
	"context"
	"testing"
	"time"

	"onegw/internal/provider"
	"onegw/internal/types"
)

// comboBenchPool builds p1 (first leg, model m1 benched by the caller) + p2,
// over a two-target combo — the live shape of `execution`.
func comboBenchPool() *provider.Pool {
	p := provider.NewPool()
	p.Set(&provider.Def{
		Name: "p1", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "a", APIKey: "k1"}},
	})
	p.Set(&provider.Def{
		Name: "p2", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "b", APIKey: "k2"}},
	})
	return p
}

func comboBenchRouter(t *testing.T, fallback bool) (*Router, *Resolution, map[string]int) {
	t.Helper()
	r := New(comboBenchPool())
	r.SetComboFallback(fallback)
	r.SetCombos([]*Combo{{
		Name: "stack",
		Targets: []Target{
			{Provider: "p1", Model: "m1"},
			{Provider: "p2", Model: "m2"},
		},
	}})
	res, err := r.Resolve("stack")
	if err != nil {
		t.Fatalf("resolve stack: %v", err)
	}
	return r, res, map[string]int{}
}

// A model bench on the FIRST leg of a combo is a transient verdict on the
// model the client asked for, not an operator config skip: with
// combo_fallback off the router must surface the bench's own 503 (with the
// Retry-After that names the expiry) instead of letting a sibling target
// answer under the client's requested model name.
//
// Live 2026-10-13: `execution` → opencode/step-5-preview-free, benched on a
// 429 burst, was answered by opencode-free/big-pickle (403 FreeTierError)
// with X-OneGW-Decision fallback=true — a model switch nobody asked for.
func TestExecuteComboFallbackOffStopsOnBenchedFirstLeg(t *testing.T) {
	r, res, calls := comboBenchRouter(t, false)
	def, _ := r.pool.Get("p1")
	def.BenchModel("m1", time.Minute)

	got := r.Execute(context.Background(), res, func(ctx context.Context, d *provider.Def, a *provider.Account, m string) (any, *types.APIError) {
		calls[d.Name]++
		return "ok", nil
	}, func(a any) {})

	if got == nil || got.Status != 503 || got.Type != "provider_model_benched" {
		t.Fatalf("want 503 provider_model_benched, got %v", got)
	}
	if got.RetryAfter == "" {
		t.Fatal("Retry-After missing on the benched-first-leg verdict")
	}
	if calls["p2"] != 0 {
		t.Fatalf("a benched first leg must not hand the request to a sibling model: %v", calls)
	}
	if calls["p1"] != 0 {
		t.Fatalf("a benched leg must cost zero upstream attempts: %v", calls)
	}
}

// combo_fallback on keeps master's fall-through: the bench is skipped and the
// sibling leg serves.
func TestExecuteComboFallbackOnFallsThroughBenchedFirstLeg(t *testing.T) {
	r, res, calls := comboBenchRouter(t, true)
	def, _ := r.pool.Get("p1")
	def.BenchModel("m1", time.Minute)

	if got := r.Execute(context.Background(), res, func(ctx context.Context, d *provider.Def, a *provider.Account, m string) (any, *types.APIError) {
		calls[d.Name]++
		return "ok", nil
	}, func(a any) {}); got != nil {
		t.Fatalf("combo_fallback on should fall through to p2, got %v", got)
	}
	if calls["p2"] != 1 {
		t.Fatalf("combo_fallback on must try the sibling leg: %v", calls)
	}
}

// A DIRECT route to a benched model is unchanged: honest 503, no attempts.
func TestExecuteDirectBenchedModelStill503(t *testing.T) {
	r, _, calls := comboBenchRouter(t, false)
	def, _ := r.pool.Get("p1")
	def.BenchModel("m1", time.Minute)
	res, err := r.Resolve("p1/m1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := r.Execute(context.Background(), res, func(ctx context.Context, d *provider.Def, a *provider.Account, m string) (any, *types.APIError) {
		calls[d.Name]++
		return "ok", nil
	}, func(a any) {})
	if got == nil || got.Status != 503 || got.Type != "provider_model_benched" {
		t.Fatalf("direct benched route: got %v", got)
	}
	if calls["p1"] != 0 {
		t.Fatalf("benched direct route must cost zero attempts: %v", calls)
	}
}

// A DISABLED first leg still skips to the next live target with
// combo_fallback off (operator config, not an upstream verdict) — the
// contract TestExecuteComboFallbackOffSkipsDisabledThenStops pins. Kept
// beside the bench case because the two are one line apart in Execute and
// must not converge.
func TestExecuteComboFallbackOffStillSkipsDisabledFirstLeg(t *testing.T) {
	p := provider.NewPool()
	p.Set(&provider.Def{
		Name: "p1", Kind: provider.KindOpenAI, Disabled: true,
		Accounts: []provider.Account{{Name: "a", APIKey: "k1"}},
	})
	p.Set(&provider.Def{
		Name: "p2", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "b", APIKey: "k2"}},
	})
	r := New(p)
	r.SetComboFallback(false)
	r.SetCombos([]*Combo{{
		Name: "stack",
		Targets: []Target{
			{Provider: "p1", Model: "m1"},
			{Provider: "p2", Model: "m2"},
		},
	}})
	res, _ := r.Resolve("stack")
	calls := map[string]int{}
	if got := r.Execute(context.Background(), res, func(ctx context.Context, d *provider.Def, a *provider.Account, m string) (any, *types.APIError) {
		calls[d.Name]++
		return "ok", nil
	}, func(a any) {}); got != nil {
		t.Fatalf("disabled first leg should skip to p2, got %v", got)
	}
	if calls["p2"] != 1 {
		t.Fatalf("disabled first leg must skip, not stop: %v", calls)
	}
}
