package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"onegw/internal/types"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Per-model lockout: a model-scoped upstream refusal (403 model access
// denied, 404 model not found) benches the (provider, model) pair, NOT the
// account pool. Sibling models keep serving on the same accounts; 429s stay
// account-scoped byte-identically.
// ---------------------------------------------------------------------------

const zhipuModelDeniedBody = `{"error":{"code":"1211","message":"Model access denied for model glm-9-pro.","type":"model_access_denied"}}`

// mkModelStub answers the given status/body when the request BODY names
// blockedModel (OpenAI-style /chat/completions carries the model there),
// 200 otherwise. hits counts every upstream attempt.
func mkModelStub(t *testing.T, status int, body, blockedModel string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		atomic.AddInt32(&hits, 1)
		if strings.Contains(string(raw), `"model":"`+blockedModel+`"`) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func doModel(t *testing.T, d *Def, acct *Account, model string) *types.APIError {
	t.Helper()
	_, apiErr := d.Do(context.Background(), acct, model, nil, bytes.NewReader([]byte(`{"model":"`+model+`","messages":[]}`)), false)
	return apiErr
}

func TestModelScopedClassifier(t *testing.T) {
	cases := []struct {
		status int
		typ    string
		code   string
		msg    string
		want   bool
	}{
		// Zhipu per-model 403 (live 2026-09-09).
		{403, "model_access_denied", "", "Model access denied for model x", true},
		{403, "", "1211", "Model access denied for model glm-9-pro.", true},
		// message-only 403 model access.
		{403, "", "", "you do not have model access to this model", true},
		// 404 model-not-found shapes.
		{404, "invalid_request_error", "model_not_found", "The model 'x' does not exist", true},
		{404, "", "", "model not found", true},
		// NOT model-scoped: the b-ai deposit gate is per-CREDENTIAL state
		// with its own pinned #48 contract (next request must reach the
		// healthy key — a model bench here would over-lock the model);
		// generic 404 (dead path); plain 403s; 429s.
		{403, "access_denied", "access_denied", "Deposit required to unlock premium models.", false},
		{404, "invalid_request_error", "", "Invalid URL (POST /v1/foo)", false},
		{403, "permission_error", "", "not allowed", false},
		{403, "invalid_request_error", "", "Invalid API key", false},
		{429, "rate_limit_error", "rate_limit_exceeded", "model access slow down", false},
		{200, "", "", "", false},
	}
	for i, tc := range cases {
		e := &types.APIError{Status: tc.status, Type: tc.typ, Code: tc.code, Message: tc.msg}
		if got := e.ModelScoped(); got != tc.want {
			t.Errorf("case %d (%d %s %q): ModelScoped = %v, want %v", i, tc.status, tc.typ, tc.msg, got, tc.want)
		}
	}
	// nil receiver must not panic.
	var e *types.APIError
	if e.ModelScoped() {
		t.Fatal("nil receiver must read as not model-scoped")
	}
}

func TestBenchModelSetExpireBound(t *testing.T) {
	d := &Def{Name: "p"}
	if benched, ready := d.ModelBenched("m"); benched || !ready.IsZero() {
		t.Fatalf("fresh def reports benched=%v ready=%v, want false/zero", benched, ready)
	}

	// Set with explicit TTL; not yet expired.
	d.BenchModel("m", time.Minute)
	benched, ready := d.ModelBenched("m")
	if !benched || ready.IsZero() {
		t.Fatalf("benched=%v ready=%v, want true/non-zero", benched, ready)
	}
	if got := time.Until(ready); got <= 50*time.Second || got > time.Minute {
		t.Fatalf("ready = %v from now, want ~1m", got)
	}

	// TTL 0 falls back to the default.
	d2 := &Def{Name: "p"}
	d2.BenchModel("m", 0)
	if benched, _ := d2.ModelBenched("m"); !benched {
		t.Fatal("zero TTL must still bench (default)")
	}
	_, ready2 := d2.ModelBenched("m")
	if got := time.Until(ready2); got <= ModelBenchTTL-time.Second {
		t.Fatalf("default TTL bench = %v from now, want ~%v", got, ModelBenchTTL)
	}

	// Bound: fill past the cap with LIVE benches; the map must stay capped.
	d3 := &Def{Name: "p"}
	for i := range maxModelBenches + 50 {
		d3.BenchModel(fmt.Sprintf("m%d", i), time.Hour)
	}
	d3.modelMu.RLock()
	size := len(d3.modelBench)
	d3.modelMu.RUnlock()
	if size != maxModelBenches {
		t.Fatalf("bench map size = %d, want capped at %d", size, maxModelBenches)
	}
	// At the cap with every entry LIVE, the soonest-to-expire bench is
	// evicted to make room ("m0" holds the earliest deadline), so the map
	// stays at the cap and m0 is gone.
	d3.BenchModel("overflow", time.Minute)
	d3.modelMu.RLock()
	size = len(d3.modelBench)
	_, m0Gone := d3.modelBench["m0"]
	_, freshOK := d3.modelBench["overflow"]
	d3.modelMu.RUnlock()
	if size != maxModelBenches {
		t.Fatalf("bench map size after overflow = %d, want still capped at %d", size, maxModelBenches)
	}
	if m0Gone {
		t.Fatal("soonest-to-expire bench m0 should have been evicted at the cap")
	}
	if !freshOK {
		t.Fatal("overflow bench must be recorded at the cap")
	}

	// Auto-expiry: past the TTL, ModelBenched reports false and clears.
	d4 := &Def{Name: "p"}
	d4.BenchModel("m", 10*time.Millisecond)
	time.Sleep(15 * time.Millisecond)
	if benched, ready := d4.ModelBenched("m"); benched || !ready.IsZero() {
		t.Fatalf("expired bench: benched=%v ready=%v, want false/zero", benched, ready)
	}
	d4.modelMu.RLock()
	size = len(d4.modelBench)
	d4.modelMu.RUnlock()
	if size != 0 {
		t.Fatalf("expired entry must be removed on read, size=%d", size)
	}
}

// TestDoBenchesModelOn403NotAccount pins the Do-side contract: a
// model-scoped 403 benches the (provider, model) pair and leaves the
// account untouched (no ladder strike, no cooldown), so the pool keeps
// serving sibling models on the same key.
func TestDoBenchesModelOn403NotAccount(t *testing.T) {
	srv, _ := mkModelStub(t, 403, zhipuModelDeniedBody, "blocked")
	def := &Def{Name: "zhipu", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "a", APIKey: "k1"}}}
	NewPool().Set(def)

	if apiErr := doModel(t, def, &def.Accounts[0], "blocked"); apiErr == nil || apiErr.Status != 403 {
		t.Fatalf("got %v, want the 403 relayed", apiErr)
	}
	if benched, ready := def.ModelBenched("blocked"); !benched || ready.IsZero() {
		t.Fatalf("model bench missing after 403: benched=%v ready=%v", benched, ready)
	}
	// The Zhipu 403 ALSO matches gated403 (access_denied family, pinned by
	// TestGated403SignatureNarrow), so the account gets its independent
	// ladder bench — intended layering: the in-flight request rotates to a
	// healthy key now, the model bench saves future requests entirely.
	// Assert the ladder benched the account (not a region-lock-style 5m).
	slot := findSlot(def.pool, "a")
	if d := time.Until(slot.cooldown); d <= 0 || d > coolCap {
		t.Fatalf("gated-layer account bench missing: cooldown=%v", slot.cooldown)
	}
	// Sibling model still served by the same account (stub answers 200).
	if apiErr := doModel(t, def, &def.Accounts[0], "healthy"); apiErr != nil {
		t.Fatalf("sibling model must not be poisoned by the bench: %v", apiErr)
	}
}

// TestDoBenchesModelOn404ModelNotFound: upstream 404 model_not_found is the
// second model-scoped shape — the model is dead on this provider.
func TestDoBenchesModelOn404ModelNotFound(t *testing.T) {
	srv, _ := mkModelStub(t, 404, `{"error":{"code":"model_not_found","message":"The model 'blocked' does not exist","type":"invalid_request_error"}}`, "blocked")
	def := &Def{Name: "p", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "a", APIKey: "k1"}}}
	NewPool().Set(def)
	if apiErr := doModel(t, def, &def.Accounts[0], "blocked"); apiErr == nil || apiErr.Status != 404 {
		t.Fatalf("got %v, want the 404 relayed", apiErr)
	}
	if benched, _ := def.ModelBenched("blocked"); !benched {
		t.Fatal("404 model_not_found must bench the model")
	}
	// A generic 404 (dead path, not model-shaped) must NOT bench.
	srv2, _ := mkModelStub(t, 404, `{"error":{"message":"Invalid URL (POST /v1/foo)","type":"invalid_request_error"}}`, "blocked")
	def2 := &Def{Name: "p2", Kind: KindOpenAI, BaseURL: srv2.URL,
		Accounts: []Account{{Name: "a", APIKey: "k1"}}}
	NewPool().Set(def2)
	if apiErr := doModel(t, def2, &def2.Accounts[0], "blocked"); apiErr == nil || apiErr.Status != 404 {
		t.Fatalf("got %v, want the 404 relayed", apiErr)
	}
	if benched, _ := def2.ModelBenched("blocked"); benched {
		t.Fatal("generic 404 must NOT bench the model")
	}
}

// TestDoDoesNotBenchModelOn429 pins byte-identical 429 behavior: the
// account ladder owns 429s; ModelBenched stays false.
func TestDoDoesNotBenchModelOn429(t *testing.T) {
	srv, _ := mkModelStub(t, 429, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`, "blocked")
	def := &Def{Name: "p", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "a", APIKey: "k1"}}}
	NewPool().Set(def)
	if apiErr := doModel(t, def, &def.Accounts[0], "blocked"); apiErr == nil || apiErr.Status != 429 {
		t.Fatalf("got %v, want the 429 relayed", apiErr)
	}
	if benched, _ := def.ModelBenched("blocked"); benched {
		t.Fatal("429 must NOT bench the model (account ladder owns it)")
	}
	if slot := findSlot(def.pool, "a"); slot.cooldown.IsZero() {
		t.Fatal("429 must still bench the ACCOUNT (ladder intact)")
	}
}
