package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"onegw/internal/config"
	"onegw/internal/oauth"
)

// kind = "codex" over a stubbed chatgpt.com: the gateway must send the CLI
// identity headers, the workspace id decoded off the bearer, force stream=true,
// keep store=false, and answer a NON-streaming client by aggregating the SSE.
func TestCodexEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var gotHdr http.Header
	var gotBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		mu.Lock()
		gotHdr = r.Header.Clone()
		gotBody = append([]byte(nil), b...)
		mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/backend-api/codex/responses") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.1-codex\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	// A ChatGPT-shaped bearer: header.payload.sig with the workspace claim.
	bearer := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(
		[]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"ws-123"}}`)) + ".sig"

	dir := t.TempDir()
	t.Setenv("ONEGW_DATA_DIR", dir)
	tokPath := dir + "/oauth-tokens.json"
	tok := `{"tokens":{"codex/me":{"access_token":"` + bearer + `","refresh_token":"r","expires_at":"2099-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(tokPath, []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgText := `
[[providers]]
name = "codex"
kind = "codex"
base_url = "` + up.URL + `"
subscription_quota = "codex"
models = ["gpt-5.1-codex"]

[[providers.accounts]]
name = "me"

[[oauth.accounts]]
provider = "codex"
account = "me"
service = "codex"
`
	clearConfigEnv(t)
	t.Setenv("ONEGW_DATA_DIR", dir) // after clear, which zeroes it
	path := dir + "/onegw.toml"
	if err := os.WriteFile(path, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("server rejected the codex config: %v", err)
	}
	defer srv.Close()

	body := `{"model":"codex/gpt-5.1-codex","messages":[{"role":"user","content":"ping"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-client")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, `"content":"pong"`) {
		t.Fatalf("non-streaming aggregation failed: %s", out)
	}

	mu.Lock()
	defer mu.Unlock()
	want := map[string]string{
		"Originator":         "codex_cli_rs",
		"Chatgpt-Account-Id": "ws-123",
		"Authorization":      "Bearer " + bearer,
	}
	for k, v := range want {
		if got := gotHdr.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	if !strings.Contains(strings.ToLower(gotHdr.Get("User-Agent")), "codex") {
		t.Errorf("User-Agent = %q", gotHdr.Get("User-Agent"))
	}
	if gotHdr.Get("Version") == "" {
		t.Error("Version header missing")
	}
	if gotHdr.Get("Session_id") == "" {
		t.Error("session_id missing")
	}
	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("upstream body: %v (%s)", err, gotBody)
	}
	if sent["stream"] != true {
		t.Errorf("stream not forced: %s", gotBody)
	}
	if sent["store"] != false {
		t.Errorf("store not false: %s", gotBody)
	}
	if _, ok := sent["input"]; !ok {
		t.Errorf("responses body has no input: %s", gotBody)
	}
}

// The codex profile pins the loopback redirect ChatGPT has allow-listed for
// the Codex CLI (127.0.0.1:1455/auth/callback). A dashboard login that binds
// the shared default port instead gets a redirect_uri the vendor refuses, so
func TestCodexBrowserLoginUsesRegisteredCallback(t *testing.T) {
	idp := &oauthIdP{}
	_, upstreamURL := idp.start(t)
	fixture := `
[server]
data_dir = "memory"
admin_password = "pw-test"

[auth]
keys = ["key-a"]

[[providers]]
name = "codex"
kind = "codex"
base_url = "` + upstreamURL + `"
models = ["gpt-5.1-codex"]

[[providers.accounts]]
name = "main"

[[oauth.accounts]]
provider = "codex"
account = "main"
service = "codex"
`
	_, h, _ := newTestServerFromFile(t, fixture)
	w := adminCall(t, h, http.MethodPost, "/admin/config/oauth/login?key=codex/main", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	bp := decodeJSON[browserPrompt](t, w.Body.String())
	if bp.Prompt == nil || bp.Prompt.Mode != "browser" {
		t.Fatalf("codex must use the browser dialect: %s", w.Body.String())
	}
	authURL, err := url.Parse(bp.Prompt.VerifyURL)
	if err != nil {
		t.Fatalf("authorize url %q: %v", bp.Prompt.VerifyURL, err)
	}
	if got := authURL.Query().Get("redirect_uri"); got != oauth.CodexRedirectURI {
		t.Fatalf("redirect_uri = %q, want the registered %q", got, oauth.CodexRedirectURI)
	}
}
