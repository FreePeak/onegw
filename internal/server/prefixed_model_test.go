package server

// Regression guard (2026-09-21): a "provider-prefixed" models entry must NOT be
// rewritten by the server. An upstream model id may legitimately begin with a
// segment equal to the provider's own name — OpenRouter's registry ships
// `openrouter/free`, `openrouter/auto`, `openrouter/fusion` — and the way to
// route to those is exactly the doubled advertised form (provider "openrouter"
// + model "openrouter/free").
//
// The id-advertisement confusion that motivated the normalization is handled
// where the model id is CONSUMED (translat.cursorRequestedModel drops a provider
// prefix before the wire) and by keeping kind defaults bare — neither of which
// can tell a real upstream segment from a redundant one the way a blanket
// string rewrite did. Live cost of getting this wrong: `openrouter/free` was
// rewritten to `free` and every request answered
// `404 No endpoints available for openrouter/free`.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelsEntryIsNotPrefixStripped(t *testing.T) {
	cfg := makeCfg(t, "sk-client", "", false,
		providerSpec{name: "openrouter", up: "http://127.0.0.1:1", model: "openrouter/free"})
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-client")
	w := do(t, s.Handler(), req)
	if w.Code != http.StatusOK {
		t.Fatalf("models status %d: %s", w.Code, w.Body.String())
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 {
		t.Fatalf("want exactly the configured entry, got %+v", list.Data)
	}
	// The advertised (doubled) form is the only string that routes: the router
	// strips one provider prefix, leaving "openrouter/free" for the upstream.
	if got := list.Data[0].ID; got != "openrouter/openrouter/free" {
		t.Fatalf("advertised %q, want the model id left intact (%q)", got, "openrouter/openrouter/free")
	}
}
