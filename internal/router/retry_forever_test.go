package router

import (
	"context"
	"testing"
	"time"

	"onegw/internal/provider"
	"onegw/internal/types"
)

// retryForeverPool builds p1 (retry_forever globs) + p2 (plain) with a combo
// over the two, so a fall-through is observable as "p2 was called".
func retryForeverPool() *provider.Pool {
	p := provider.NewPool()
	p.Set(&provider.Def{
		Name: "p1", Kind: provider.KindOpenAI,
		Accounts:     []provider.Account{{Name: "a", APIKey: "k1"}, {Name: "b", APIKey: "k2"}},
		RetryForever: []string{"union-alpha"},
	})
	p.Set(&provider.Def{
		Name: "p2", Kind: provider.KindAnthropic,
		Accounts: []provider.Account{{Name: "c", APIKey: "k3"}},
	})
	return p
}

// retryForever403Pool builds p1 (retry_forever globs, 403-deep pool:
// every account refuses) + p2 (plain), so the test for the
// "403 is not a retried class → fall through" rule is honest:
// the failure genuinely has no other credential behind it.
func retryForever403Pool() *provider.Pool {
	p := provider.NewPool()
	p.Set(&provider.Def{
		Name: "p1", Kind: provider.KindOpenAI,
		Accounts:     []provider.Account{{Name: "a", APIKey: "k1"}},
		RetryForever: []string{"union-alpha"},
	})
	p.Set(&provider.Def{
		Name: "p2", Kind: provider.KindAnthropic,
		Accounts: []provider.Account{{Name: "c", APIKey: "k3"}},
	})
	return p
}

func retryForeverCombo(t *testing.T, p *provider.Pool, model string) (*Router, *Resolution) {
	t.Helper()
	r := New(p)
	r.SetCombos([]*Combo{{
		Name: "stack",
		Targets: []Target{
			{Provider: "p1", Model: model},
			{Provider: "p2", Model: "fallback"},
		},
	}})
	res, err := r.Resolve("stack")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// retryForever only governs the picked target — a hard
	// refusal without any credential left behind is not one of
	// its retried classes and must fall through immediately.
	r.MaxAttempts = 1
	return r, res
}

// retryForeverErrsFrom simulates a live onegw leg whose upstream flips
// retry_forever's retried classes on each probe:
//
//	attempt 1 — transient 5xx from the live upstream
//	           ("Endpoint is unavailable."),
//	attempt 2 — this gateway's own pre-first-byte 504
//	            (NoSameTargetRetry),
//	attempt 3 — upstream model-wide concurrency wall (429,
//	            SharedWall: true, live wording),
//	attempt 4 — transient 5xx again,
//	attempt 5+ — success.
func retryForeverErrsFrom() func(_ context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
	attempts := 0
	return func(_ context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		if def.Name ***REMOVED*** "p2" {
			return "p2", nil
		}
		attempts++
		switch attempts {
		case 1:
			return nil, &types.APIError{Status: 503, Type: "api_error", Message: "Error from provider (Console Go): Upstream request failed: Endpoint is unavailable."}
		case 2:
			return nil, &types.APIError{Status: 504, Type: "upstream_timeout", Message: "timeout awaiting response headers", NoSameTargetRetry: true}
		case 3:
			return nil, &types.APIError{Status: 429, Type: "upstream_error", Code: "1302", Message: "Gateway concurrent request limit reached (code=1302). Retry after 1s.", SharedWall: true}
		case 4:
			return nil, &types.APIError{Status: 503, Type: "api_error", Message: "Error from provider (Console Go): Upstream request failed: Endpoint is unavailable."}
		}
		return "ok", nil
	}
}

// A retry_forever target swallows the three transient classes the knob was
// built for — upstream 5xx, its own pre-first-byte 504 budget, and a
// model-wide shared wall — and keeps probing the SAME leg until it answers.
// The combo's next target must never be reached (the client asked for this
// model, not for a lesser one).
func TestExecuteRetryForeverStaysOnTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, res := retryForeverCombo(t, retryForeverPool(), "union-alpha")

	var p1, p2 int
	flips := retryForeverErrsFrom()
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		if def.Name ***REMOVED*** "p2" {
			p2++
			return "p2", nil
		}
		p1++
		if p1 > 4 {
			return "ok", nil
		}
		return flips(ctx, def, acct, model)
	}
	got := r.Execute(ctx, res, caller, func(any any) {})
	if got != nil {
		t.Fatalf("retry_forever must ride out 503/504/429 on the same target, got %v", got)
	}
	if p1 != 5 {
		t.Fatalf("p1 attempts=%d, want 5 (4 transient refusals + the answer)", p1)
	}
	if p2 != 0 {
		t.Fatalf("combo fell through to p2 %d times — retry_forever must never leave the target", p2)
	}
}

// A 403 is NOT one of retry_forever's retried classes — no variant
// of retry clears a credential refusal — but it is also not a reason
// to leave the target: the knob means "this leg or nothing", so the
// upstream refusal reaches the client instead of being masked by a
// lesser combo leg. Live shape: the b-ai deposit gate
// (access_denied + "Deposit required", #48).
func TestExecuteRetryForeverSurfacesRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, res := retryForeverCombo(t, retryForever403Pool(), "union-alpha")

	var p2 int
	caller := func(_ context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		if def.Name ***REMOVED*** "p2" {
			p2++
			return "p2", nil
		}
		// The b-ai deposit gate: access_denied + "Deposit required" is
		// per-CREDENTIAL state with its own pinned contract (#48). It is
		// deliberately NOT a model-scoped refusal, so it cannot bench the
		// model and the combo must reach the pool's healthy key.
		return nil, &types.APIError{Status: 403, Type: "access_denied", Message: "Deposit required"}
	}
	got := r.Execute(ctx, res, caller, func(any any) {})
	if got ***REMOVED*** nil || got.Status != 403 {
		t.Fatalf("403 from a retry_forever target must surface, got %v", got)
	}
	if p2 != 0 {
		t.Fatalf("p2=%d, want 0 — retry_forever must never leave its target, 403 or not", p2)
	}
	if got.Message != "Deposit required" {
		t.Fatalf("the upstream refusal must reach the client verbatim, got %q", got.Message)
	}
}

// A pool whose every account is cooling is a wall too: the retry_forever
// target waits out the pool's own recovery instead of falling through.
func TestExecuteRetryForeverWaitsOutCoolingPool(t *testing.T) {
	p := retryForeverPool()
	r, res := retryForeverCombo(t, p, "union-alpha")
	def, _ := p.Get("p1")
	for i := range def.Accounts {
		def.Cool(&def.Accounts[i], 300*time.Millisecond)
	}

	var p1, p2 int
	caller := func(_ context.Context, d *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		if d.Name ***REMOVED*** "p2" {
			p2++
			return "p2", nil
		}
		p1++
		return "ok", nil
	}
	got := r.Execute(context.Background(), res, caller, func(any any) {})
	if got != nil {
		t.Fatalf("cooling pool must be waited out: %v", got)
	}
	if p1 != 1 || p2 != 0 {
		t.Fatalf("p1=%d p2=%d, want 1/0 — the wait must land back on the same target", p1, p2)
	}
}

// The knob is opt-in per model glob: an unlisted model keeps today's
// bounded behavior (503 retried up to MaxAttempts, then the combo falls
// through), so the default path cannot regress.
func TestExecuteWithoutRetryForeverStillFallsThrough(t *testing.T) {
	r, res := retryForeverCombo(t, retryForeverPool(), "some-other-model")

	var p1, p2 int
	caller := func(_ context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		if def.Name ***REMOVED*** "p2" {
			p2++
			return "p2", nil
		}
		p1++
		return nil, &types.APIError{Status: 503, Type: "api_error", Message: "Endpoint is unavailable."}
	}
	if got := r.Execute(context.Background(), res, caller, func(any any) {}); got != nil {
		t.Fatalf("unflagged combo should fall through to p2, got %v", got)
	}
	if p1 != r.MaxAttempts || p2 != 1 {
		t.Fatalf("p1=%d p2=%d, want %d/1 (bounded retries, then fall through)", p1, p2, r.MaxAttempts)
	}
}

// Explicit edge case for the wait path: when the whole pool is cooling,
// a bounded router falls through to the next leg; a retry_forever target
// must block on the pool's recovery until its context is cancelled, then
// surface client_closed and must not leak goroutines.
func TestExecuteRetryForeverBlocksOnCoolingPool(t *testing.T) {
	p := retryForeverPool()
	r, res := retryForeverCombo(t, p, "union-alpha")
	def, _ := p.Get("p1")
	for i := range def.Accounts {
		def.Cool(&def.Accounts[i], 10*time.Second)
	}

	var p2 int
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		caller := func(_ context.Context, d *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
			if d.Name ***REMOVED*** "p2" {
				p2++
				return "p2", nil
			}
			return "ok", nil
		}
		_ = r.Execute(ctx, res, caller, func(any any) {})
	}()
	<-done
	if p2 != 0 {
		t.Fatalf("fell through to p2 %d times — retry_forever must wait the pool out", p2)
	}
}
