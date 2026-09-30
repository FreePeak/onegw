package server

// Presets: the catalog is built-ins from code overlaid by the sqlite store,
// save/delete round-trip through the admin API, secrets and junk kinds are
// refused, and built-ins survive deletion attempts.

import (
	"encoding/json"
	"net/http"
	"onegw/internal/oauth"
	"strings"
	"testing"
)

func TestPresetsAPIRoundTrip(t *testing.T) {
	// The catalog persists, so this needs a disk data dir — swap the
	// fixture's "memory" for one (s.st is nil in RAM, and PUT answers 503).
	dir := t.TempDir()
	toml := strings.Replace(editTestToml, `data_dir = "memory"`, `data_dir = "`+dir+`"`, 1)
	_, h, _ := newTestServerFromFile(t, toml)

	w := adminCall(t, h, http.MethodGet, "/admin/config/presets", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET presets: %d %s", w.Code, w.Body.String())
	}
	var list struct {
		Presets []struct {
			Name   string    `json:"name"`
			Source string    `json:"source"`
			Doc    presetDoc `json:"doc"`
		} `json:"presets"`
	}
	if json.Unmarshal(w.Body.Bytes(), &list) != nil {
		t.Fatal("bad GET shape")
	}
	names := map[string]string{}
	for _, p := range list.Presets {
		names[p.Name] = p.Source
	}
	// The operator-named trio must be present out of the box — that is the
	// "preconfigured, no need to input" promise.
	for _, want := range []string{"GLM (z.ai coding)", "xAI — Model API (api.x.ai)", "OpenCode Go (opencode-go)"} {
		if names[want] != "built-in" {
			t.Fatalf("built-in preset %q missing (have %v)", want, names)
		}
	}

	// Save an override of a built-in: it wins, source flips to saved.
	w = adminCall(t, h, http.MethodPut, "/admin/config/presets",
		`{"name":"GLM (z.ai coding)","doc":{"kind":"openai","base_url":"https://api.z.ai/api/paas/v4","models":["glm-4.7"]}}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT preset: %d %s", w.Code, w.Body.String())
	}
	w = adminCall(t, h, http.MethodGet, "/admin/config/presets", "", true)
	if !strings.Contains(w.Body.String(), `"base_url":"https://api.z.ai/api/paas/v4"`) ||
		!strings.Contains(w.Body.String(), `"source":"saved"`) {
		t.Fatalf("override not applied: %s", w.Body.String())
	}

	// Deletable custom, undeletable built-in.
	if w := adminCall(t, h, http.MethodDelete, "/admin/config/presets?name=GLM%20(z.ai%20coding)", "", true); w.Code != http.StatusBadRequest {
		t.Fatalf("delete built-in: want 400, got %d %s", w.Code, w.Body.String())
	}
	w = adminCall(t, h, http.MethodPut, "/admin/config/presets",
		`{"name":"mine","doc":{"kind":"anthropic","base_url":"https://api.anthropic.com"}}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT custom: %d %s", w.Code, w.Body.String())
	}
	if w := adminCall(t, h, http.MethodDelete, "/admin/config/presets?name=mine", "", true); w.Code != http.StatusOK {
		t.Fatalf("delete custom: %d %s", w.Code, w.Body.String())
	}
	w = adminCall(t, h, http.MethodGet, "/admin/config/presets", "", true)
	if strings.Contains(w.Body.String(), `"name":"mine"`) {
		t.Fatalf("deleted preset still listed: %s", w.Body.String())
	}

	// Rejections: unknown kind, non-http base_url.
	for _, body := range []string{
		`{"name":"x","doc":{"kind":"telepathy"}}`,
		`{"name":"x","doc":{"kind":"openai","base_url":"ftp://nope"}}`,
		`{"name":"","doc":{"kind":"openai"}}`,
	} {
		if w := adminCall(t, h, http.MethodPut, "/admin/config/presets", body, true); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d %s", body, w.Code, w.Body.String())
		}
	}
}

// The dashboard mirrors a provider's recipe into the preset catalog on every
// save, including the account row's OAuth service — and it swallows the
// response (.catch(() => {})). So a service the validator rejects means the
// codex recipe is silently never saved, and re-applying the preset leaves the
// provider with no Sign-in button. The allowlist must therefore be the
// PROFILE REGISTRY, not a hand-kept pair.
func TestPresetAcceptsEveryRegisteredOAuthService(t *testing.T) {
	dir := t.TempDir()
	toml := strings.Replace(editTestToml, `data_dir = "memory"`, `data_dir = "`+dir+`"`, 1)
	_, h, _ := newTestServerFromFile(t, toml)

	for _, svc := range oauth.Providers() {
		w := adminCall(t, h, http.MethodPut, "/admin/config/presets",
			`{"name":"recipe-`+svc+`","doc":{"kind":"codex","models":["gpt-6.1-sol"],"oauth_service":"`+svc+`"}}`, true)
		if w.Code != http.StatusOK {
			t.Errorf("preset for service %q rejected: %d %s", svc, w.Code, w.Body.String())
		}
	}
	// Something that is not a profile must still be refused, with the name in
	// the message so the operator can see which value was wrong.
	w := adminCall(t, h, http.MethodPut, "/admin/config/presets",
		`{"name":"bogus","doc":{"kind":"codex","oauth_service":"nope"}}`, true)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "nope") {
		t.Fatalf("unknown service not refused: %d %s", w.Code, w.Body.String())
	}
}
