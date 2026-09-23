package server

// Tests for the shared proxy-pool admin surface (GET/PUT
// /admin/config/proxies): the on-disk [proxy] table after a save, the
// live reloaded pool + provider opt-ins, and validation rejections that
// leave the file byte-identical.

import (
	"net/http"
	"strings"
	"testing"
)

const proxyTestToml = `# gateway config (proxy test fixture)
[server]
data_dir = "memory"
admin_password = "pw-test"

[auth]
keys = ["key-a"]

[[providers]]
name = "p1"
kind = "openai"
base_url = "http://p1.local"
api_key = "sk-test-p1"
models = ["m1"]

[[providers]]
name = "p2"
kind = "openai"
base_url = "http://p2.local"
api_key = "sk-test-p2"
models = ["m2"]
`

func TestProxiesPutBuildsPoolAndOptIns(t *testing.T) {
	_, h, path := newTestServerFromFile(t, proxyTestToml)

	body := `{"urls":["http://px1:8080","http://px2:8080"],"no_proxy":"localhost","rotation":"random",` +
		`"providers":[{"name":"p1","proxy":true},{"name":"p2","proxy":false}]}`
	w := adminCall(t, h, http.MethodPut, "/admin/config/proxies", body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT proxies: %d %s", w.Code, w.Body.String())
	}

	file := mustReadFile(t, path)
	for _, want := range []string{
		"[proxy]",
		`urls = ["http://px1:8080", "http://px2:8080"]`,
		`no_proxy = "localhost"`,
		`rotation = "random"`,
		"proxy = true", // exactly once: only p1 opted in
	} {
		if !strings.Contains(file, want) {
			t.Fatalf("file missing %q after save:\n%s", want, file)
		}
	}
	if n := strings.Count(file, "proxy = true"); n != 1 {
		t.Fatalf("want exactly one proxy opt-in, got %d:\n%s", n, file)
	}

	// Live config reloaded: p1 routes through the pool, p2 stays direct.
	w = adminCall(t, h, http.MethodGet, "/admin/config/proxies", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET proxies: %d %s", w.Code, w.Body.String())
	}
	got := w.Body.String()
	for _, want := range []string{
		`"urls":["http://px1:8080","http://px2:8080"]`,
		`"rotation":"random"`,
		`"name":"p1","proxy":true`,
		`"name":"p2","proxy":false`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("GET proxies missing %q: %s", want, got)
		}
	}
}

func TestProxiesPutValidationLeavesFileUntouched(t *testing.T) {
	_, h, path := newTestServerFromFile(t, proxyTestToml)
	before := mustReadFile(t, path)

	// Bad pool URL.
	w := adminCall(t, h, http.MethodPut, "/admin/config/proxies",
		`{"urls":["http://"],"providers":[]}`, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad proxy URL: want 400, got %d %s", w.Code, w.Body.String())
	}
	// Unknown provider name.
	w = adminCall(t, h, http.MethodPut, "/admin/config/proxies",
		`{"urls":["http://px1:8080"],"providers":[{"name":"nope","proxy":true}]}`, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider: want 400, got %d %s", w.Code, w.Body.String())
	}
	// Opt-in with an emptied pool: validation fails (no pool configured).
	w = adminCall(t, h, http.MethodPut, "/admin/config/proxies",
		`{"urls":[],"providers":[{"name":"p1","proxy":true}]}`, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("opt-in with no pool: want 400, got %d %s", w.Code, w.Body.String())
	}

	if after := mustReadFile(t, path); after != before {
		t.Fatalf("rejected saves must leave the file byte-identical:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

func TestProxiesPutEmptyPoolClearsTable(t *testing.T) {
	_, h, path := newTestServerFromFile(t, proxyTestToml)

	seed := `{"urls":["http://px1:8080"],"providers":[{"name":"p1","proxy":true}]}`
	if w := adminCall(t, h, http.MethodPut, "/admin/config/proxies", seed, true); w.Code != http.StatusOK {
		t.Fatalf("seed PUT: %d %s", w.Code, w.Body.String())
	}
	clear := `{"urls":[],"providers":[{"name":"p1","proxy":false}]}`
	if w := adminCall(t, h, http.MethodPut, "/admin/config/proxies", clear, true); w.Code != http.StatusOK {
		t.Fatalf("clear PUT: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	if strings.Contains(file, "[proxy]") {
		t.Fatalf("emptied pool must remove the [proxy] table:\n%s", file)
	}
	if strings.Contains(file, "proxy = true") {
		t.Fatalf("cleared opt-in must remove the proxy line:\n%s", file)
	}
}
