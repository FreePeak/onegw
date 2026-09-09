package router

import (
	"context"
	"strings"
	"testing"

	"onegw/internal/provider"
	"onegw/internal/types"
)

// commandcode's edge answers Cloudflare 52x while its own upstream model
// provider flaps — live 2026-09-09 15:35: 520 "Upstream model provider is
// temporarily unavailable. Please try again in a moment." (type server_error),
// terminal in the client's dashboard because 520 was missing from Retryable.
// The whole 52x family must retry the same target and fall through to the
// next combo target instead of ending the chain.
func TestCloudflare52xIsRetryable(t *testing.T) {
	for _, status := range []int{520, 521, 522, 523, 524, 525, 526, 527} {
		e := &types.APIError{Status: status, Type: "server_error"}
		if !e.Retryable() {
			t.Fatalf("status %d: must be retryable so combos fall through", status)
		}
	}
	// The neighbors stay classified as before: 5xx general retryable, 4xx not.
	if !(&types.APIError{Status: 502}).Retryable() {
		t.Fatal("502 must stay retryable")
	}
	if (&types.APIError{Status: 519}).Retryable() || (&types.APIError{Status: 528}).Retryable() {
		t.Fatal("non-CF 51x/52x must stay non-retryable")
	}
	if (&types.APIError{Status: 403}).Retryable() {
		t.Fatal("403 must stay non-retryable")
	}
}

// A combo whose commandcode target answers the live 520 must fall through to
// the next target and serve, never surface the 520.
func TestExecuteFallsThroughOnCloudflare520(t *testing.T) {
	p := provider.NewPool()
	p.Set(&provider.Def{
		Name: "cc", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "linh", APIKey: "k1"}},
	})
	p.Set(&provider.Def{
		Name: "oc", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "go", APIKey: "k2"}},
	})
	r := New(p)
	r.SetCombos([]*Combo{{
		Name:    "free",
		Targets: []Target{{Provider: "cc", Model: "z-ai/glm-5.3-flash"}, {Provider: "oc", Model: "mimo-v2.5"}},
	}})
	res, err := r.Resolve("free")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	attempts := 0
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		attempts++
		if def.Name == "cc" {
			return nil, &types.APIError{Status: 520, Type: "server_error",
				Message: "Upstream model provider is temporarily unavailable. Please try again in a moment."}
		}
		return "ok", nil
	}
	if got := r.Execute(context.Background(), res, caller, func(a any) {}); got != nil {
		t.Fatalf("combo must serve via the next target, got %v", got)
	}
	if attempts != 3 { // 520 retry (MaxAttempts 2) + fallback serve
		t.Fatalf("attempts=%d, want 3 (two 520 attempts then the serving target)", attempts)
	}
}

// The pool-empty 429 keeps its status/Retry-After contract (#48) but the
// message must name the real cause: a pool drained by per-model 403s is not
// "rate-limited upstream".
func TestExecutePoolEmptyMessageNamesCause(t *testing.T) {
	p := provider.NewPool()
	def := &provider.Def{
		Name: "glm", Kind: provider.KindOpenAI,
		Accounts: []provider.Account{{Name: "harvey", APIKey: "k1"}},
	}
	p.Set(def)
	r := New(p)
	r.SetModels([]string{"glm/glm-5.3-flash"})
	res, _ := r.Resolve("glm/glm-5.3-flash")
	caller := func(ctx context.Context, def *provider.Def, acct *provider.Account, model string) (any, *types.APIError) {
		// Mirror Do's Zhipu model_access_denied handling: bench + Fallbackable.
		def.Gated(acct)
		return nil, &types.APIError{Status: 403, Type: "model_access_denied", Fallbackable: true,
			Message: "No permission to access model: glm-5.3-flash"}
	}
	err := r.Execute(context.Background(), res, caller, func(a any) {})
	if err == nil || err.Status != 429 || err.Type != "provider_rate_limited" || err.RetryAfter == "" {
		t.Fatalf("got %v, want pool-empty 429 provider_rate_limited with Retry-After", err)
	}
	if !strings.Contains(err.Message, "403") || !strings.Contains(err.Message, "model_access_denied") {
		t.Fatalf("pool-empty message must name the upstream cause, got %q", err.Message)
	}
	if strings.Contains(err.Message, "all accounts rate-limited upstream; retry after") {
		t.Fatalf("cause-bearing message must not keep the lying rate-limit clause, got %q", err.Message)
	}
}

// Slash-models advertised in a provider's models table must stay resolved in
// failure-row labels: boundedModel("z-ai/glm-5.3-flash") collapsed to
// "unresolved" (live commandcode 520 rows logged as commandcode/unresolved)
// because KnownModel treated the "z-ai/" prefix as an unknown provider and
// returned before the advertised-model scan. Junk slash strings stay unknown.
func TestKnownModelAdvertisedSlashModel(t *testing.T) {
	p := provider.NewPool()
	p.Set(&provider.Def{
		Name: "commandcode", Kind: provider.KindOpenAI,
		Models:   []string{"z-ai/glm-5.3-flash", "deepseek/deepseek-v4-flash"},
		Accounts: []provider.Account{{Name: "linh", APIKey: "k1"}},
	})
	r := New(p)
	r.SetModels([]string{"commandcode/z-ai/glm-5.3-flash", "commandcode/deepseek/deepseek-v4-flash"})
	if !r.KnownModel("z-ai/glm-5.3-flash") {
		t.Fatal("advertised slash-model must be known (failure-row label honesty)")
	}
	if !r.KnownModel("deepseek/deepseek-v4-flash") {
		t.Fatal("second advertised slash-model must be known")
	}
	if !r.KnownModel("commandcode/z-ai/glm-5.3-flash") {
		t.Fatal("full provider/model route must stay known")
	}
	if r.KnownModel("z-ai/glm-9.9-fake") {
		t.Fatal("unadvertised slash-model must stay unknown: bounded cardinality")
	}
	if r.KnownModel("nosuchprovider/model") {
		t.Fatal("unknown provider prefix must stay unknown")
	}
}
