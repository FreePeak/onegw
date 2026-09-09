package main

// Regression for #63: PUT /admin/config/reload (the dashboard Reload
// button) must keep the outer-mux /admin/update handler's config copy in
// sync. Before the SetOnConfigReload hook, the handler authenticated
// against the PROCESS-START password while every other admin route saw the
// reloaded one — the dashboard's Update now 401'd until a SIGHUP. The hook
// mirrors what the SIGHUP branch already did (curCfg.Store(fresh)).

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"onegw/internal/config"
	"onegw/internal/server"
	"onegw/internal/update"
)

const fix63TOML = `
[server]
listen = "127.0.0.1:0"
admin_password = "old-pw"
data_dir = "memory"

[auth]
keys = ["sk-test-gw"]

[[providers]]
name = "p1"
kind = "openai"
base_url = "http://127.0.0.1:1"
api_key = "sk-fake"
models = ["m1"]

[update]
check_interval = "0"
`

func loadTOMLForTest(t *testing.T, text string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "onegw.toml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load toml: %v", err)
	}
	return cfg
}

// newHookedStack boots a server plus the outer mux with /admin/update
// authenticated against a curCfg atomic — the exact wiring of runGateway,
// including the #63 hook registration. The server's config file points at
// newPwTOML so a reload swaps the password to "new-pw".
func newHookedStack(t *testing.T) (http.Handler, string) {
	t.Helper()
	cfg := loadTOMLForTest(t, fix63TOML)
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	svc := update.NewService(func() update.Settings { return update.Settings{} })
	svc.Start()
	t.Cleanup(svc.Stop)

	// Reload source of truth: same file, NEW password (what a dashboard
	// reload would pull in).
	newPwPath := filepath.Join(t.TempDir(), "new.toml")
	newText := strings.Replace(fix63TOML, `"old-pw"`, `"new-pw"`, 1)
	if err := os.WriteFile(newPwPath, []byte(newText), 0o600); err != nil {
		t.Fatalf("write new toml: %v", err)
	}
	srv.SetConfigPath(newPwPath)

	var curCfg atomic.Pointer[config.Config]
	curCfg.Store(cfg)

	outer := http.NewServeMux()
	outer.Handle("/admin/update", updateHandler(svc, curCfg.Load))
	outer.Handle("/", srv.Handler())
	srv.SetUpdater(svc)
	srv.SetOnConfigReload(curCfg.Store) // the #63 fix, as main wires it
	return outer, newPwPath
}

// TestDashboardReloadKeepsUpdateEndpointAuthed replays the live failure:
// reload the config through the HTTP endpoint (new password), then hit
// /admin/update with the NEW password — the hook must have synced curCfg.
func TestDashboardReloadKeepsUpdateEndpointAuthed(t *testing.T) {
	stack, _ := newHookedStack(t)

	// The dashboard's Reload button, against the old password (still valid
	// pre-reload).
	r := httptest.NewRequest(http.MethodPut, "/admin/config/reload", nil)
	r.Header.Set("X-Admin-Password", "old-pw")
	w0 := httptest.NewRecorder()
	stack.ServeHTTP(w0, r)
	if w0.Code != http.StatusOK {
		t.Fatalf("reload failed: %d", w0.Code)
	}

	// The endpoint that stayed stale before the fix: NEW password must
	// authenticate now.
	g := httptest.NewRequest(http.MethodGet, "/admin/update", nil)
	g.Header.Set("X-Admin-Password", "new-pw")
	w := httptest.NewRecorder()
	stack.ServeHTTP(w, g)
	if w.Code != http.StatusOK {
		t.Fatalf("/admin/update with reloaded password: got %d %s — outer-mux config went stale (#63 regression)", w.Code, w.Body.String())
	}
	// And the OLD password must now be rejected (the swap really happened).
	o := httptest.NewRequest(http.MethodGet, "/admin/update", nil)
	o.Header.Set("X-Admin-Password", "old-pw")
	w2 := httptest.NewRecorder()
	stack.ServeHTTP(w2, o)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("old password still accepted: %d", w2.Code)
	}
}
