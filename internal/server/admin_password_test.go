package server

// PUT /admin/config/password: dashboard password change end to end —
// current-password re-proof, TOML splice, immediate effect, forced logout.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"onegw/internal/config"
)

const pwTOML = `# gateway config (password test fixture)
# second comment line

[server]
listen = "127.0.0.1:0"
admin_password = "admin"
data_dir = "@DATA@"

[auth]
keys = ["sk-test-gw"]

[[providers]]
name = "p1"
kind = "openai"
base_url = "http://127.0.0.1:1"
api_key = "sk-test-upstream-secret"
models = ["m1"]
`

func newPwSrv(t *testing.T) (*Server, http.Handler, string, string) {
	t.Helper()
	toml := strings.Replace(pwTOML, "@DATA@", t.TempDir(), 1)
	srv, h, path := newTestServerFromFile(t, toml)
	return srv, h, path, tomlDataDir(toml)
}

func tomlDataDir(toml string) string {
	for _, ln := range strings.Split(toml, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(ln), `data_dir = "`); ok {
			return strings.TrimSuffix(v, `"`)
		}
	}
	return ""
}

func putPassword(t *testing.T, h http.Handler, c *http.Cookie, header, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/admin/config/password", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if c != nil {
		r.AddCookie(c)
	}
	if header != "" {
		r.Header.Set("X-Admin-Password", header)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAdminPasswordChangeEndToEnd(t *testing.T) {
	srv, h, path, dataDir := newPwSrv(t)
	_ = srv

	c, code := loginForm(t, h, "admin")
	if c == nil || code != http.StatusSeeOther {
		t.Fatalf("login: code %d cookie %v", code, c)
	}

	// Wrong current password: 401, file untouched.
	if w := putPassword(t, h, c, "", `{"current_password":"wrong","new_password":"new-secret-9"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current: %d %s", w.Code, w.Body.String())
	}
	before, _ := os.ReadFile(path)

	// The change itself.
	w := putPassword(t, h, c, "", `{"current_password":"admin","new_password":"new-secret-9"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("change: %d %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["changed"] != true || resp["generated"] != false {
		t.Fatalf("response flags: %v", resp)
	}
	if _, echoed := resp["password"]; echoed {
		t.Fatal("a chosen password must not be echoed back")
	}

	// Old session is dead; the new credential works everywhere.
	if w := do(t, h, cookieReq(http.MethodGet, "/admin/api/v1/providers", c)); w.Code != http.StatusUnauthorized {
		t.Fatalf("old session still valid: %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/api/v1/providers", nil)
	r.Header.Set("X-Admin-Password", "new-secret-9")
	if w := do(t, h, r); w.Code != http.StatusOK {
		t.Fatalf("new header credential rejected: %d", w.Code)
	}
	r2 := httptest.NewRequest(http.MethodGet, "/admin/api/v1/providers", nil)
	r2.Header.Set("X-Admin-Password", "admin")
	if w := do(t, h, r2); w.Code != http.StatusUnauthorized {
		t.Fatalf("old header credential still valid: %d", w.Code)
	}
	// login with the old password must fail (no cookie issued)
	if cc, code := loginForm(t, h, "admin"); cc != nil {
		t.Fatalf("login with old password succeeded: %d", code)
	}

	// The TOML keeps every other line byte-for-byte and swaps only the key.
	after, _ := os.ReadFile(path)
	aTxt := string(after)
	if !strings.Contains(aTxt, `admin_password = "new-secret-9"`) {
		t.Fatalf("spliced file lacks the new password:\n%s", aTxt)
	}
	for _, ln := range strings.Split(string(before), "\n") {
		if strings.Contains(ln, "admin_password") {
			continue
		}
		if !strings.Contains(aTxt, ln) {
			t.Fatalf("splice dropped or rewrote line %q", ln)
		}
	}

	// The data-dir mirror follows, so a recreated container keeps the value.
	if b, err := os.ReadFile(filepath.Join(dataDir, config.AdminPasswordFile)); err != nil ||
		strings.TrimSpace(string(b)) != "new-secret-9" {
		t.Fatalf("mirror: %v %q", err, b)
	}

	// A SECOND change (generated path) right after must not deadlock, and a
	// plain reload afterwards still answers: cfgMu was released.
	w2 := putPassword(t, h, nil, "new-secret-9", `{"current_password":"new-secret-9"}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("second change: %d %s", w2.Code, w2.Body.String())
	}
	var resp2 map[string]any
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	if resp2["generated"] != true {
		t.Fatalf("generated flag: %v", resp2)
	}
	gen, _ := resp2["password"].(string)
	if gen == "" {
		t.Fatal("generated response must carry the credential once")
	}
	r3 := httptest.NewRequest(http.MethodPut, "/admin/config/reload", nil)
	r3.Header.Set("X-Admin-Password", gen)
	if w := do(t, h, r3); w.Code != http.StatusOK {
		t.Fatalf("reload after change: %d %s", w.Code, w.Body.String())
	}
	// Third change proves the mutex is free and the file swap is repeatable.
	w3 := putPassword(t, h, nil, gen, `{"current_password":"`+gen+`","new_password":"third-pass-7"}`)
	if w3.Code != http.StatusOK {
		t.Fatalf("third change: %d %s", w3.Code, w3.Body.String())
	}
}

func TestSpliceAdminPasswordShapes(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"replace existing key under [server]",
			"# c\n[server]\nlisten = \"1\"\nadmin_password = \"old\"\n\n[auth]\nkeys = []\n",
			"# c\n[server]\nlisten = \"1\"\nadmin_password = \"new-one\"\n\n[auth]\nkeys = []\n",
		},
		{
			"insert after [server] header",
			"[server]\nlisten = \"1\"\n",
			"[server]\nadmin_password = \"new-one\"\nlisten = \"1\"\n",
		},
		{
			"open a [server] table when none exists",
			"[auth]\nkeys = []\n",
			"[server]\nadmin_password = \"new-one\"\n\n[auth]\nkeys = []\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := spliceAdminPassword(strings.Split(tc.in, "\n"), "new-one")
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(out, "\n"); got != tc.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// A gateway started WITHOUT a config file (main.go's defaults branch) has a
// config path that was never written: the change must swap in memory and
// persist through the data-dir mirror, and a simulated restart must keep the
// changed password instead of generating a new one.
func TestAdminPasswordChangeWithoutConfigFile(t *testing.T) {
	dataDir := t.TempDir()
	cfg := &config.Config{}
	cfg.Defaults()
	cfg.Server.DataDir = dataDir
	cfg.Server.Listen = "127.0.0.1:0"
	if gen, err := cfg.GenerateAndStoreAdminPassword(); err != nil || !gen {
		t.Fatalf("boot generation: gen=%v err=%v", gen, err)
	}
	first := cfg.Server.AdminPassword
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	srv.SetConfigPath(filepath.Join(dataDir, "never-written.toml"))
	h := srv.Handler()

	w := putPassword(t, h, nil, first, `{"current_password":"`+first+`","new_password":"noconfig-pw-1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("change: %d %s", w.Code, w.Body.String())
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, config.AdminPasswordFile)); err != nil ||
		strings.TrimSpace(string(b)) != "noconfig-pw-1" {
		t.Fatalf("mirror after change: %v %q", err, b)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/api/v1/providers", nil)
	r.Header.Set("X-Admin-Password", "noconfig-pw-1")
	if w := do(t, h, r); w.Code != http.StatusOK {
		t.Fatalf("new credential: %d", w.Code)
	}

	// Restart equivalent: Defaults + the boot tail must adopt the marker.
	cfg2 := &config.Config{}
	cfg2.Defaults()
	cfg2.Server.DataDir = dataDir
	if gen, err := cfg2.GenerateAndStoreAdminPassword(); err != nil || gen {
		t.Fatalf("restart regenerated: gen=%v err=%v", gen, err)
	}
	if cfg2.Server.AdminPassword != "noconfig-pw-1" {
		t.Fatalf("restart lost the changed password: %q", cfg2.Server.AdminPassword)
	}
}

// A read-only config mount (docker `-v cfg:/etc/onegw:ro`) must not dead-end
// the change: it stays live in this process and persists through the data-dir
// mirror.
func TestAdminPasswordChangeReadOnlyConfigMount(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "etc")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfgDir, "onegw.toml")
	if err := os.WriteFile(path, []byte(strings.Replace(pwTOML, "@DATA@", dir, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	srv.SetConfigPath(path)
	h := srv.Handler()

	// Lock the directory so the atomic temp+rename cannot succeed.
	if err := os.Chmod(cfgDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })

	w := putPassword(t, h, nil, "admin", `{"current_password":"admin","new_password":"ro-mount-pw-1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("read-only mount change: %d %s", w.Code, w.Body.String())
	}
	// Live immediately...
	r := httptest.NewRequest(http.MethodGet, "/admin/api/v1/providers", nil)
	r.Header.Set("X-Admin-Password", "ro-mount-pw-1")
	if w := do(t, h, r); w.Code != http.StatusOK {
		t.Fatalf("new credential after read-only mount: %d", w.Code)
	}
	// ...the config file untouched...
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "ro-mount-pw-1") {
		t.Fatal("config file was written despite the read-only directory")
	}
	// ...and the mirror carries it into the next boot.
	if b, err := os.ReadFile(filepath.Join(dir, config.AdminPasswordFile)); err != nil ||
		strings.TrimSpace(string(b)) != "ro-mount-pw-1" {
		t.Fatalf("mirror: %v %q", err, b)
	}
}
