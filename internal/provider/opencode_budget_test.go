package provider

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Live 2026-10-19 14:09Z (opencode/beam-free, OpenCode Zen Go): the Go lane
// meters every MODEL against its own allowance — the vendor's usage page
// lists a monthly limit per model — so its 429 indicts the (provider,
// model) pair, never the credential:
//
//	429, Retry-After: 34951
//	{"error":{"type":"server_error","message":"Upstream request failed: Rate limit exceeded. Please try again later."}}
//
// The Retry-After points at the lane's next UTC-midnight window (~9.7h).
// Benching the ACCOUNT for it took the whole provider down: both keys went
// dark and every one of the provider's 37 models answered "all accounts
// rate-limited upstream" until midnight — while 27 of them served 200s
// with the same key the whole time (live-probed 2026-10-19 14:22Z: 27 of
// 37 models 200, beam-free 429, both accounts carrying the same
// Retry-After).
//
// The wall must park the MODEL for the vendor's window and leave the
// credential in rotation.
// ---------------------------------------------------------------------------

const goModelBudgetBody = `{"error":{"type":"server_error","message":"Upstream request failed: Rate limit exceeded. Please try again later."}}`

// goBudgetStub answers every request with the Go lane's per-model budget
// wall, carrying the vendor's window as a Retry-After.
func goBudgetStub(t *testing.T, retryAfter string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, goModelBudgetBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenCodeGoModelBudgetParksModelNotAccount(t *testing.T) {
	srv := goBudgetStub(t, "34951")
	def := &Def{Name: "opencode", Kind: KindOpenCode, BaseURL: srv.URL,
		Accounts: []Account{{Name: "a1", APIKey: "oc"}}}
	NewPool().Set(def)

	_, apiErr := def.Do(t.Context(), &def.Accounts[0], "beam-free", nil, bytes.NewReader([]byte(`{}`)), false)
	if apiErr == nil || apiErr.Status != 429 {
		t.Fatalf("got %+v, want 429", apiErr)
	}
	if apiErr.RetryAfter != "34951" {
		t.Fatalf("RetryAfter = %q, want the vendor's 34951", apiErr.RetryAfter)
	}
	if !apiErr.SharedConcurrency() {
		t.Fatalf("a model-wide budget wall must fall through, not burn the pool: %+v", apiErr)
	}
	// The credential is healthy: it served every sibling model 200s in the
	// same window, so it must stay in rotation.
	if slot := findSlot(def.pool, "a1"); !slot.cooldown.IsZero() || slot.strikes != 0 {
		t.Fatalf("model budget wall must not bench the account, cooldown=%v strikes=%d", slot.cooldown, slot.strikes)
	}
	if got, _ := def.NextAccount(""); got == nil || got.Name != "a1" {
		t.Fatalf("pool must keep serving the healthy key, got %+v", got)
	}
	// The MODEL is parked for the vendor's window.
	benched, ready := def.ModelBenched("beam-free")
	if !benched {
		t.Fatal("the model must be parked for the vendor's window")
	}
	if left := time.Until(ready); left < 34000*time.Second || left > 35000*time.Second {
		t.Fatalf("model bench = %v, want the vendor's ~34951s window", left)
	}
}

// A genuine per-key 429 on the Go lane keeps the account ladder: only the
// vendor's per-model budget wrapper is model-scoped.
func TestOpenCodeGoPerKey429StillBenchesAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "34951")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`)
	}))
	t.Cleanup(srv.Close)
	def := &Def{Name: "opencode", Kind: KindOpenCode, BaseURL: srv.URL,
		Accounts: []Account{{Name: "a1", APIKey: "oc"}}}
	NewPool().Set(def)

	_, apiErr := def.Do(t.Context(), &def.Accounts[0], "beam-free", nil, bytes.NewReader([]byte(`{}`)), false)
	if apiErr == nil || apiErr.Status != 429 {
		t.Fatalf("got %+v, want 429", apiErr)
	}
	if slot := findSlot(def.pool, "a1"); slot.cooldown.IsZero() {
		t.Fatal("a per-key 429 must still bench the account")
	}
}

// The wrapper is Go-lane specific: the same wording on another kind keeps
// the per-key ladder (only the Go lane meters per model).
func TestGoModelBudgetWrapperIsGoLaneOnly(t *testing.T) {
	srv := goBudgetStub(t, "34951")
	def := &Def{Name: "other", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "a1", APIKey: "k1"}}}
	NewPool().Set(def)

	_, apiErr := def.Do(t.Context(), &def.Accounts[0], "m", nil, bytes.NewReader([]byte(`{}`)), false)
	if apiErr == nil || apiErr.Status != 429 {
		t.Fatalf("got %+v, want 429", apiErr)
	}
	if slot := findSlot(def.pool, "a1"); slot.cooldown.IsZero() {
		t.Fatal("a non-Go-lane 429 must still bench the account")
	}
}
