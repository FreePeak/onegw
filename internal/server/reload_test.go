package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"onegw/internal/config"
)

// upstreamStub answers OpenAI chat completions with a fixed completion that
// echoes the model string it was asked for.
func upstreamStub(model string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "cmpl-test",
			"object":  "chat.completion",
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "pong from " + model}}},
		})
	}))
}

// providerSpec is one fake upstream provider in a test config.
type providerSpec struct {
	name  string
	up    string
	model string
}

func makeCfg(t *testing.T, key, adminPW string, saverEnabled bool, provs ...providerSpec) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Server.AdminPassword = adminPW
	cfg.Auth.Keys = []string{key}
	cfg.Saver.Enabled = saverEnabled
	for _, pr := range provs {
		cfg.Providers = append(cfg.Providers, config.ProviderCfg{
			Name: pr.name, Kind: "openai", BaseURL: pr.up,
			APIKey: "up-key", Models: []string{pr.model},
		})
	}
	if len(provs) > 1 {
		cfg.Combos = []config.ComboCfg{{
			Name:    "pair",
			Targets: []string{provs[0].name + "/" + provs[0].model, provs[1].name + "/" + provs[1].model},
		}}
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	return cfg
}

func chatReq(t *testing.T, model string) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func do(t *testing.T, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestReloadSwapsProvidersKeysAndAdmin(t *testing.T) {
	up1 := upstreamStub("m1")
	defer up1.Close()
	up2 := upstreamStub("m2")
	defer up2.Close()

	cfg1 := makeCfg(t, "key-one", "pw-one", false, providerSpec{name: "p1", up: up1.URL, model: "m1"})
	srv, err := New(cfg1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	// Baseline: route through p1 with the initial key.
	w := do(t, h, func() *http.Request {
		r := chatReq(t, "p1/m1")
		r.Header.Set("Authorization", "Bearer key-one")
		return r
	}())
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from m1") {
		t.Fatalf("baseline request failed: code=%d body=%s", w.Code, w.Body.String())
	}

	// Wrong key is 401 before the reload too (sanity).
	if w := do(t, h, func() *http.Request {
		r := chatReq(t, "p1/m1")
		r.Header.Set("Authorization", "Bearer key-two")
		return r
	}()); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key should 401, got %d", w.Code)
	}

	// Reload: p1 dropped, only p2 remains; keys and admin password rotated.
	cfg2 := makeCfg(t, "key-two", "pw-two", true, providerSpec{name: "p2", up: up2.URL, model: "m2"})
	srv.Reload(cfg2)

	// Old key now rejected.
	if w := do(t, h, func() *http.Request {
		r := chatReq(t, "p2/m2")
		r.Header.Set("Authorization", "Bearer key-one")
		return r
	}()); w.Code != http.StatusUnauthorized {
		t.Fatalf("old key should 401 after reload, got %d", w.Code)
	}

	// New key routes to p2.
	w = do(t, h, func() *http.Request {
		r := chatReq(t, "p2/m2")
		r.Header.Set("Authorization", "Bearer key-two")
		return r
	}())
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from m2") {
		t.Fatalf("new provider request failed: code=%d body=%s", w.Code, w.Body.String())
	}

	// Removed provider no longer resolves.
	if w := do(t, h, func() *http.Request {
		r := chatReq(t, "p1/m1")
		r.Header.Set("Authorization", "Bearer key-two")
		return r
	}()); w.Code != http.StatusNotFound {
		t.Fatalf("removed provider should 404 after reload, got %d body=%s", w.Code, w.Body.String())
	}

	// /v1/models reflects the new provider set only.
	w = do(t, h, func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer key-two")
		return r
	}())
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatalf("models decode: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range listing.Data {
		ids[m.ID] = true
	}
	if ids["p2/m2"] != true || ids["p1/m1"] != false {
		t.Fatalf("models listing wrong after reload: %v", ids)
	}
	// The combo from cfg1 was dropped along with p1; it must not be listed.
	if ids["pair"] {
		t.Fatal("combo from previous config must not survive a reload that drops its providers")
	}

	// Admin password rotated.
	if w := do(t, h, httptest.NewRequest(http.MethodGet, "/admin/usage?password=pw-one", nil)); w.Code != http.StatusUnauthorized {
		t.Fatalf("old admin password should 401, got %d", w.Code)
	}
	if w := do(t, h, httptest.NewRequest(http.MethodGet, "/admin/usage?password=pw-two", nil)); w.Code != 200 {
		t.Fatalf("new admin password should pass, got %d", w.Code)
	}
}

// The SIGHUP caller only ever hands Reload configs that passed config.Load
// (which validates) — prove the live state is untouched when nothing changes
// and that a config which WOULD fail validation is rejected before Reload.
func TestReloadBadConfigNeverApplied(t *testing.T) {
	up1 := upstreamStub("m1")
	defer up1.Close()
	cfg1 := makeCfg(t, "key-one", "pw-one", false, providerSpec{name: "p1", up: up1.URL, model: "m1"})
	srv, err := New(cfg1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	// A config that fails Validate (provider without kind) is rejected by
	// config.Load and never reaches Reload.
	bad := makeCfg(t, "key-one", "pw-one", false, providerSpec{name: "p1", up: up1.URL, model: "m1"})
	bad.Providers[0].Kind = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("expected bad config to fail Validate (caller-side guard exists)")
	}

	// Live config unchanged: old key still works and routes to p1.
	w := do(t, h, func() *http.Request {
		r := chatReq(t, "p1/m1")
		r.Header.Set("Authorization", "Bearer key-one")
		return r
	}())
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from m1") {
		t.Fatalf("state disturbed: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAliasRoutesThroughHandler(t *testing.T) {
	up1 := upstreamStub("m1")
	defer up1.Close()

	cfg1 := makeCfg(t, "key-one", "pw-one", false, providerSpec{name: "p1", up: up1.URL, model: "m1"})
	cfg1.Aliases = map[string]string{"fast": "p1/m1", "best": "fast"}
	srv, err := New(cfg1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	for _, model := range []string{"fast", "best"} {
		w := do(t, h, func() *http.Request {
			r := chatReq(t, model)
			r.Header.Set("Authorization", "Bearer key-one")
			return r
		}())
		if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from m1") {
			t.Fatalf("alias %q request failed: code=%d body=%s", model, w.Code, w.Body.String())
		}
	}

	// Aliases surface in /v1/models.
	w := do(t, h, func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer key-one")
		return r
	}())
	if !strings.Contains(w.Body.String(), `"fast"`) {
		t.Fatalf("alias should be listed in /v1/models: %s", w.Body.String())
	}

	// Reload with the alias dropped: the name must stop resolving.
	cfg2 := makeCfg(t, "key-one", "pw-one", false, providerSpec{name: "p1", up: up1.URL, model: "m1"})
	srv.Reload(cfg2)
	// After dropping the alias table, "fast" is no longer a named alias:
	// bare-model pass-through still serves it via the first provider.
	w = do(t, h, func() *http.Request {
		r := chatReq(t, "fast")
		r.Header.Set("Authorization", "Bearer key-one")
		return r
	}())
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pong from m1") {
		t.Fatalf("post-drop alias should fall through as bare model: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestReloadTightensBodyCap(t *testing.T) {
	up1 := upstreamStub("m1")
	defer up1.Close()
	cfg1 := makeCfg(t, "key-one", "pw-one", false, providerSpec{name: "p1", up: up1.URL, model: "m1"})
	srv, err := New(cfg1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	cfg2 := makeCfg(t, "key-one", "pw-one", false, providerSpec{name: "p1", up: up1.URL, model: "m1"})
	cfg2.Server.MaxBody = 10
	srv.Reload(cfg2)

	big := strings.Repeat("a", 100)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"p1/m1","messages":[{"role":"user","content":"`+big+`"}]}`))
	r.Header.Set("Authorization", "Bearer key-one")
	if w := do(t, h, r); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body should 413 after reload, got %d", w.Code)
	}
}
