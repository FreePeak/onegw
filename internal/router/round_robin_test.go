package router

import (
	"context"
	"testing"

	"onegw/internal/provider"
	"onegw/internal/types"
)

func rrCombo(limit int) *Router {
	r := New(newTestPool())
	r.SetCombos([]*Combo{{
		Name:            "rr",
		Strategy:        "round-robin",
		RoundRobinLimit: limit,
		Targets: []Target{
			{Provider: "p1", Model: "m1"},
			{Provider: "p2", Model: "m2"},
		},
	}})
	return r
}

// Sticky round-robin (#82): one leg leads for `limit` successes, then the
// counter advances — the whole point being that leg #1 stops absorbing every
// request (and every failure) until it is exhausted.
func TestStickyRoundRobinRotation(t *testing.T) {
	r := rrCombo(2)
	var order []string
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		order = append(order, def.Name)
		return "ok", nil
	}
	for i := 0; i < 6; i++ {
		res, err := r.Resolve("rr")
		if err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		if apiErr := r.Execute(context.Background(), res, caller, func(any) {}); apiErr != nil {
			t.Fatalf("execute %d: %v", i, apiErr)
		}
	}
	want := []string{"p1", "p1", "p2", "p2", "p1", "p1"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("rotation: got %v, want %v", order, want)
		}
	}
}

// Regression guard for the coordinate bug: when the leader FAILS and a later
// leg serves, stickiness must follow the SERVING leg — otherwise the next
// request re-offers the known-dead leader first, which is exactly what sticky
// rotation is supposed to stop.
func TestStickyRoundRobinFallbackLegBecomesLeader(t *testing.T) {
	r := rrCombo(2)
	r.MaxAttempts = 1 // one attempt per target, so a failure falls straight through
	var order []string
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		order = append(order, def.Name)
		if def.Name == "p1" {
			// Retryable 503 with MaxAttempts=1: one attempt, then the next leg.
			return nil, &types.APIError{Status: 503, Type: "upstream_unavailable", Message: "down"}
		}
		return "ok", nil
	}
	run := func(n int) {
		for i := 0; i < n; i++ {
			res, err := r.Resolve("rr")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if apiErr := r.Execute(context.Background(), res, caller, func(any) {}); apiErr != nil {
				t.Fatalf("execute: %v", apiErr)
			}
		}
	}

	run(1) // p1 leads and fails, p2 serves
	if len(order) != 2 || order[0] != "p1" || order[1] != "p2" {
		t.Fatalf("first request must try p1 then p2, got %v", order)
	}
	order = nil
	run(1) // sticky: p2 leads — p1 must NOT be tried again
	if len(order) != 1 || order[0] != "p2" {
		t.Fatalf("after p2 served, the next request must lead with p2 (failed leader must not stay sticky), got %v", order)
	}
	order = nil
	run(1) // p2's second success spends the run: the counter advances to p1 again
	if len(order) != 2 || order[0] != "p1" || order[1] != "p2" {
		t.Fatalf("a spent run must advance past the serving leg (p1 leads again), got %v", order)
	}
}

// Default limit is 3 when the combo sets none, and a single-target combo
// never rotates (rotation must not invent legs).
func TestRoundRobinLimitDefaultAndSingleTarget(t *testing.T) {
	r := rrCombo(0)
	var order []string
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		order = append(order, def.Name)
		return "ok", nil
	}
	for i := 0; i < 4; i++ {
		res, _ := r.Resolve("rr")
		if apiErr := r.Execute(context.Background(), res, caller, func(any) {}); apiErr != nil {
			t.Fatalf("execute: %v", apiErr)
		}
	}
	// limit 3: p1 p1 p1 p2
	if len(order) != 4 || order[0] != "p1" || order[3] != "p2" {
		t.Fatalf("default limit 3 rotation: got %v", order)
	}

	solo := New(newTestPool())
	solo.SetCombos([]*Combo{{Name: "solo", Strategy: "round-robin", Targets: []Target{{Provider: "p1", Model: "m1"}}}})
	for i := 0; i < 3; i++ {
		res, err := solo.Resolve("solo")
		if err != nil {
			t.Fatalf("resolve solo: %v", err)
		}
		if apiErr := solo.Execute(context.Background(), res, caller, func(any) {}); apiErr != nil {
			t.Fatalf("solo execute: %v", apiErr)
		}
	}
	if got := order[4:]; len(got) != 3 {
		t.Fatalf("single-target combo served %d times, want 3 (%v)", len(got), got)
	}
}
