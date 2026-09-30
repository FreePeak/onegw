package provider

// The Codex wire contract, end to end against a stub backend-api: the CLI
// identity headers, the workspace id decoded off the bearer (and its ABSENCE
// for a bearer that is not a ChatGPT credential), the prompt-cache session
// affinity, and the curated catalog.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"onegw/internal/oauth"
)

// codexBearer builds an unsigned ChatGPT-shaped access token: only the
// payload is ever decoded, so no signing key belongs in a test.
func codexBearer(t *testing.T, accountID string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  "plus",
		},
	})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func TestCodexFingerprintOnChatAndModels(t *testing.T) {
	type call struct {
		path string
		hdr  http.Header
	}
	var seen []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, call{path: r.URL.Path, hdr: r.Header.Clone()})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	d := &Def{Name: "codex", Kind: KindCodex, BaseURL: srv.URL,
		Accounts: []Account{{Name: "main", APIKey: codexBearer(t, "workspace-abc")}}}
	acct := &d.Accounts[0]

	if _, err := d.Do(t.Context(), acct, "gpt-5.6-sol", http.Header{},
		bytes.NewReader([]byte(`{"model":"gpt-5.6-sol","input":"hi"}`)), true); err != nil {
		t.Fatalf("Do: %+v", err)
	}
	// The catalog is curated (see FetchModels), so this makes no upstream
	// call — only the chat POST must have happened, and on the one path.
	if _, _, err := d.FetchModels(t.Context(), acct); err != nil {
		t.Fatalf("FetchModels: %+v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("saw %d upstream calls, want 1 (chat only)", len(seen))
	}
	if seen[0].path != codexResponsesPath {
		t.Errorf("chat path = %q, want %q", seen[0].path, codexResponsesPath)
	}
	// The CLI identity triple gates models on the backend: a missing
	// User-Agent reads as an anonymous browser session, and a missing
	// Version 400s the newer ids with "requires a newer version of Codex".
	h := seen[0].hdr
	if ua := h.Get("User-Agent"); ua != oauth.CodexUserAgent {
		t.Errorf("User-Agent = %q, want %q", ua, oauth.CodexUserAgent)
	}
	if h.Get("originator") != oauth.CodexOriginator {
		t.Errorf("originator = %q", h.Get("originator"))
	}
	if h.Get("Version") != oauth.CodexClientVersion {
		t.Errorf("Version = %q", h.Get("Version"))
	}
	if h.Get("chatgpt-account-id") != "workspace-abc" {
		t.Errorf("chatgpt-account-id = %q", h.Get("chatgpt-account-id"))
	}
	if h.Get("Authorization") != "Bearer "+acct.APIKey {
		t.Errorf("Authorization = %q", h.Get("Authorization"))
	}
	if h.Get("session_id") == "" {
		t.Error("chat call carries no session_id (prompt-cache affinity)")
	}
}

func TestCodexNoWorkspaceForNonChatGPTBearer(t *testing.T) {
	h := http.Header{}
	setCodexFingerprint(h, oauth.Token{AccessToken: "sk-proj-opaque-key"}, "")
	if h.Get("chatgpt-account-id") != "" {
		t.Errorf("invented a workspace for a non-ChatGPT key: %q", h.Get("chatgpt-account-id"))
	}
	if h.Get("session_id") != "" {
		t.Errorf("session_id without a workspace: %q", h.Get("session_id"))
	}
	if h.Get("originator") != oauth.CodexOriginator {
		t.Errorf("originator = %q", h.Get("originator"))
	}
}

// Prompt-cache affinity: the SAME conversation must keep the SAME session id
// (a rotating one silently destroys the backend's cache hit rate), and the
// client's own id wins when it is one the backend accepts.
func TestCodexSessionIsStableAndClientWins(t *testing.T) {
	if a, b := codexSession("", "ws-1"), codexSession("", "ws-1"); a != b {
		t.Errorf("session id is not stable across calls: %q vs %q", a, b)
	}
	if codexSession("", "ws-1") == codexSession("", "ws-2") {
		t.Error("two workspaces share one derived session id")
	}
	if got := codexSession("conv-42", "ws-1"); got != "conv-42" {
		t.Errorf("client session id = %q, want conv-42", got)
	}
	// An off-shape client value is not forwarded: the backend 403s it.
	if got := codexSession("bad id with spaces", "ws-1"); got == "bad id with spaces" {
		t.Error("forwarded an off-shape client session id")
	}
}

// The same rule has to hold on the REAL request path, not just in the
// helper. Do() runs the codex fingerprint and then applySessionAffinity,
// which forwards the four session headers verbatim — so a client value the
// fingerprint rejected must not be reintroduced by the generic pass. It
// was: an off-shape id went upstream and the backend 403s those.
func TestCodexOffShapeClientSessionIsNotForwarded(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"id":"r1","output":[]}`))
	}))
	defer srv.Close()

	d := &Def{Name: "codex", Kind: KindCodex, BaseURL: srv.URL,
		Accounts: []Account{{Name: "main", APIKey: codexBearer(t, "ws-1")}}}
	client := http.Header{}
	client.Set("session_id", "bad value with spaces/and-slashes")
	if _, apiErr := d.Do(t.Context(), &d.Accounts[0], "gpt-6.1-sol", client,
		bytes.NewReader([]byte(`{"model":"gpt-6.1-sol"}`)), false); apiErr != nil {
		t.Fatalf("Do: %+v", apiErr)
	}
	if got == nil {
		t.Fatal("upstream was never called")
	}
	derived := codexSession("", "ws-1")
	if s := got.Get("session_id"); s != derived {
		t.Errorf("session_id = %q, want the derived %q (off-shape client value leaked upstream)", s, derived)
	}
}

func TestCodexFetchModelsIsCurated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("codex must not hit an upstream catalog, got %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	d := &Def{Name: "codex", Kind: KindCodex, BaseURL: srv.URL,
		Accounts: []Account{{Name: "main", APIKey: codexBearer(t, "ws")}}}
	body, status, err := d.FetchModels(t.Context(), &d.Accounts[0])
	if err != nil || status != 200 {
		t.Fatalf("FetchModels: %+v status=%d", err, status)
	}
	var out struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("catalog is not JSON: %v", err)
	}
	if len(out.Models) == 0 {
		t.Fatal("catalog is empty")
	}
	for _, m := range out.Models {
		if m == "" {
			t.Error("catalog carries an empty id")
		}
	}
}

// The workspace id reaches the wire through the ACCOUNT's stored credential,
// so the resolver must hand over the id_token as well as the access token.
// A resolver that returned only the bearer would send no chatgpt-account-id
// and every request would 403 upstream.
func TestCodexIdentityComesFromTheTokenProvider(t *testing.T) {
	var gotHdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHdr = r.Header.Clone()
		_, _ = w.Write([]byte(`{"id":"r1","output":[]}`))
	}))
	defer srv.Close()

	claims := codexBearer(t, "ws-from-id-token")
	d := &Def{Name: "codex", Kind: KindCodex, BaseURL: srv.URL,
		Accounts: []Account{{Name: "main"}}} // no static api_key: OAuth-only
	d.Accounts[0].SetTokenResolver(
		func(string) string { return "at-opaque" },
		func(string) string { return claims },
		"codex/main")
	if _, apiErr := d.Do(t.Context(), &d.Accounts[0], "gpt-6.1-sol", nil,
		bytes.NewReader([]byte(`{"model":"gpt-6.1-sol"}`)), false); apiErr != nil {
		t.Fatalf("Do: %+v", apiErr)
	}
	if gotHdr == nil {
		t.Fatal("upstream was never called")
	}
	if got := gotHdr.Get("Authorization"); got != "Bearer at-opaque" {
		t.Errorf("Authorization = %q, want the access token", got)
	}
	if got := gotHdr.Get("chatgpt-account-id"); got != "ws-from-id-token" {
		t.Errorf("chatgpt-account-id = %q, want the id_token's workspace", got)
	}
	if gotHdr.Get("session_id") == "" {
		t.Error("no session_id: without a workspace there is no cache affinity")
	}
}
