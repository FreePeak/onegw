package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"

	"onegw/internal/config"
)

// testConfigToml renders a starting config file for the admin config
// tests. The admin password lives in the file so the auth gate is
// exercised end to end; all keys are obviously fake (sk-test-...).
const testConfigToml = `# gateway config (test fixture)
[server]
data_dir = "memory"
admin_password = "pw-test"

[auth]
# gateway client keys
keys = ["key-a"]

[[providers]]
name = "p1"
kind = "openai"
base_url = "%s"
api_key = "sk-test-upstream-secret"
models = ["m1"]

[[providers.accounts]]
name = "acct1"
api_key = "sk-test-account-secret"

[[combo]]
name = "c1"
targets = ["p1/m1"]
`

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"ONEGW_KEYS", "ONEGW_ADMIN_PASSWORD", "ONEGW_PROVIDER_P1_KEY", "ONEGW_DATA_DIR", "ONEGW_CONFIG"} {
		t.Setenv(k, "")
	}
}

// newTestServerFromFile loads tomlText from a temp file and builds a
// server wired to it exactly like main does.
func newTestServerFromFile(t *testing.T, tomlText string) (*Server, http.Handler, string) {
	t.Helper()
	clearConfigEnv(t)
	path := filepath.Join(t.TempDir(), "onegw.toml")
	if err := os.WriteFile(path, []byte(tomlText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(srv.Close)
	srv.SetConfigPath(path)
	return srv, srv.Handler(), path
}

// adminCfgReq builds a request; withAuth sets the X-Admin-Password header
// (master's adminOK is header-only constant-time).
func adminCfgReq(method, target string, body *strings.Reader, withAuth bool) *http.Request {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, body)
		r.Header.Set("Content-Type", "application/json")
	}
	if withAuth {
		r.Header.Set("X-Admin-Password", "pw-test")
	}
	return r
}

func adminCall(t *testing.T, h http.Handler, method, target, body string, withAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	var br *strings.Reader
	if body != "" {
		br = strings.NewReader(body)
	}
	return do(t, h, adminCfgReq(method, target, br, withAuth))
}

func chatWithKey(t *testing.T, h http.Handler, model, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := chatReq(t, model)
	r.Header.Set("Authorization", "Bearer "+key)
	return do(t, h, r)
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAdminConfigGetMasksSecrets(t *testing.T) {
	_, h, path := newTestServerFromFile(t, fmt.Sprintf(testConfigToml, "http://unused.local"))

	w := adminCall(t, h, http.MethodGet, "/admin/config", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/config: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, secret := range []string{"sk-test-upstream-secret", "sk-test-account-secret", "key-a", "pw-test"} {
		if strings.Contains(body, secret) {
			t.Fatalf("GET /admin/config leaked %q in body:\n%s", secret, body)
		}
	}
	if !strings.Contains(body, "***(len=") {
		t.Fatalf("expected mask markers in:\n%s", body)
	}
	if w.Header().Get("X-OneGW-Config-Path") != path {
		t.Fatalf("config path header: %q, want %q", w.Header().Get("X-OneGW-Config-Path"), path)
	}
	if !strings.Contains(body, "# config_file = "+path) {
		t.Fatalf("config_file meta missing:\n%s", body)
	}
	// The masked view must still be well-formed TOML.
	var v map[string]any
	if _, err := toml.Decode(body, &v); err != nil {
		t.Fatalf("masked view is not valid TOML: %v\n%s", err, body)
	}

	if w := adminCall(t, h, http.MethodGet, "/admin/config", "", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /admin/config: %d", w.Code)
	}
}

func TestAdminConfigReloadEndToEnd(t *testing.T) {
	up := upstreamStub("m1")
	defer up.Close()
	start := fmt.Sprintf(testConfigToml, up.URL)
	_, h, path := newTestServerFromFile(t, start)

	// Happy path: the operator swaps the client key on disk, then reloads
	// over HTTP — no shell, same contract as SIGHUP.
	swapped := strings.Replace(start, `keys = ["key-a"]`, `keys = ["key-b"]`, 1)
	if err := os.WriteFile(path, []byte(swapped), 0o600); err != nil {
		t.Fatal(err)
	}
	w := adminCall(t, h, http.MethodPut, "/admin/config/reload", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("reload: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Reloaded bool `json:"reloaded"`
		AuthKeys int  `json:"auth_keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("reload response: %v (%s)", err, w.Body.String())
	}
	if !resp.Reloaded || resp.AuthKeys != 1 {
		t.Fatalf("reload summary wrong: %s", w.Body.String())
	}
	if w := chatWithKey(t, h, "p1/m1", "key-b"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "pong from m1") {
		t.Fatalf("new key should route after reload: %d %s", w.Code, w.Body.String())
	}
	if w := chatWithKey(t, h, "p1/m1", "key-a"); w.Code != http.StatusUnauthorized {
		t.Fatalf("old key should 401 after reload: %d", w.Code)
	}

	// Invalid config: 400 with the validation error, old state untouched.
	broken := strings.Replace(swapped, `kind = "openai"`, ``, 1)
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	w = adminCall(t, h, http.MethodPut, "/admin/config/reload", "", true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid config should 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "kind") {
		t.Fatalf("validation error not surfaced: %s", w.Body.String())
	}
	if w := chatWithKey(t, h, "p1/m1", "key-b"); w.Code != http.StatusOK {
		t.Fatalf("old state disturbed after rejected reload: %d", w.Code)
	}

	if w := adminCall(t, h, http.MethodPut, "/admin/config/reload", "", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated reload: %d", w.Code)
	}
}

func TestAdminConfigReloadWithoutPath(t *testing.T) {
	clearConfigEnv(t)
	cfg := makeCfg(t, "k", "pw", false)
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	h := srv.Handler()

	// Reload and PATCH both refuse to guess a file.
	r := httptest.NewRequest(http.MethodPut, "/admin/config/reload", nil)
	r.Header.Set("X-Admin-Password", "pw")
	if w := do(t, h, r); w.Code != http.StatusBadRequest {
		t.Fatalf("reload without config path: %d", w.Code)
	}
	r = httptest.NewRequest(http.MethodPatch, "/admin/config/keys", strings.NewReader(`{"add":["k2"]}`))
	r.Header.Set("X-Admin-Password", "pw")
	if w := do(t, h, r); w.Code != http.StatusBadRequest {
		t.Fatalf("patch without config path: %d", w.Code)
	}
}

func TestAdminConfigKeysEndToEnd(t *testing.T) {
	up := upstreamStub("m1")
	defer up.Close()
	_, h, path := newTestServerFromFile(t, fmt.Sprintf(testConfigToml, up.URL))

	patch := func(body string) *httptest.ResponseRecorder {
		return adminCall(t, h, http.MethodPatch, "/admin/config/keys", body, true)
	}

	// Unauthenticated PATCH must not touch the file.
	orig := mustReadFile(t, path)
	if w := adminCall(t, h, http.MethodPatch, "/admin/config/keys", `{"add":["key-x"]}`, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PATCH keys: %d", w.Code)
	}
	if mustReadFile(t, path) != orig {
		t.Fatal("unauthenticated PATCH mutated the config file")
	}

	// Add: the duplicate key-a is skipped, key-b is added.
	w := patch(`{"add":["key-b","key-a"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch keys: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Added   int `json:"added"`
		Removed int `json:"removed"`
		Total   int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("patch response: %v (%s)", err, w.Body.String())
	}
	if resp.Added != 1 || resp.Total != 2 {
		t.Fatalf("add summary wrong: %s", w.Body.String())
	}
	if w := chatWithKey(t, h, "p1/m1", "key-b"); w.Code != http.StatusOK {
		t.Fatalf("key-b should work after PATCH: %d %s", w.Code, w.Body.String())
	}

	// Remove: key-a stops working, key-b keeps working.
	w = patch(`{"remove":["key-a"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("remove keys: %d %s", w.Code, w.Body.String())
	}
	if w := chatWithKey(t, h, "p1/m1", "key-a"); w.Code != http.StatusUnauthorized {
		t.Fatalf("key-a should 401 after removal: %d", w.Code)
	}
	if w := chatWithKey(t, h, "p1/m1", "key-b"); w.Code != http.StatusOK {
		t.Fatalf("key-b should still work: %d", w.Code)
	}

	// On-disk result: key-a gone, key-b present, other sections and
	// comments preserved byte-for-byte.
	s := mustReadFile(t, path)
	if strings.Contains(s, "key-a") {
		t.Fatalf("key-a still on disk:\n%s", s)
	}
	if !strings.Contains(s, `"key-b"`) {
		t.Fatalf("key-b missing on disk:\n%s", s)
	}
	for _, want := range []string{
		"# gateway config (test fixture)", "[server]", `data_dir = "memory"`,
		"[auth]", "# gateway client keys", "[[providers]]", "sk-test-upstream-secret",
		"sk-test-account-secret", "[[combo]]", `name = "c1"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("config file lost %q after keys patch:\n%s", want, s)
		}
	}

	// Removing the last key is refused: the file and live state are intact.
	w = patch(`{"remove":["key-b"]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("last-key removal should 400, got %d: %s", w.Code, w.Body.String())
	}
	if mustReadFile(t, path) != s {
		t.Fatal("refused last-key removal mutated the file")
	}
	if w := chatWithKey(t, h, "p1/m1", "key-b"); w.Code != http.StatusOK {
		t.Fatalf("key-b disturbed by refused patch: %d", w.Code)
	}

	// Malformed input.
	if w := patch(`{`); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON: %d", w.Code)
	}
	if w := patch(`{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty patch: %d", w.Code)
	}
	if w := patch(`{"add":["  padded  "]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("whitespace-padded key: %d", w.Code)
	}
}

func TestSpliceAuthKeysPreservesFile(t *testing.T) {
	raw := `# top comment
[server]
listen = "127.0.0.1:9999"

[auth]
# gateway client keys
keys = [
  "one",   # first
  "two",
]

[[providers]]
name = "p"
kind = "openai"
api_key = "sk-x"
`
	out, keys, added, removed, err := spliceAuthKeys(strings.Split(raw, "\n"), []string{"three"}, []string{"one"})
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	got := strings.Join(out, "\n")
	if added != 1 || removed != 1 {
		t.Fatalf("added=%d removed=%d, want 1/1", added, removed)
	}
	if len(keys) != 2 || keys[0] != "two" || keys[1] != "three" {
		t.Fatalf("key set wrong: %q", keys)
	}
	if strings.Contains(got, `"one"`) {
		t.Fatalf("removed key still present:\n%s", got)
	}
	// Untouched sections are byte-identical.
	if !strings.Contains(got, "# top comment\n[server]\nlisten = \"127.0.0.1:9999\"") {
		t.Fatalf("header/server section disturbed:\n%s", got)
	}
	if !strings.Contains(got, "[[providers]]\nname = \"p\"\nkind = \"openai\"\napi_key = \"sk-x\"") {
		t.Fatalf("providers section disturbed:\n%s", got)
	}
	// Comments inside [auth] that stand on their own line survive.
	if !strings.Contains(got, "# gateway client keys") {
		t.Fatalf("auth comment lost:\n%s", got)
	}
	// The rewritten keys line is valid TOML with the expected set.
	if !strings.Contains(got, `keys = ["two", "three"]`) {
		t.Fatalf("keys line wrong:\n%s", got)
	}
}

func TestSpliceAuthKeysEdgeShapes(t *testing.T) {
	t.Run("section absent appends", func(t *testing.T) {
		raw := "# header\n\n[server]\nlisten = \"x\"\n"
		out, keys, added, _, err := spliceAuthKeys(strings.Split(raw, "\n"), []string{"k1"}, nil)
		if err != nil || added != 1 || len(keys) != 1 {
			t.Fatalf("splice: %v %q", err, keys)
		}
		got := strings.Join(out, "\n")
		if !strings.Contains(got, "[auth]\nkeys = [\"k1\"]\n") {
			t.Fatalf("appended section wrong:\n%q", got)
		}
		if !strings.Contains(got, "[server]\nlisten = \"x\"\n") {
			t.Fatalf("existing section disturbed:\n%q", got)
		}
	})
	t.Run("section without keys line", func(t *testing.T) {
		raw := "[auth]\n# none yet\n"
		out, _, _, _, err := spliceAuthKeys(strings.Split(raw, "\n"), []string{"k1"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Join(out, "\n")
		if !strings.Contains(got, "[auth]\n# none yet\nkeys = [\"k1\"]\n") {
			t.Fatalf("insert at section end wrong:\n%q", got)
		}
	})
	t.Run("single-quoted and escaped elements", func(t *testing.T) {
		raw := "[auth]\nkeys = ['lit\"eral', \"quo\\\"ted\"]\n"
		_, keys, _, _, err := spliceAuthKeys(strings.Split(raw, "\n"), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(keys) != 2 || keys[0] != `lit"eral` || keys[1] != `quo"ted` {
			t.Fatalf("parse of exotic strings wrong: %q", keys)
		}
	})
	t.Run("unterminated array refused", func(t *testing.T) {
		raw := "[auth]\nkeys = [\"a\",\n"
		if _, _, _, _, err := spliceAuthKeys(strings.Split(raw, "\n"), []string{"b"}, nil); err == nil {
			t.Fatal("expected refusal for unterminated array")
		}
	})
	t.Run("non-string element refused", func(t *testing.T) {
		raw := "[auth]\nkeys = [\"a\", 42]\n"
		if _, _, _, _, err := spliceAuthKeys(strings.Split(raw, "\n"), nil, nil); err == nil {
			t.Fatal("expected refusal for non-string element")
		}
	})
	t.Run("refuse removing last key", func(t *testing.T) {
		raw := "[auth]\nkeys = [\"only\"]\n"
		if _, _, _, _, err := spliceAuthKeys(strings.Split(raw, "\n"), nil, []string{"only"}); err == nil {
			t.Fatal("expected refusal to remove the last key")
		}
	})
}

func TestWriteConfigAtomicallyRefusesInvalid(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "onegw.toml")
	good := fmt.Sprintf(testConfigToml, "http://unused.local")
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigAtomically(path, []byte("this is not toml")); err == nil {
		t.Fatal("expected rejection of invalid TOML")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, []byte(good)) {
		t.Fatal("file clobbered by refused write")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestAdminConfigAliasesEndToEnd(t *testing.T) {
	up := upstreamStub("m1")
	defer up.Close()
	_, h, path := newTestServerFromFile(t, fmt.Sprintf(testConfigToml, up.URL)+"\n[aliases]\nfast = \"p1/m1\"\n")

	patch := func(body string) *httptest.ResponseRecorder {
		return adminCall(t, h, http.MethodPatch, "/admin/config/aliases", body, true)
	}

	// Unauthenticated PATCH must not touch the file.
	orig := mustReadFile(t, path)
	if w := adminCall(t, h, http.MethodPatch, "/admin/config/aliases", `{"set":{"x":"p1/m1"}}`, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PATCH aliases: %d", w.Code)
	}
	if mustReadFile(t, path) != orig {
		t.Fatal("unauthenticated PATCH mutated the config file")
	}

	// Replace fast in place, append slow.
	w := patch(`{"set":{"fast":"c1","slow":"p1/m1"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch aliases: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Set   int `json:"set"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response: %v (%s)", err, w.Body.String())
	}
	if resp.Set != 2 || resp.Total != 2 {
		t.Fatalf("set summary wrong: %s", w.Body.String())
	}
	s := mustReadFile(t, path)
	if !strings.Contains(s, "[aliases]\nfast = \"c1\"\nslow = \"p1/m1\"\n") {
		t.Fatalf("aliases section wrong after set:\n%s", s)
	}

	// Delete removes only the named line.
	w = patch(`{"delete":["fast"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("delete alias: %d %s", w.Code, w.Body.String())
	}
	s = mustReadFile(t, path)
	if strings.Contains(s, "fast") {
		t.Fatalf("deleted alias still on disk:\n%s", s)
	}
	if !strings.Contains(s, "slow = \"p1/m1\"") {
		t.Fatalf("surviving alias lost:\n%s", s)
	}

	// Invalid name: 400, file untouched.
	before := mustReadFile(t, path)
	if w := patch(`{"set":{"a/b":"p1/m1"}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid alias name should 400, got %d", w.Code)
	}
	if mustReadFile(t, path) != before {
		t.Fatal("refused alias patch mutated the file")
	}

	// Section absent: appended at EOF.
	_, h2, path2 := newTestServerFromFile(t, fmt.Sprintf(testConfigToml, up.URL))
	w = adminCall(t, h2, http.MethodPatch, "/admin/config/aliases", `{"set":{"q":"p1/m1"}}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("append aliases section: %d %s", w.Code, w.Body.String())
	}
	s = mustReadFile(t, path2)
	if !strings.HasSuffix(s, "[aliases]\nq = \"p1/m1\"\n") {
		t.Fatalf("appended aliases section wrong:\n%q", s)
	}
}

// Concurrent PATCHes are serialized: every read-modify-write cycle lands,
// no lost updates (run under -race too).
func TestAdminConfigPatchConcurrent(t *testing.T) {
	up := upstreamStub("m1")
	defer up.Close()
	_, h, path := newTestServerFromFile(t, fmt.Sprintf(testConfigToml, up.URL))

	const n = 12
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"add":["key-%02d"]}`, i)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, adminCfgReq(http.MethodPatch, "/admin/config/keys", strings.NewReader(body), true))
			if w.Code != http.StatusOK {
				errs[i] = fmt.Errorf("patch %d: %d %s", i, w.Code, w.Body.String())
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("final file invalid: %v", err)
	}
	if len(cfg.Auth.KeyList) != n+1 {
		t.Fatalf("lost updates: %d keys on disk, want %d", len(cfg.Auth.KeyList), n+1)
	}
}
