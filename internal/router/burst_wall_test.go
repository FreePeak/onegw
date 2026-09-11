package router

// The behavioural half of the burst-wall fix (live 2026-09-11,
// b-ai/qwen3.8-flash, ring seqs 5960-6000): an empty-body 429 carries no
// wording, so provider.Do marks it SharedWall after cross-account burst
// detection. Execute must treat it exactly like the text-matched shared
// walls — fall through to the next combo leg immediately (the user's ask:
// switch model right away instead of rotating keys into the same wall) —
// and a parked (provider, model) pair must be skipped with ZERO upstream
// attempts while the burst window runs.

import (
	"context"
	"testing"
	"time"

	"onegw/internal/provider"
	"onegw/internal/types"
)

func TestExecuteSharedWallFlagFallsThroughImmediately(t *testing.T) {
	r := New(newTestPool())
	r.SetCombos([]*Combo{{
		Name: "stack",
		Targets: []Target{
			{Provider: "p1", Model: "m1"},
			{Provider: "p2", Model: "m2"},
		},
	}})
	res, _ := r.Resolve("stack")
	calls := map[string]int{}
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		calls[def.Name]++
		if def.Name == "p1" {
			// The empty-body one-api shape: nothing to text-match, only
			// the behavioural verdict.
			return nil, &types.APIError{Status: 429, Type: "upstream_empty_body",
				Message:    "upstream p1 returned HTTP 429 with an empty error body",
				SharedWall: true}
		}
		return "ok", nil
	}
	start := time.Now()
	if got := r.Execute(context.Background(), res, caller, func(a any) {}); got != nil {
		t.Fatalf("expected success via the next combo target, got %v", got)
	}
	if calls["p1"] != 1 || calls["p2"] != 1 {
		t.Fatalf("SharedWall must skip the same-target key rotation: %v", calls)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("fall-through must not wait out a backoff, took %v", time.Since(start))
	}
}

func TestExecuteSharedWallDirectRouteSurfacesWithRetryAfter(t *testing.T) {
	r := New(newTestPool())
	res, _ := r.Resolve("p1/m1")
	calls := 0
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		calls++
		return nil, &types.APIError{Status: 429, Type: "upstream_empty_body", SharedWall: true}
	}
	got := r.Execute(context.Background(), res, caller, func(a any) {})
	if got == nil || got.Status != 429 {
		t.Fatalf("direct route must surface the burst 429, got %+v", got)
	}
	if got.RetryAfter != "2" {
		t.Fatalf("shared-wall direct route must stamp the 2s hint, got %q", got.RetryAfter)
	}
	if calls != 1 {
		t.Fatalf("one attempt only, got %d", calls)
	}
}

func TestExecuteParkedModelSkippedZeroAttempts(t *testing.T) {
	p := newTestPool()
	r := New(p)
	def, _ := p.Get("p1")
	def.BenchModel("m1", 6*time.Second)
	r.SetCombos([]*Combo{{
		Name: "stack",
		Targets: []Target{
			{Provider: "p1", Model: "m1"},
			{Provider: "p2", Model: "m2"},
		},
	}})
	res, _ := r.Resolve("stack")
	calls := map[string]int{}
	caller := func(ctx context.Context, d *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		calls[d.Name]++
		return "ok", nil
	}
	if got := r.Execute(context.Background(), res, caller, func(a any) {}); got != nil {
		t.Fatalf("parked leg must fall through to the healthy leg, got %v", got)
	}
	if calls["p1"] != 0 || calls["p2"] != 1 {
		t.Fatalf("parked pair must be skipped with zero attempts: %v", calls)
	}
}
