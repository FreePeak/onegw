package router

import (
	"context"
	"errors"
	"testing"

	"onegw/internal/provider"
	"onegw/internal/types"
)

func newTestPool() *provider.Pool {
	p := provider.NewPool()
	p.Set(&provider.Def{
		Name: "p1", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "a", APIKey: "k1"}, {Name: "b", APIKey: "k2"}},
	})
	p.Set(&provider.Def{
		Name: "p2", Kind: provider.KindAnthropic,
		Accounts: []provider.Account{{Name: "c", APIKey: "k3"}},
	})
	return p
}

func TestResolveDirectAndCombo(t *testing.T) {
	r := New(newTestPool())
	r.SetModels([]string{"p1/gpt-x"})
	r.SetCombos([]*Combo{{
		Name: "stack",
		Targets: []Target{
			{Provider: "p1", Model: "gpt-x"},
			{Provider: "p2", Model: "claude-y"},
		},
	}})

	res, err := r.Resolve("p1/gpt-x")
	if err != nil || len(res.Targets) != 1 || res.Targets[0].Provider != "p1" {
		t.Fatalf("direct resolve wrong: %v %v", res, err)
	}
	res, err = r.Resolve("stack")
	if err != nil || !res.IsCombo || len(res.Targets) != 2 {
		t.Fatalf("combo resolve wrong: %v %v", res, err)
	}
	if res.Targets[1].Model != "claude-y" {
		t.Fatalf("combo order wrong: %+v", res.Targets)
	}
	if _, err = r.Resolve("nope/zzz"); err == nil || err.Status != 404 {
		t.Fatalf("unknown provider should 404: %v", err)
	}
}

func TestExecuteFallbackOnQuota(t *testing.T) {
	r := New(newTestPool())
	r.SetCombos([]*Combo{{
		Name: "stack",
		Targets: []Target{
			{Provider: "p1", Model: "m1"},
			{Provider: "p2", Model: "m2"},
		},
	}})
	res, _ := r.Resolve("stack")

	calls := 0
	var servedModel string
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		calls++
		if def.Name == "p1" {
			return nil, &types.APIError{Status: 429, Type: "rate_limit_error", Message: "quota"}
		}
		servedModel = model
		return "ok", nil
	}
	got := r.Execute(context.Background(), res, caller, func(a any) {})
	if got != nil {
		t.Fatalf("expected success via fallback, got %v", got)
	}
	if calls != 3 || servedModel != "m2" { // 2 attempts p1 (429 retryable) + 1 p2
		t.Fatalf("calls=%d served=%s", calls, servedModel)
	}
}

func TestExecuteNoRetryOnBadRequest(t *testing.T) {
	r := New(newTestPool())
	r.SetCombos([]*Combo{{
		Name: "stack",
		Targets: []Target{
			{Provider: "p1", Model: "m1"},
			{Provider: "p2", Model: "m2"},
		},
	}})
	res, _ := r.Resolve("stack")
	calls := 0
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		calls++
		return nil, &types.APIError{Status: 400, Type: "invalid_request", Message: "bad"}
	}
	err := r.Execute(context.Background(), res, caller, func(a any) {})
	if err == nil || err.Status != 400 {
		t.Fatalf("expected 400 passthrough: %v", err)
	}
	if calls != 1 {
		t.Fatalf("400 must not retry or fallback, calls=%d", calls)
	}
}

func TestExecuteErrorWhenAllFail(t *testing.T) {
	r := New(newTestPool())
	res, _ := r.Resolve("p1/m")
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		return nil, &types.APIError{Status: 503, Type: "api_error", Message: "down"}
	}
	err := r.Execute(context.Background(), res, caller, func(a any) {})
	if err == nil || err.Status != 503 {
		t.Fatalf("expected last error: %v", err)
	}
	_ = errors.New
}
