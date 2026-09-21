package server

// A provider's `models` entry sometimes already carries the provider's own name
// ("cursor/auto" under [[providers]] name = "cursor"). The server qualifies
// every advertised model with its provider name and the router strips exactly
// one prefix, so such an entry resolves to nothing — and, worse, /v1/models
// publishes the unusable doubled string, which is how a client pins it. That is
// the reported 2026-09-21 symptom: the live gateway answered `400 AI Model Not
// Found` for the exact id it had advertised, while cursor/auto worked beside it.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrefixedModelsEntryIsNormalized(t *testing.T) {
	cfg := makeCfg(t, "sk-client", "", false,
		providerSpec{name: "cursor", up: "http://127.0.0.1:1", model: "cursor/auto"})
	cfg.Providers[0].Kind = "cursor"
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
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		ids = append(ids, m.ID)
		if strings.HasPrefix(m.ID, "cursor/cursor/") {
			t.Fatalf("advertised the unusable doubled id %q (all: %v)", m.ID, ids)
		}
	}
	if len(ids) != 1 || ids[0] != "cursor/auto" {
		t.Fatalf("want [cursor/auto], got %v", ids)
	}
}
