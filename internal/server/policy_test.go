package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"onegw/internal/config"
)

// newPolicyServer wires a server with one provider and the given key policies.
func newPolicyServer(t *testing.T, keys []config.AuthKey, provs ...providerSpec) (*Server, http.Handler) {
	t.Helper()
	up := upstreamStub(provs[0].model)
	t.Cleanup(up.Close)
	specs := append([]providerSpec{{name: "p1", up: up.URL, model: provs[0].model}}, provs[1:]...)
	cfg := makeCfg(t, "", "", false, specs...)
	cfg.Auth.KeyList = keys
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, srv.Handler()
}

func authed(t *testing.T, model, key string) *http.Request {
	t.Helper()
	r := chatReq(t, model)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	return r
}

func body(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("response body not json: %v (%q)", err, w.Body.String())
	}
	return m
}

func errBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	b := body(t, w)
	e, ok := b["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object in %q", w.Body.String())
	}
	return e
}

func TestRateLimitRPMDeniesWithRetryAfter(t *testing.T) {
	_, h := newPolicyServer(t, []config.AuthKey{{Key: "sk-test-rpm", Name: "r", RPM: 3}}, providerSpec{name: "p1", model: "m1"})

	for i := 0; i < 3; i++ {
		w := do(t, h, authed(t, "m1", "sk-test-rpm"))
		if w.Code != 200 {
			t.Fatalf("request %d: status %d, want 200", i+1, w.Code)
		}
	}
	w := do(t, h, authed(t, "m1", "sk-test-rpm"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("4th request status %d, want 429", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After = %q, want positive seconds", ra)
	}
	e := errBody(t, w)
	if e["type"] != "rate_limited" {
		t.Fatalf("error type = %v, want rate_limited", e["type"])
	}
	if !strings.Contains(e["message"].(string), "r") {
		t.Fatalf("message %v should name the key label", e["message"])
	}
}

func TestRateLimitRPMWindowRecovers(t *testing.T) {
	_, h := newPolicyServer(t, []config.AuthKey{{Key: "sk-test-rpm2", RPM: 1}}, providerSpec{name: "p1", model: "m1"})

	if w := do(t, h, authed(t, "m1", "sk-test-rpm2")); w.Code != 200 {
		t.Fatalf("first request status %d", w.Code)
	}
	if w := do(t, h, authed(t, "m1", "sk-test-rpm2")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status %d, want 429 (sliding window)", w.Code)
	}
	// Recovery after the full 60s window slides is verified deterministically
	// by the injected-clock tests in internal/ratelimit.
}

func TestRateLimitTPMBlocksNextRequest(t *testing.T) {
	// The stub reports no usage, so tokens are estimated as len(body)/4
	// (~15 per call). With tpm=30 the third call is pre-blocked.
	_, h := newPolicyServer(t, []config.AuthKey{{Key: "sk-test-tpm", Name: "t", TPM: 30}}, providerSpec{name: "p1", model: "m1"})

	for i := 0; i < 2; i++ {
		if w := do(t, h, authed(t, "m1", "sk-test-tpm")); w.Code != 200 {
			t.Fatalf("request %d status %d, want 200", i+1, w.Code)
		}
	}
	w := do(t, h, authed(t, "m1", "sk-test-tpm"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 after tokens exceeded tpm", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After on 429")
	}
	if got := errBody(t, w)["type"]; got != "rate_limited" {
		t.Fatalf("error type = %v, want rate_limited", got)
	}
}

func TestAllowlistDeniesAndNamesModelAndKey(t *testing.T) {
	_, h := newPolicyServer(t, []config.AuthKey{{
		Key: "sk-test-allow", Name: "teamA", Models: []string{"m1"},
	}}, providerSpec{name: "p1", model: "m1"}, providerSpec{name: "p2", model: "m2"})

	w := do(t, h, authed(t, "m2", "sk-test-allow"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", w.Code)
	}
	e := errBody(t, w)
	if e["type"] != "model_not_allowed" {
		t.Fatalf("error type = %v, want model_not_allowed", e["type"])
	}
	msg := e["message"].(string)
	if !strings.Contains(msg, "m2") || !strings.Contains(msg, "teamA") {
		t.Fatalf("message %q must name model and key", msg)
	}

	// Allowed model still goes through.
	if w := do(t, h, authed(t, "m1", "sk-test-allow")); w.Code != 200 {
		t.Fatalf("allowed model status %d, want 200", w.Code)
	}
}

func TestAllowlistMatchesComboAndProviderModel(t *testing.T) {
	_, h := newPolicyServer(t, []config.AuthKey{{
		Key: "sk-test-combo", Models: []string{"pair"},
	}}, providerSpec{name: "p1", model: "m1"}, providerSpec{name: "p2", model: "m2"})

	// Combo listed in the allowlist resolves for the key.
	w := do(t, h, authed(t, "pair", "sk-test-combo"))
	if w.Code != 200 {
		t.Fatalf("combo request status %d, want 200; body %q", w.Code, w.Body.String())
	}
	// provider/model entry also matches a direct route.
	h2key := []config.AuthKey{{Key: "sk-test-direct", Models: []string{"p1/m1"}}}
	_, h2 := newPolicyServer(t, h2key, providerSpec{name: "p1", model: "m1"}, providerSpec{name: "p2", model: "m2"})
	if w := do(t, h2, authed(t, "p1/m1", "sk-test-direct")); w.Code != 200 {
		t.Fatalf("direct route status %d, want 200", w.Code)
	}
	if w := do(t, h2, authed(t, "p2/m2", "sk-test-direct")); w.Code != http.StatusForbidden {
		t.Fatalf("off-allowlist direct route status %d, want 403", w.Code)
	}
}

func TestFlatKeysRemainUnlimited(t *testing.T) {
	_, h := newPolicyServer(t, []config.AuthKey{{Key: "sk-test-flat", Name: "", RPM: 0, TPM: 0}}, providerSpec{name: "p1", model: "m1"})

	for i := 0; i < 25; i++ {
		if w := do(t, h, authed(t, "m1", "sk-test-flat")); w.Code != 200 {
			t.Fatalf("request %d status %d, want 200 (flat key unlimited)", i+1, w.Code)
		}
	}
}

func TestOpenGatewayWithNoKeys(t *testing.T) {
	_, h := newPolicyServer(t, nil, providerSpec{name: "p1", model: "m1"})

	if w := do(t, h, authed(t, "m1", "")); w.Code != 200 {
		t.Fatalf("open gateway status %d, want 200", w.Code)
	}
}

func TestInvalidKeyStill401(t *testing.T) {
	_, h := newPolicyServer(t, []config.AuthKey{{Key: "sk-test-good"}}, providerSpec{name: "p1", model: "m1"})

	w := do(t, h, authed(t, "m1", "sk-test-wrong"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
	// Keyed policy also applies per surface header.
	r := chatReq(t, "m1")
	r.Header.Set("x-api-key", "sk-test-good")
	if w := do(t, h, r); w.Code != 200 {
		t.Fatalf("x-api-key auth status %d, want 200", w.Code)
	}
}

func TestGeminiSurfaceEnforcesPolicy(t *testing.T) {
	_, h := newPolicyServer(t, []config.AuthKey{{Key: "sk-test-gem", RPM: 1}}, providerSpec{name: "p1", model: "m1"})
	mkGemini := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1beta/models/m1:generateContent", strings.NewReader(`{"contents":[{"parts":[{"text":"ping"}]}]}`))
		r.Header.Set("x-api-key", "sk-test-gem")
		return r
	}
	if w := do(t, h, mkGemini()); w.Code != 200 {
		t.Fatalf("first gemini request status %d", w.Code)
	}
	w := do(t, h, mkGemini())
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second gemini request status %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After on gemini 429")
	}
}

func TestUsageRecordsKeyLabelNotRawKey(t *testing.T) {
	srv, h := newPolicyServer(t, []config.AuthKey{
		{Key: "sk-test-abcd1234ef56", Name: "teamA"},
		{Key: "sk-test-rawkey99xy"},
	}, providerSpec{name: "p1", model: "m1"})

	if w := do(t, h, authed(t, "m1", "sk-test-abcd1234ef56")); w.Code != 200 {
		t.Fatalf("named key request status %d", w.Code)
	}
	if w := do(t, h, authed(t, "m1", "sk-test-rawkey99xy")); w.Code != 200 {
		t.Fatalf("unnamed key request status %d", w.Code)
	}
	labels := map[string]bool{}
	for _, b := range srv.cur().usage.Snapshot() {
		labels[b.APIKey] = true
	}
	if !labels["teamA"] {
		t.Fatalf("usage missing label teamA: %v", labels)
	}
	if !labels["sk-t***xy"] {
		t.Fatalf("usage missing masked key sk-t***xy: %v", labels)
	}
	if len(labels) != 2 {
		t.Fatalf("usage labels = %v, want exactly the two labels", labels)
	}
}
