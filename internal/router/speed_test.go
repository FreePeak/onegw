package router

import (
	"context"
	"testing"
	"time"

	"onegw/internal/provider"
	"onegw/internal/types"
)

func TestExecuteSpeedOrder(t *testing.T) {
	p := provider.NewPool()
	slow := &provider.Def{Name: "slow", Kind: provider.KindOpenAI, Accounts: []provider.Account{{Name: "a", APIKey: "k1"}}}
	fast := &provider.Def{Name: "fast", Kind: provider.KindOpenAI, Accounts: []provider.Account{{Name: "b", APIKey: "k2"}}}
	p.Set(slow)
	p.Set(fast)
	// Seed speeds: slow = 10 tok/s, fast = 100 tok/s.
	slow.ObserveSpeed(nil, "m", 100, 10*time.Second)
	fast.ObserveSpeed(nil, "m", 100, time.Second)

	r := New(p)
	r.SetCombos([]*Combo{{
		Name: "s",
		Targets: []Target{
			{Provider: "slow", Model: "m"},
			{Provider: "fast", Model: "m"},
		},
		Strategy: "fastest",
	}})
	res, err := r.Resolve("s")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	execErr := r.Execute(context.Background(), res, func(ctx context.Context, def *provider.Def, acct *provider.Account, m string) (any, *types.APIError) {
		order = append(order, def.Name)
		return nil, nil
	}, func(any) {})
	if execErr != nil {
		t.Fatal(execErr)
	}
	if len(order) != 1 || order[0] != "fast" {
		t.Fatalf("fastest leg must serve first: order=%v", order)
	}

	// Without the strategy the configured order stands even with the same
	// speed data — the fallback sequence is an explicit contract.
	r2 := New(p)
	r2.SetCombos([]*Combo{{
		Name: "s",
		Targets: []Target{
			{Provider: "slow", Model: "m"},
			{Provider: "fast", Model: "m"},
		},
	}})
	res2, _ := r2.Resolve("s")
	order = nil
	_ = r2.Execute(context.Background(), res2, func(ctx context.Context, def *provider.Def, acct *provider.Account, m string) (any, *types.APIError) {
		order = append(order, def.Name)
		return nil, nil
	}, func(any) {})
	if len(order) != 1 || order[0] != "slow" {
		t.Fatalf("default strategy must keep configured order: order=%v", order)
	}
}

// No speed data anywhere → "fastest" behaves exactly like the configured
// order (all legs tie at zero).
func TestExecuteSpeedOrderNoData(t *testing.T) {
	p := provider.NewPool()
	p.Set(&provider.Def{Name: "p1", Kind: provider.KindOpenAI, Accounts: []provider.Account{{Name: "a", APIKey: "k1"}}})
	p.Set(&provider.Def{Name: "p2", Kind: provider.KindOpenAI, Accounts: []provider.Account{{Name: "b", APIKey: "k2"}}})
	r := New(p)
	r.SetCombos([]*Combo{{
		Name: "s",
		Targets: []Target{
			{Provider: "p1", Model: "m"},
			{Provider: "p2", Model: "m"},
		},
		Strategy: "fastest",
	}})
	res, _ := r.Resolve("s")
	var order []string
	_ = r.Execute(context.Background(), res, func(ctx context.Context, def *provider.Def, acct *provider.Account, m string) (any, *types.APIError) {
		order = append(order, def.Name)
		return nil, nil
	}, func(any) {})
	if len(order) != 1 || order[0] != "p1" {
		t.Fatalf("no-data pool must keep configured order: order=%v", order)
	}
}
