package provider

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Some upstreams answer error statuses with a ZERO-byte body — live glm
// evidence 2026-09-09: api.z.ai /api/v1 returned 500s with no payload while
// model access was being deprovisioned. The surfaced APIError must name the
// observation (upstream_empty_body) instead of an empty message that tells
// the dashboard and the client nothing.
func TestDoNamesEmptyUpstreamErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500) // no body at all
	}))
	t.Cleanup(srv.Close)
	p := NewPool()
	def := &Def{Name: "glm", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "harvey", APIKey: "k1"}}}
	p.Set(def)

	_, apiErr := def.Do(t.Context(), &def.Accounts[0], "glm-5.3-flash", nil, bytes.NewReader([]byte(`{"model":"m","messages":[]}`)), false)
	if apiErr == nil {
		t.Fatal("Do: want error")
	}
	if apiErr.Status != 500 {
		t.Fatalf("status = %d, want 500 (status is real, body is not)", apiErr.Status)
	}
	if apiErr.Type != "upstream_empty_body" {
		t.Fatalf("type = %q, want upstream_empty_body", apiErr.Type)
	}
	if !strings.Contains(apiErr.Message, "empty error body") || !strings.Contains(apiErr.Message, "glm") {
		t.Fatalf("message must name the provider and the empty body, got %q", apiErr.Message)
	}
}

// An upstream error WITH a payload keeps its decoded shape — the empty-body
// rewrite must never fire for real error bodies.
func TestDoKeepsRealErrorBodyShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"server_error"}}`))
	}))
	t.Cleanup(srv.Close)
	p := NewPool()
	def := &Def{Name: "glm", Kind: KindOpenAI, BaseURL: srv.URL,
		Accounts: []Account{{Name: "harvey", APIKey: "k1"}}}
	p.Set(def)

	_, apiErr := def.Do(t.Context(), &def.Accounts[0], "glm-5.3-flash", nil, bytes.NewReader([]byte(`{"model":"m","messages":[]}`)), false)
	if apiErr == nil {
		t.Fatal("Do: want error")
	}
	if apiErr.Type == "upstream_empty_body" {
		t.Fatalf("real error body must keep its decoded type, got %q", apiErr.Type)
	}
	if apiErr.Message != "boom" {
		t.Fatalf("message = %q, want upstream's own", apiErr.Message)
	}
}
