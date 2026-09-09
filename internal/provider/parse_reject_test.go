package provider

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Do must rewrite the b-ai distributor's node-level parse rejection of
// large valid bodies ("Invalid request body. (request id: …)", live RCA
// 2026-09-09: 22 client-visible terminal 400s, all one backend node,
// byte-identical replays serving 200 elsewhere) into a retryable
// 502-class fault so Router.Execute retries and combos fall through.
func TestDoRewritesParseReject400ToRetryable502(t *testing.T) {
	const body = `{"error":{"message":"Invalid request body. (request id: 20260909040930395509442c955d568b92psW5o)","type":"api_error","param":"invalid_request"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p := NewPool()
	def := &Def{Name: "b-ai", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "k1", APIKey: "k1"}}}
	p.Set(def)

	_, apiErr := def.Do(t.Context(), &def.Accounts[0], "glm-5.3-flash", nil, bytes.NewReader([]byte(`{"model":"m","messages":[]}`)), false)
	if apiErr == nil {
		t.Fatal("Do: want error")
	}
	if apiErr.Status != 502 {
		t.Fatalf("status = %d, want 502 (retryable; node fault, not request fault)", apiErr.Status)
	}
	if apiErr.Type != "upstream_parse_rejected" {
		t.Fatalf("type = %q, want upstream_parse_rejected", apiErr.Type)
	}
	if apiErr.Message != "Invalid request body. (request id: 20260909040930395509442c955d568b92psW5o)" {
		t.Fatalf("upstream message must survive for the dashboard, got %q", apiErr.Message)
	}
	// Healthy credential: the account must not land on the cooldown ladder.
	if slot := findSlot(def.pool, "k1"); slot != nil && !slot.cooldown.IsZero() {
		t.Fatalf("healthy account benched on cooldown %v", slot.cooldown)
	}
}

// A genuine schema 400 (named parameter, invalid_request_error) is the
// client's fault and must keep failing fast — no rewrite.
func TestDoKeepsGenuineSchema400Terminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"The request is invalid: invalid parameter temperature. Please check the request body.","type":"invalid_request_error","param":"","code":"400001"}}`))
	}))
	t.Cleanup(srv.Close)
	p := NewPool()
	def := &Def{Name: "b-ai", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "k1", APIKey: "k1"}}}
	p.Set(def)

	_, apiErr := def.Do(t.Context(), &def.Accounts[0], "glm-5.3-flash", nil, bytes.NewReader([]byte(`{"model":"m","messages":[]}`)), false)
	if apiErr == nil {
		t.Fatal("Do: want error")
	}
	if apiErr.Status != 400 || apiErr.Type != "invalid_request_error" {
		t.Fatalf("got %d/%s, want untouched 400/invalid_request_error", apiErr.Status, apiErr.Type)
	}
}
