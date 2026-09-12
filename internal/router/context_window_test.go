package router

import (
	"context"
	"strings"
	"testing"

	"onegw/internal/provider"
	"onegw/internal/types"
)

// Live incident 2026-09-12 (`dev` combo): a 283,915-token advisor request
// reached a 262,144-token leg first, the upstream answered 400, and
// Router.Execute surfaced it terminally — killing the client turn while
// larger-window legs (kilocode 271K, opencode 283K) sat unused further down
// the chain. The overflow indicts THIS target's window for THIS body: the
// combo must fall through on the FIRST 400 without rotating the pool or
// benching the model.
func TestExecuteFallsThroughOnContextWindowOverflow(t *testing.T) {
	const live400 = "The input (283915 tokens) is longer than the model's context length (262144 tokens)."

	p := provider.NewPool()
	p.Set(&provider.Def{
		Name: "smallwin", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "k1", APIKey: "k1"}, {Name: "k2", APIKey: "k2"}},
	})
	p.Set(&provider.Def{
		Name: "bigwin", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "go", APIKey: "k3"}},
	})
	r := New(p)
	r.SetCombos([]*Combo{{
		Name: "dev",
		Targets: []Target{
			{Provider: "smallwin", Model: "glm-5.3-free"},
			{Provider: "bigwin", Model: "deepseek-v4.1-flash"},
		},
	}})

	var smallAttempts int
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		if def.Name == "smallwin" {
			smallAttempts++
			return nil, &types.APIError{Status: 400, Type: "BadRequestError", Message: live400}
		}
		return "ok", nil
	}

	res, err := r.Resolve("dev")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := r.Execute(context.Background(), res, caller, func(a any) {}); got != nil {
		t.Fatalf("combo must serve via the larger-window leg, got %v", got)
	}
	if smallAttempts != 1 {
		t.Fatalf("smallwin attempts=%d, want 1 — an oversized body must not rotate the account pool", smallAttempts)
	}

	// No bench: the same target must still be offered to a SMALLER request
	// (benching a model over one oversized body would exile a healthy leg).
	var servedFromSmall bool
	smallCaller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		servedFromSmall = true
		return "ok", nil
	}
	resSmall, _ := r.Resolve("smallwin/glm-5.3-free")
	if got := r.Execute(context.Background(), resSmall, smallCaller, func(a any) {}); got != nil || !servedFromSmall {
		t.Fatalf("oversized 400 must NOT bench the model: got %v, served=%v", got, servedFromSmall)
	}

	// Direct route (single target): no next hop exists, so the honest upstream
	// 400 surfaces instead of a fabricated success.
	resD, _ := r.Resolve("smallwin/glm-5.3-free")
	direct := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		return nil, &types.APIError{Status: 400, Type: "BadRequestError", Message: live400}
	}
	got := r.Execute(context.Background(), resD, direct, func(a any) {})
	if got == nil || got.Status != 400 || !strings.Contains(got.Message, "longer than the model's context length") {
		t.Fatalf("direct oversized route: got %v, want the upstream 400", got)
	}
}
