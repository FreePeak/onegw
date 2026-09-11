package router

import (
	"context"
	"testing"
	"time"

	"onegw/internal/provider"
	"onegw/internal/types"
)

// Live 2026-09-11 06:14Z (seq 595): b-ai's distributor answered 503 "No
// available channel for model glm-5.3-flash under group default
// (distributor)" — the lane is dead for every key. Today each request
// burned the same-target retry ladder into the empty channel pool before
// the combo's next leg served, and the wait produced client cancels (seq
// 574, 499 client_closed). The shared-wall classification must make the
// first failed call the LAST on that leg: fall through immediately.
func TestExecuteChannelEmpty503FallsThroughAfterOneCall(t *testing.T) {
	r := New(newTestPool())
	r.SetCombos([]*Combo{{
		Name: "dev",
		Targets: []Target{
			{Provider: "p1", Model: "m1"},
			{Provider: "p2", Model: "m2"},
		},
	}})
	res, _ := r.Resolve("dev")
	calls := map[string]int{}
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		calls[def.Name]++
		if def.Name == "p1" {
			return nil, &types.APIError{Status: 503, Type: "api_error",
				Message: "No available channel for model m1 under group default (distributor)"}
		}
		return "ok", nil
	}
	start := time.Now()
	if got := r.Execute(context.Background(), res, caller, func(a any) {}); got != nil {
		t.Fatalf("expected success via the next combo target, got %v", got)
	}
	if calls["p1"] != 1 || calls["p2"] != 1 {
		t.Fatalf("dead lane must get exactly one doomed call, then fall through: %v", calls)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("fall-through must not wait out a backoff on the dead lane, took %v", time.Since(start))
	}
}

// A direct route has no next leg: the lane-empty 503 surfaces with the
// honest 2s Retry-After so the client's SDK backs off instead of
// instant-failing back into the dead channel pool.
func TestExecuteChannelEmpty503DirectRouteSurfacesWithRetryAfter(t *testing.T) {
	r := New(newTestPool())
	res, _ := r.Resolve("p1/m1")
	calls := 0
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		calls++
		return nil, &types.APIError{Status: 503, Type: "api_error",
			Message: "No available channel for model m1 under group default (distributor)"}
	}
	got := r.Execute(context.Background(), res, caller, func(a any) {})
	if got == nil || got.Status != 503 || !got.SharedConcurrency() {
		t.Fatalf("direct route must surface the lane-empty 503, got %+v", got)
	}
	if got.RetryAfter != "2" {
		t.Fatalf("Retry-After = %q, want 2", got.RetryAfter)
	}
	if calls != 1 {
		t.Fatalf("dead lane must not burn retries, calls=%d", calls)
	}
}
