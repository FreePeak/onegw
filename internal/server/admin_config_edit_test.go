package server

// Tests for the dashboard config-edit endpoints (PUT /admin/config/providers,
// PUT /admin/config/combos): behavior consumers observe — the on-disk file
// after a save, the live reloaded config, validation rejections that leave
// the file byte-identical, and secret preservation on keyless updates.

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

const editTestToml = `# gateway config (edit test fixture)
[server]
data_dir = "memory"
admin_password = "pw-test"

[auth]
keys = ["key-a"]

[[providers]]
name = "p1"
kind = "openai"
base_url = "http://p1.local"
api_key = "sk-test-p1-secret"
# p1 comment that must survive an update
extra_headers = { "X-Custom" = "keep-me" }
# provider-wide shared budget (Def.RPM) — hand-set; a modal Save must not
# drop it (the dashboard editor renders only the fields it knows).
models = ["m1"]
rpm = 6

[[providers.accounts]]
name = "acct1"
api_key = "sk-test-acct1-secret"

[[providers.accounts]]
name = "acct2"
api_key = "sk-test-acct2-secret"
weight = 3

[[combo]]
name = "c1"
targets = ["p1/m1"]
`

func TestProviderEditAddReloadsLiveConfig(t *testing.T) {
	_, h, path := newTestServerFromFile(t, editTestToml)

	body := `{"name":"p2","kind":"openai","base_url":"http://p2.local","api_key":"sk-test-p2","models":["m2"],"max_concurrency":4,"sticky":"5m"}`
	w := adminCall(t, h, http.MethodPut, "/admin/config/providers", body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT provider: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"action":"added"`) {
		t.Fatalf("want action=added, got %s", w.Body.String())
	}

	// The file must contain the rendered block…
	file := mustReadFile(t, path)
	for _, want := range []string{`[[providers]]`, `name = "p2"`, `kind = "openai"`, `api_key = "sk-test-p2"`, `models = ["m2"]`, `max_concurrency = 4`, `sticky = "5m"`} {
		if !strings.Contains(file, want) {
			t.Fatalf("file missing %q after add:\n%s", want, file)
		}
	}
	// …and the RUNNING config must have reloaded (consumer-observable via
	// the grouped API, same as a SIGHUP).
	w = adminCall(t, h, http.MethodGet, "/admin/api/v1/providers", "", true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"p2"`) {
		t.Fatalf("reloaded providers missing p2: %d %s", w.Code, w.Body.String())
	}
}

func TestProviderEditUpdatePreservesSecretsAndForeignLines(t *testing.T) {
	_, h, path := newTestServerFromFile(t, editTestToml)

	// Update p1 WITHOUT api_key and WITHOUT accounts: the on-disk key
	// lines must survive; only managed fields change.
	body := `{"name":"p1","kind":"openai","base_url":"http://p1-new.local","models":["m9"],"max_concurrency":2}`
	w := adminCall(t, h, http.MethodPut, "/admin/config/providers", body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT provider update: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	for _, want := range []string{
		`api_key = "sk-test-p1-secret"`,    // provider key preserved
		`api_key = "sk-test-acct1-secret"`, // account keys preserved
		`api_key = "sk-test-acct2-secret"`,
		"# p1 comment that must survive an update", // comments preserved
		`"X-Custom" = "keep-me"`,                   // foreign keys preserved
		"weight = 3",                               // account scalar preserved
		`base_url = "http://p1-new.local"`,         // managed field updated
		`models = ["m9"]`,
		"max_concurrency = 2",
	} {
		if !strings.Contains(file, want) {
			t.Fatalf("file missing %q after update:\n%s", want, file)
		}
	}
}

func TestProviderEditUpdateAccountKeepsKeyByName(t *testing.T) {
	_, h, path := newTestServerFromFile(t, editTestToml)

	// Accounts provided with empty keys: existing keys carry over by name.
	body := `{"name":"p1","kind":"openai","accounts":[{"name":"acct1"},{"name":"acct2","weight":5}]}`
	w := adminCall(t, h, http.MethodPut, "/admin/config/providers", body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT provider accounts update: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	if !strings.Contains(file, `api_key = "sk-test-acct1-secret"`) ||
		!strings.Contains(file, `api_key = "sk-test-acct2-secret"`) {
		t.Fatalf("account keys not preserved:\n%s", file)
	}
	if !strings.Contains(file, "weight = 5") || strings.Contains(file, "weight = 3") {
		t.Fatalf("account weight not updated:\n%s", file)
	}
}

func TestProviderEditAccountRPMRoundTrip(t *testing.T) {
	srv, h, path := newTestServerFromFile(t, editTestToml)

	// A hand-set rpm survives the full acctEdit parse + spliceProvider round
	// trip (the #57 hazard: a modal Save must not silently drop it).
	w := adminCall(t, h, http.MethodPut, "/admin/config/providers",
		`{"name":"p1","kind":"openai","accounts":[{"name":"acct1","api_key":"sk-new1","rpm":5}]}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT provider rpm update: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	if !strings.Contains(file, "rpm = 5") {
		t.Fatalf("hand-set rpm not preserved after save:\n%s", file)
	}
	// The hand-set provider-WIDE rpm (Def.RPM, a shared per-user budget)
	// must survive a modal Save untouched — the editor only rewrites the
	// scalars it knows, and an account-table re-render never touches the
	// top region where it lives.
	if !strings.Contains(file, "rpm = 6") {
		t.Fatalf("hand-set provider rpm dropped by save:\n%s", file)
	}
	// The live reloaded config must carry both (consumer-observable,
	// same as a SIGHUP).
	st := srv.cur().cfg.Providers[0]
	if len(st.Accounts) == 0 || st.Accounts[0].Name != "acct1" || st.Accounts[0].RPM != 5 {
		t.Fatalf("reloaded config missing rpm 5 on acct1: %+v", st.Accounts)
	}
	if st.RPM != 6 {
		t.Fatalf("reloaded config lost provider-level rpm 6: %+v", st.RPM)
	}

	// rpm 0 emits no line: a save without rpm must not leave a stale
	// `rpm = 5` behind, and must not write `rpm = 0`.
	w = adminCall(t, h, http.MethodPut, "/admin/config/providers",
		`{"name":"p1","kind":"openai","accounts":[{"name":"acct1","api_key":"sk-new1"}]}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT provider rpm-less update: %d %s", w.Code, w.Body.String())
	}
	file = mustReadFile(t, path)
	if strings.Contains(file, "rpm = 5") {
		t.Fatalf("stale account rpm present after rpm-less save:\n%s", file)
	}
	if !strings.Contains(file, "rpm = 6") {
		t.Fatalf("provider-level rpm must survive an rpm-less save:\n%s", file)
	}
}

func TestProviderEditValidationRejectsAndLeavesFileUntouched(t *testing.T) {
	srv, h, path := newTestServerFromFile(t, editTestToml)
	before := mustReadFile(t, path)

	cases := map[string]string{
		"unknown kind":     `{"name":"px","kind":"nope","api_key":"sk-x"}`,
		"missing name":     `{"name":"","kind":"openai","api_key":"sk-x"}`,
		"no credentials":   `{"name":"p3","kind":"openai","base_url":"http://x.local"}`,
		"invalid sticky":   `{"name":"p3","kind":"openai","api_key":"sk-x","sticky":"banana"}`,
		"quota w/o window": `{"name":"p1","kind":"openai","api_key":"sk-test-p1-secret","quota_limit_tokens":5}`,
	}
	for name, body := range cases {
		w := adminCall(t, h, http.MethodPut, "/admin/config/providers", body, true)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d %s", name, w.Code, w.Body.String())
		}
		if got := mustReadFile(t, path); got != before {
			t.Fatalf("%s: file mutated on rejected edit:\n%s", name, diffLines(before, got))
		}
	}
	// The live config never changed either.
	if got := providerViews(srv.cur()); len(got) != 1 {
		t.Fatalf("live providers changed after rejections: %d", len(got))
	}
}

func TestProviderEditUnauthorized(t *testing.T) {
	_, h, _ := newTestServerFromFile(t, editTestToml)
	w := adminCall(t, h, http.MethodPut, "/admin/config/providers",
		`{"name":"p2","kind":"openai","api_key":"sk-x"}`, false)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestComboEditAddUpdateReload(t *testing.T) {
	_, h, path := newTestServerFromFile(t, editTestToml)

	// add
	w := adminCall(t, h, http.MethodPut, "/admin/config/combos",
		`{"name":"c2","targets":["p1/m1"]}`, true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"action":"added"`) {
		t.Fatalf("PUT combo add: %d %s", w.Code, w.Body.String())
	}
	// update (targets rewritten in place) — different target so the count
	// assertion below is unambiguous (added c2 shares ["p1/m1"])
	w = adminCall(t, h, http.MethodPut, "/admin/config/combos",
		`{"name":"c1","targets":["p1/m9"]}`, true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"action":"updated"`) {
		t.Fatalf("PUT combo update: %d %s", w.Code, w.Body.String())
	}
	// update must not leave a stale targets line behind
	file := mustReadFile(t, path)
	if got := strings.Count(file, `targets = ["p1/m9"]`); got != 1 {
		t.Fatalf("combo c1 targets line count = %d after update:\n%s", got, file)
	}
	// live reload is consumer-observable
	w = adminCall(t, h, http.MethodGet, "/admin/api/v1/combos", "", true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"c2"`) {
		t.Fatalf("reloaded combos missing c2: %d %s", w.Code, w.Body.String())
	}
}

func TestComboEditValidationRejectsAndLeavesFileUntouched(t *testing.T) {
	_, h, path := newTestServerFromFile(t, editTestToml)
	before := mustReadFile(t, path)

	cases := map[string]string{
		"unknown provider":    `{"name":"c9","targets":["ghost/m1"]}`,
		"empty targets":       `{"name":"c9","targets":[]}`,
		"duplicate combo":     `{"name":"C1","targets":["p1/m1"]}`,
		"target not prov/mod": `{"name":"c9","targets":["p1m1"]}`,
	}
	for name, body := range cases {
		w := adminCall(t, h, http.MethodPut, "/admin/config/combos", body, true)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d %s", name, w.Code, w.Body.String())
		}
		if got := mustReadFile(t, path); got != before {
			t.Fatalf("%s: file mutated on rejected edit:\n%s", name, diffLines(before, got))
		}
	}
}

func diffLines(a, b string) string {
	al := strings.Split(a, "\n")
	bl := strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return fmt.Sprintf("line %d:\n- %q\n+ %q", i+1, x, y)
		}
	}
	return "(identical)"
}

var _ = os.Getenv // keep os import if unused after refactors
