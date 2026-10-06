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
	"strconv"
	"strings"

	"onegw/internal/oauth"
)

// validOAuthService reports whether name is a registered OAuth profile —
// the same registry the dashboard's service <select> is rendered from, so
// the two cannot drift.
func validOAuthService(name string) bool {
	for _, s := range oauth.Providers() {
		if s == name {
			return true
		}
	}
	return false
}

// presetDoc is one catalog entry's payload — a subset of the provider-edit
// request shape (admin_config_edit.go), keys stripped by construction.
type presetDoc struct {
	Kind              string   `json:"kind"`
	BaseURL           string   `json:"base_url,omitempty"`
	Models            []string `json:"models,omitempty"`
	ResponsesModels   []string `json:"responses_models,omitempty"`
	QuotaWindow       string   `json:"quota_window,omitempty"`
	QuotaLimitTokens  int64    `json:"quota_limit_tokens,omitempty"`
	QuotaLimitReqs    int64    `json:"quota_limit_requests,omitempty"`
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
		"Freebuff (Codebuff free tier)": {
			Kind:              "freebuff",
			Models:            []string{"deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-pro", "openai/gpt-5.6-luna", "minimax/minimax-m3", "mimo/mimo-v2.5"},
			SubscriptionQuota: "freebuff",
			Note:              "Codebuff CLI auth token (authToken in ~/.config/manicode/credentials.json); multi-step freebuff executor",
		},
		"Xiaomi Mimo (token plan)": {
			Kind:              "openai",
			BaseURL:           "https://token-plan-sgp.xiaomimimo.com/v1",
			Models:            []string{"mimo-v2.5", "mimo-v2.5-asr", "mimo-v2.5-pro", "mimo-v2.5-tts", "mimo-v2.5-tts-voiceclone", "mimo-v2.5-tts-voicedesign"},
			QuotaWindow:       "monthly",
			QuotaLimitTokens:  4_100_000_000,
			SubscriptionQuota: "xiaomi-tokenplan",
			Note:              "paste your xiaomi api key on the account row; set dashboard_token to the platform.xiaomimimo.com browser session cookie (userId + api-platform_serviceToken)",
		},
		// Codex/ChatGPT and plain OpenAI were the two catalog entries the
		// dashboard could not fill in: both kinds exist in the gateway
		// (provider.KindCodex / KindOpenAI) but nothing offered them, so an
		// operator had to hand-write base_url + kind + the OAuth service.
		"Codex — ChatGPT Plus/Pro (chatgpt.com)": {
			Kind:              "codex",
			SubscriptionQuota: "codex",
			OAuthService:      "codex",
			Note:              "ChatGPT subscription wire (backend-api/codex/responses): NO api key — base_url defaults to https://chatgpt.com and models default to the curated Codex catalog. Add an account row, pick codex in its OAuth select, then Sign in (browser + PKCE on 127.0.0.1:1455).",
		},
		"OpenAI API (api.openai.com)": {
			Kind: "openai", BaseURL: "https://api.openai.com/v1",
			Models: []string{
				"gpt-5.6", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
				"gpt-5.5", "gpt-5.5-pro", "gpt-5.4", "gpt-5.4-pro", "gpt-5.4-mini", "gpt-5.4-nano",
				"gpt-4.1", "gpt-4.1-mini", "gpt-4.1-nano",
				"gpt-4o", "gpt-4o-2024-11-20", "gpt-4o-mini",
				"o3", "o3-mini", "o4-mini",
			},
			// The 5.6 family and both -pro ids answer ONLY on /v1/responses.
			ResponsesModels: []string{"gpt-5.6*", "gpt-5.5-pro", "gpt-5.4-pro"},
			Note:            "official OpenAI API, api-key auth: paste your sk- key on the account row",
		},
	}
}

// validPresetKinds mirrors provider.Kind; keep in sync with internal/provider.
var validPresetKinds = map[string]bool{
	"openai": true, "anthropic": true, "gemini": true,
	"opencode": true, "opencode-free": true, "commandcode": true,
	"openai-responses": true, "cursor": true, "searxng": true,
	"codex": true,
	"cline": true, "mistral": true, "systemone": true, "freebuff": true,
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
	// The preset's service must be a REGISTERED OAuth profile, not a
	// hand-kept list: the account row's Sign-in button is what this field
	// exists to enable, and a service the profile registry does not know
	// would leave the applied provider with no way to sign in. (It was
	// xai/kilocode only, so saving a codex/cline provider's recipe from the
	// dashboard was rejected — and the UI swallows that 400.)
	if doc.OAuthService != "" && !validOAuthService(doc.OAuthService) {
		return "oauth_service " + strconv.Quote(doc.OAuthService) + " is not a known OAuth service"
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
