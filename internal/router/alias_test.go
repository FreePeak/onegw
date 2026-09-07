package router

import (
	"testing"
	"time"

	"onegw/internal/provider"
	"onegw/internal/types"
)

func newTestRouter() *Router {
	p := provider.NewPool()
	p.Set(&provider.Def{Name: "p1", Kind: provider.KindOpenAI, BaseURL: "https://p1.example", Accounts: []provider.Account{{Name: "default", APIKey: "k"}}})
	p.Set(&provider.Def{Name: "p2", Kind: provider.KindOpenAI, BaseURL: "https://p2.example", Accounts: []provider.Account{{Name: "default", APIKey: "k"}}})
	r := New(p)
	r.SetModels([]string{"p1/m1", "p2/m2"})
	r.SetCombos([]*Combo{{Name: "pair", Targets: []Target{{Provider: "p1", Model: "m1"}, {Provider: "p2", Model: "m2"}}}})
	return r
}

func TestAliasToProviderModel(t *testing.T) {
	r := newTestRouter()
	r.SetAliases(map[string]string{"fast": "p1/m1"})
	res, err := r.Resolve("fast")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.IsCombo || len(res.Targets) != 1 || res.Targets[0].Provider != "p1" || res.Targets[0].Model != "m1" {
		t.Fatalf("alias should resolve to p1/m1, got %+v", res)
	}
}

func TestAliasToCombo(t *testing.T) {
	r := newTestRouter()
	r.SetAliases(map[string]string{"best": "pair"})
	res, err := r.Resolve("best")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.IsCombo || len(res.Targets) != 2 {
		t.Fatalf("alias to combo should resolve as combo, got %+v", res)
	}
}

func TestAliasChain(t *testing.T) {
	r := newTestRouter()
	r.SetAliases(map[string]string{
		"a": "b",
		"b": "c",
		"c": "p2/m2",
	})
	res, err := r.Resolve("a")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Targets) != 1 || res.Targets[0].Provider != "p2" || res.Targets[0].Model != "m2" {
		t.Fatalf("chain a→b→c should resolve to p2/m2, got %+v", res)
	}
}

func TestAliasCaseInsensitive(t *testing.T) {
	r := newTestRouter()
	r.SetAliases(map[string]string{"Fast": "p1/m1"})
	res, err := r.Resolve("FAST")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Targets) != 1 || res.Targets[0].Provider != "p1" {
		t.Fatalf("alias lookup should be case-insensitive, got %+v", res)
	}
}

func TestAliasDoesNotShadowRealRoutes(t *testing.T) {
	r := newTestRouter()
	// Even if an alias pointed at itself, real provider/model wins because
	// SetAliases drops self-references and Resolve only consults aliases
	// when the name is not already provider/model or combo — provider/model
	// hit their tables first after alias translation.
	r.SetAliases(map[string]string{"pair": "p1/m1"})
	res, err := r.Resolve("pair")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.IsCombo {
		t.Fatalf("real combo must win over an alias with the same name, got %+v", res)
	}
}

func TestAliasUnknownNameFallsToBareModel(t *testing.T) {
	r := newTestRouter()
	r.SetAliases(map[string]string{"fast": "p1/m1"})
	// "nope" is not an alias, not provider/model, not a combo: the
	// bare-model pass-through serves it via the first provider in order.
	res, err := r.Resolve("nope")
	if err != nil {
		t.Fatalf("bare model should route pass-through, got %v", err)
	}
	if res.IsCombo || len(res.Targets) != 1 || res.Targets[0].Model != "nope" {
		t.Fatalf("bare-model fallback should keep the model string, got %+v", res)
	}
}

func TestAliasCycleDoesNotHang(t *testing.T) {
	r := newTestRouter()
	r.SetAliases(map[string]string{
		"x": "y",
		"y": "x",
	})
	// Validate rejects this upstream; Resolve must not hang or panic — it
	// bails after MaxAliasHops and falls through to the normal lookup,
	// where "x" is treated as a bare model (first provider in order).
	done := make(chan struct{})
	var res *Resolution
	var err *types.APIError
	go func() {
		res, err = r.Resolve("x")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cyclic alias resolution hung")
	}
	if err != nil {
		t.Fatalf("cycle should fall through to bare-model lookup, got %v", err)
	}
	if len(res.Targets) != 1 {
		t.Fatalf("bare-model fallback should return one target, got %+v", res)
	}
}

func TestAliasChainCapsAtMaxHops(t *testing.T) {
	r := newTestRouter()
	// Build a chain longer than MaxAliasHops ending in a valid route.
	m := map[string]string{}
	last := "p1/m1"
	for i := range MaxAliasHops + 3 {
		cur := "hop" + string(rune('a'+i))
		m[cur] = last
		last = cur
	}
	r.SetAliases(m)
	// An over-long chain (Validate would reject it; the router defends in
	// depth) exhausts the hop cap and falls through to the bare-model
	// pass-through: the ORIGINAL alias string becomes the model name, not
	// the chain's terminal target.
	res, err := r.Resolve(last)
	if err != nil {
		t.Fatalf("over-long chain should fall through, not error: %v", err)
	}
	if res.Targets[0].Model == "m1" {
		t.Fatal("over-long chain must NOT resolve to its terminal target — hop cap is not enforced")
	}
	if res.Targets[0].Model != last {
		t.Fatalf("over-long chain should fall back to the original name %q as bare model, got %q", last, res.Targets[0].Model)
	}
}

var _ = types.APIError{}
