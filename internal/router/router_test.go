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

// A pre-first-byte budget exhaustion (the gateway's own ResponseHeaderTimeout,
// e.g. the 2026-09-09 tokenrouter glm-5.3-free free-lane stalls: two
// consecutive silent 120s waits before the client saw the 504) must not burn
// a second full budget on the same target. The router falls through to the
// next combo target immediately; a direct route surfaces the 504 after a
// single attempt.
func TestExecuteBudgetTimeoutSkipsSameTargetRetry(t *testing.T) {
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
			return nil, &types.APIError{
				Status: 504, Type: "upstream_timeout",
				Message:           `Post "https://x/v1/chat/completions": net/http: timeout awaiting response headers`,
				NoSameTargetRetry: true,
			}
		}
		return "ok", nil
	}
	if got := r.Execute(context.Background(), res, caller, func(a any) {}); got != nil {
		t.Fatalf("expected success via the next combo target, got %v", got)
	}
	if calls["p1"] != 1 || calls["p2"] != 1 {
		t.Fatalf("budget timeout must skip the same-target retry: %v", calls)
	}

	// Direct route (single target): one attempt, then the 504 surfaces.
	res, _ = r.Resolve("p1/m1")
	p1 := 0
	caller = func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		p1++
		return nil, &types.APIError{Status: 504, Type: "upstream_timeout", Message: "budget", NoSameTargetRetry: true}
	}
	if got := r.Execute(context.Background(), res, caller, func(a any) {}); got == nil || got.Status != 504 || got.Type != "upstream_timeout" {
		t.Fatalf("direct route must surface the budget-exhausted 504: %v", got)
	}
	if p1 != 1 {
		t.Fatalf("direct route must not retry a spent budget, calls=%d", p1)
	}
}

// An ordinary upstream 504 (no budget marker) keeps the historical
// MaxAttempts retry on the same target — the skip is reserved for the
// gateway's own header-budget aborts.
func TestExecutePlainTimeoutStillRetries(t *testing.T) {
	r := New(newTestPool())
	res, _ := r.Resolve("p1/m1")
	calls := 0
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		calls++
		return nil, &types.APIError{Status: 504, Type: "upstream_timeout", Message: "gateway timeout"}
	}
	if got := r.Execute(context.Background(), res, caller, func(a any) {}); got == nil || got.Status != 504 {
		t.Fatalf("expected the 504 to surface: %v", got)
	}
	if calls != 2 { // MaxAttempts
		t.Fatalf("plain 504 must keep the same-target retry, calls=%d", calls)
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

// A region-locked credential must not fail the request when another key of
// the same provider can serve it: the router retries the same target and
// the parked account is skipped by the pool.
func TestExecuteRetriesRegionLocked(t *testing.T) {
	r := New(newTestPool())
	res, _ := r.Resolve("p1/m1")
	calls := 0
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		calls++
		if calls == 1 {
			return nil, &types.APIError{Status: 403, Type: "RegionError", Message: "region"}
		}
		return "ok", nil
	}
	if err := r.Execute(context.Background(), res, caller, func(a any) {}); err != nil {
		t.Fatalf("region-locked first attempt must retry, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
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

func TestExecuteFallbackableRetriesThenFallsThrough(t *testing.T) {
	r := New(newTestPool())
	r.SetCombos([]*Combo{{
		Name:    "stack",
		Targets: []Target{{Provider: "p1", Model: "m"}, {Provider: "p2", Model: "m"}},
	}})
	res, _ := r.Resolve("stack")
	if res == nil {
		t.Fatal("resolve stack failed")
	}
	var calls [2]int
	var served string
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		i := 0
		if def.Name == "p2" {
			i = 1
		}
		calls[i]++
		if def.Name == "p1" {
			return nil, &types.APIError{Status: 400, Type: "invalid_request_error", Fallbackable: true,
				Message: "该模型始终思考，不支持关闭思考"}
		}
		served = model
		return "ok", nil
	}
	if err := r.Execute(context.Background(), res, caller, func(a any) {}); err != nil {
		t.Fatalf("expected fall-through success: %v", err)
	}
	if calls != [2]int{2, 1} || served != "m" {
		t.Fatalf("calls=%v served=%s, want [2 1] with p2 serving m (one retry, then next target)", calls, served)
	}
}

// When a provider's whole account pool is cooling from upstream 429s, the
// router must fall through to the next combo target without any doomed
// upstream attempt (zero calls against the cooling provider), and a
// single-target route must surface 429 + Retry-After naming the pool's
// recovery.
func TestExecuteFallsThroughOnCoolingPool(t *testing.T) {
	p := provider.NewPool()
	def := &provider.Def{
		Name: "p1", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "a", APIKey: "k1"}, {Name: "b", APIKey: "k2"}},
	}
	p.Set(def)
	p.Set(&provider.Def{
		Name: "p2", Kind: provider.KindAnthropic,
		Accounts: []provider.Account{{Name: "c", APIKey: "k3"}},
	})
	r := New(p)
	r.SetCombos([]*Combo{{
		Name: "stack",
		Targets: []Target{
			{Provider: "p1", Model: "m1"},
			{Provider: "p2", Model: "m2"},
		},
	}})
	res, _ := r.Resolve("stack")

	// Bench both p1 accounts (as upstream 429s would).
	def.RateLimited(&def.Accounts[0], 0)
	def.RateLimited(&def.Accounts[1], 0)

	calls := 0
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		calls++
		return "ok", nil
	}
	if got := r.Execute(context.Background(), res, caller, func(a any) {}); got != nil {
		t.Fatalf("combo should succeed via p2, got %v", got)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want exactly 1 (p2 only) — no doomed attempt against the cooling pool", calls)
	}

	// Single-target route: the cooling pool becomes 429 + Retry-After.
	res1, _ := r.Resolve("p1/m1")
	err := r.Execute(context.Background(), res1, caller, func(a any) {})
	if err == nil || err.Status != 429 || err.Type != "provider_rate_limited" {
		t.Fatalf("cooling pool: got %v, want 429 provider_rate_limited", err)
	}
	if err.RetryAfter == "" {
		t.Fatal("Retry-After missing on cooling-pool 429")
	}
}

// A gated 403 (issue #48) benches its account without spending the retry
// budget: rotation runs until a healthy key serves or the pool empties —
// a fully-gated pool answers the pool-empty 429, never the raw 403, and a
// mixed pool always reaches its healthy key.
func TestExecuteGated403RotatesWholePool(t *testing.T) {
	p := provider.NewPool()
	def := &provider.Def{
		Name: "p1", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{
			{Name: "g1", APIKey: "k1"}, {Name: "g2", APIKey: "k2"}, {Name: "h", APIKey: "k3"},
		},
	}
	p.Set(def)
	p.Set(&provider.Def{
		Name: "p2", Kind: provider.KindAnthropic,
		Accounts: []provider.Account{{Name: "c", APIKey: "k9"}},
	})
	r := New(p)
	r.SetCombos([]*Combo{{
		Name:    "stack",
		Targets: []Target{{Provider: "p1", Model: "m1"}, {Provider: "p2", Model: "m2"}},
	}})
	res, _ := r.Resolve("stack")

	calls := 0
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		calls++
		// Mirror Do's gated-403 handling: bench the account, then report
		// the Fallbackable 403 (Execute must not see a benched-less 403).
		// k1/k2 are the gated keys; k3/k9 serve.
		if acct.APIKey == "k1" || acct.APIKey == "k2" {
			def.Gated(acct)
			return nil, &types.APIError{Status: 403, Type: "upstream_error", Fallbackable: true,
				Message: "Access restricted. Deposit required to unlock premium models."}
		}
		return "ok", nil
	}
	if got := r.Execute(context.Background(), res, caller, func(a any) {}); got != nil {
		t.Fatalf("combo must succeed on the healthy key, got %v", got)
	}
	if calls != 3 { // g1, g2 benched, h serves — never reaches p2
		t.Fatalf("calls=%d, want 3 (two gated rotations + healthy serve)", calls)
	}

	// All-gated pool on a single-target route: pool-empty 429, not the 403.
	def2 := &provider.Def{
		Name: "p3", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "g1", APIKey: "k1"}, {Name: "g2", APIKey: "k2"}},
	}
	p.Set(def2)
	r.SetCombos(nil)
	r.SetModels([]string{"p3/m"})
	res3, _ := r.Resolve("p3/m")
	calls = 0
	if err := r.Execute(context.Background(), res3, caller, func(a any) {}); err == nil ||
		err.Status != 429 || err.Type != "provider_rate_limited" || err.RetryAfter == "" {
		t.Fatalf("fully-gated pool: got %v, want 429 provider_rate_limited with Retry-After", err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2 (both accounts benched, then pool-empty)", calls)
	}
}
