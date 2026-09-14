package server

// Provider presets — the 9router-style catalog behind the dashboard's Add
// provider dialog: preconfigured api url / kind / model lists for the
// providers this gateway is built to front (GLM z.ai, xAI Grok — both wires,
// opencode-go + free tier), so picking one needs no typing.
//
// Storage contract: the BUILT-INS live in code (guaranteed on every install,
// nothing to seed), and the sqlite catalog (internal/store provider_presets)
// holds operator edits — overrides of a built-in by same name, and custom
// presets. A preset is a TEMPLATE, not live config: [[providers]] stays in
// TOML, applying a preset just fills the editor, which saves through the
// normal validated splice. Preset docs carry no credentials: api_key/keys are
// refused on save (accounts get keys at apply time; subscription accounts
// authenticate via [[oauth.accounts]] sign-in).
//
//	GET    /admin/config/presets  — merged catalog (name+doc, source flag)
//	PUT    /admin/config/presets  — upsert {name, doc:{kind,base_url,...}}
//	DELETE /admin/config/presets?name=X — drop a saved preset (404 if custom absent,
//	                                      400 on a built-in: hide it by overriding)

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// presetDoc is one catalog entry's payload — a subset of the provider-edit
// request shape (admin_config_edit.go), keys stripped by construction.
type presetDoc struct {
	Kind              string   `json:"kind"`
	BaseURL           string   `json:"base_url,omitempty"`
	Models            []string `json:"models,omitempty"`
	ResponsesModels   []string `json:"responses_models,omitempty"`
	SubscriptionQuota string   `json:"subscription_quota,omitempty"`
	// OAuthService, when set ("xai"), marks the subscription recipe: applying
	// the preset gives the first account row that service so the Sign-in
	// button appears without any TOML typing.
	OAuthService string `json:"oauth_service,omitempty"`
	Note         string `json:"note,omitempty"`
}

// builtinPresets is the code-side catalog. Names are the join key against
// operator overrides. The xAI models list is the current public set; a stale
// list costs nothing the editor cannot fix at apply time.
func builtinPresets() map[string]presetDoc {
	return map[string]presetDoc{
		"GLM (z.ai coding)": {
			Kind: "openai", BaseURL: "https://api.z.ai/api/coding/paas/v4",
			SubscriptionQuota: "zai",
			Note:              "paste your z.ai api key on the account row",
		},
		"xAI — Model API (api.x.ai)": {
			Kind: "openai", BaseURL: "https://api.x.ai",
			Models: []string{
				"grok-4.20-0309-non-reasoning", "grok-4.20-0309-reasoning", "grok-4.20-multi-agent-0309",
				"grok-4.3", "grok-4.5", "grok-4.6", "grok-build-0.1",
				"grok-imagine-image", "grok-imagine-image-2.0", "grok-imagine-image-quality",
				"grok-imagine-video", "grok-imagine-video-1.5",
			},
			ResponsesModels:   []string{"grok-4.5*"},
			SubscriptionQuota: "grok-cli",
			OAuthService:      "xai",
			Note:              "SuperGrok subscription: no api key — add an account named after your login, then Sign in",
		},
		"xAI — Grok Build proxy (cli-chat-proxy.grok.com)": {
			Kind: "openai-responses", BaseURL: "https://cli-chat-proxy.grok.com",
			Models:            []string{"grok-build-0.1", "grok-4.5"},
			SubscriptionQuota: "grok-cli",
			OAuthService:      "xai",
			Note:              "OAuth bearer only (never an xai- key); same device sign-in as the Model API",
		},
		"OpenCode Go (opencode-go)": {
			Kind:              "opencode",
			Models:            []string{"mimo-v2.5", "deepseek-v4.1-flash"},
			SubscriptionQuota: "opencode-go",
			Note:              "subscription wire: no base_url, one account row per key",
		},
		"OpenCode Free tier": {
			Kind:   "opencode-free",
			Models: []string{"big-pickle", "mimo-v2.5-free", "nemotron-3-ultra-free", "nemotron-3.5-lightning-free", "ling-3.0-flash-fin-free"},
			Note:   "no auth at all",
		},
	}
}

// validPresetKinds mirrors provider.Kind; keep in sync with internal/provider.
var validPresetKinds = map[string]bool{
	"openai": true, "anthropic": true, "gemini": true,
	"opencode": true, "opencode-free": true, "commandcode": true,
	"openai-responses": true, "cursor": true, "searxng": true,
}

func validatePresetDoc(doc presetDoc) string {
	if !validPresetKinds[doc.Kind] {
		return "unknown kind " + doc.Kind
	}
	if doc.BaseURL != "" {
		u, err := url.Parse(doc.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "base_url must be an absolute http(s) URL"
		}
	}
	switch doc.OAuthService {
	case "", "xai", "kilocode":
	default:
		return "oauth_service must be xai or kilocode"
	}
	return ""
}

// mergePresets overlays the sqlite catalog on the built-ins: a saved doc with
// a built-in's name replaces it; anything else is an added custom preset.
func mergePresets(stored []presetRowView) []map[string]any {
	docs := map[string]presetDoc{}
	source := map[string]string{}
	for n, d := range builtinPresets() {
		docs[n], source[n] = d, "built-in"
	}
	for _, r := range stored {
		var d presetDoc
		if json.Unmarshal([]byte(r.doc), &d) != nil {
			continue
		}
		docs[r.Name], source[r.Name] = d, "saved"
	}
	names := make([]string, 0, len(docs))
	for n := range docs {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"name": n, "source": source[n], "doc": docs[n]})
	}
	return out
}

type presetRowView struct {
	Name string
	doc  string
}

// storedPresets returns the sqlite catalog rows (empty when the store is
// absent — RAM mode).
func (s *Server) storedPresets() []presetRowView {
	var stored []presetRowView
	if s.st != nil {
		if rows, err := s.st.ListPresets(); err == nil {
			for _, p := range rows {
				stored = append(stored, presetRowView{Name: p.Name, doc: p.Doc})
			}
		}
	}
	return stored
}

// presetsJSON renders the merged catalog for template embedding.
func (s *Server) presetsJSON() string {
	b, err := json.Marshal(mergePresets(s.storedPresets()))
	if err != nil {
		return "[]"
	}
	return string(b)
}

func (s *Server) handleAdminPresetList(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	writeJSON(w, map[string]any{"presets": mergePresets(s.storedPresets())})
}

func (s *Server) handleAdminPresetPut(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	if s.st == nil {
		adminError(w, http.StatusServiceUnavailable, "presets need a data dir (memory stores nothing)")
		return
	}
	body, err := s.readBody(r)
	if err != nil {
		adminError(w, http.StatusBadRequest, "unreadable body: "+err.Error())
		return
	}
	var req struct {
		Name string    `json:"name"`
		Doc  presetDoc `json:"doc"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		adminError(w, http.StatusBadRequest, "preset name is required")
		return
	}
	if msg := validatePresetDoc(req.Doc); msg != "" {
		adminError(w, http.StatusBadRequest, msg)
		return
	}
	// Credentials are structurally impossible in presetDoc (no key fields);
	// the marshal round-trip below is the only serialization, so nothing
	// unexpected can ride along.
	docJSON, err := json.Marshal(req.Doc)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "encode preset: "+err.Error())
		return
	}
	if err := s.st.UpsertPreset(req.Name, string(docJSON)); err != nil {
		adminError(w, http.StatusInternalServerError, "save preset: "+err.Error())
		return
	}
	log.Printf("admin: provider preset %q saved", req.Name)
	writeJSON(w, map[string]any{"name": req.Name, "saved": true})
}

func (s *Server) handleAdminPresetDelete(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		adminUnauthorized(w)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		adminError(w, http.StatusBadRequest, "name is required")
		return
	}
	if _, isBuiltin := builtinPresets()[name]; isBuiltin {
		adminError(w, http.StatusBadRequest, "built-in presets cannot be deleted — override or ignore them")
		return
	}
	if s.st == nil {
		adminError(w, http.StatusNotFound, "no preset "+name)
		return
	}
	if err := s.st.DeletePreset(name); err != nil {
		adminError(w, http.StatusInternalServerError, "delete preset: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"name": name, "deleted": true})
}
