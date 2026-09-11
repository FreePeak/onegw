package server

// Tests for the dashboard config-edit endpoints (PUT /admin/config/providers,
// PUT /admin/config/combos): behavior consumers observe — the on-disk file
// after a save, the live reloaded config, validation rejections that leave
// the file byte-identical, and secret preservation on keyless updates.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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

// PATCH /admin/config/providers/{name}/disabled — the grid's quick
// on/off toggle. Behavior observed by consumers: the on-disk block gains
// (true) or loses (false) exactly a `disabled` key, everything else in
// the block survives byte-for-byte, and the live pool reconfigures.
func TestProviderDisabledTogglePersistsAndReloads(t *testing.T) {
	srv, h, path := newTestServerFromFile(t, editTestToml)

	// toggle OFF
	w := adminCall(t, h, http.MethodPatch, "/admin/config/providers/p1/disabled", `{"disabled":true}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH disable: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	if !strings.Contains(file, "disabled = true") {
		t.Fatalf("file missing disabled = true:\n%s", file)
	}
	for _, keep := range []string{
		"# p1 comment that must survive an update",
		`"X-Custom" = "keep-me"`,
		"rpm = 6",
		`api_key = "sk-test-p1-secret"`,
	} {
		if !strings.Contains(file, keep) {
			t.Fatalf("disable toggle dropped %q:\n%s", keep, file)
		}
	}
	if st := srv.cur().cfg.Providers[0]; !st.Disabled {
		t.Fatalf("live config not disabled after toggle: %+v", st)
	}

	// toggle back ON: the key is removed again (enabled = default, explicit)
	w = adminCall(t, h, http.MethodPatch, "/admin/config/providers/p1/disabled", `{"disabled":false}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH enable: %d %s", w.Code, w.Body.String())
	}
	file = mustReadFile(t, path)
	if strings.Contains(file, "disabled") {
		t.Fatalf("enable toggle left a disabled key behind:\n%s", file)
	}
	if st := srv.cur().cfg.Providers[0]; st.Disabled {
		t.Fatalf("live config still disabled after re-enable: %+v", st)
	}
}

func TestProviderDisabledToggleUnknownProvider(t *testing.T) {
	_, h, path := newTestServerFromFile(t, editTestToml)
	before := mustReadFile(t, path)
	w := adminCall(t, h, http.MethodPatch, "/admin/config/providers/ghost/disabled", `{"disabled":true}`, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider: want 400, got %d %s", w.Code, w.Body.String())
	}
	if got := mustReadFile(t, path); got != before {
		t.Fatalf("file mutated on rejected toggle:\n%s", diffLines(before, got))
	}
}

func TestProviderDisabledToggleUnauthorized(t *testing.T) {
	_, h, _ := newTestServerFromFile(t, editTestToml)
	w := adminCall(t, h, http.MethodPatch, "/admin/config/providers/p1/disabled", `{"disabled":true}`, false)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

// A paused provider stops being advertised: its model ids (and any
// searxng canonical id) vanish from /v1/models, while combos and aliases
// stay listed (a combo still resolves — it just falls through its
// disabled legs).
func TestDisabledProviderNotAdvertised(t *testing.T) {
	srv, h, _ := newTestServerFromFile(t, editTestToml)
	w := adminCall(t, h, http.MethodPatch, "/admin/config/providers/p1/disabled", `{"disabled":true}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH disable: %d %s", w.Code, w.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer key-a")
	w = do(t, h, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `"p1/m1"`) {
		t.Fatalf("disabled provider still advertised: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"c1"`) {
		t.Fatalf("combo must stay advertised: %s", w.Body.String())
	}
	if st := srv.cur().cfg.Providers[0]; !st.Disabled {
		t.Fatalf("live config not disabled: %+v", st)
	}
}

// editNestedToml mirrors the two block shapes that exist in the live
// onegw.toml — and that the line-based splicers corrupted:
//
//	ph — a SINGLE-bracket nested table ([providers.extra_headers], the
//	     cursor provider's machineId) between the provider keys and its
//	     accounts;
//	ps — provider knobs (always_thinking, models) stranded AFTER its
//	     [[providers.accounts]] tables, where TOML silently re-parents them
//	     into the last account instead of the provider.
const editNestedToml = `# gateway config (nested-table edit fixture)
[server]
data_dir = "memory"
admin_password = "pw-test"

[auth]
keys = ["key-a"]

[[providers]]
name = "ph"
kind = "openai"
base_url = "http://ph.local"
# machineId pins the account identity — hand-set comment
[providers.extra_headers]
x-machine-id = "mid-1"

[[providers.accounts]]
name = "acct-h"
# session token — hand-set comment
api_key = "sk-test-acct-h-secret"

# --- the next provider is documented by this comment ---
[[providers]]
name = "ps"
kind = "openai"
base_url = "http://ps.local"
[[providers.accounts]]
name = "acct-s1"
api_key = "sk-test-acct-s1-secret"
[[providers.accounts]]
name = "acct-s2"
api_key = "sk-test-acct-s2-secret"
# hand-set provider knobs, stranded below the accounts
always_thinking = ["glm-5.3*"]
models = ["s1"]
`

// The cursor-shaped block: a Save must splice exactly ONE account table
// (carrying the on-disk key over), keep the single-bracket nested table,
// and drop nothing.
func TestProviderEditNestedSingleBracketTable(t *testing.T) {
	srv, h, path := newTestServerFromFile(t, editNestedToml)

	body := `{"name":"ph","kind":"openai","base_url":"http://ph.local","models":["h1"],"accounts":[{"name":"acct-h"}]}`
	w := adminCall(t, h, http.MethodPut, "/admin/config/providers", body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT ph: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	if got := strings.Count(file, `name = "acct-h"`); got != 1 {
		t.Fatalf("acct-h tables = %d, want 1 (save duplicated the account):\n%s", got, file)
	}
	for _, want := range []string{
		`api_key = "sk-test-acct-h-secret"`, // key carried over: the account sits below the nested table
		"[providers.extra_headers]",
		`x-machine-id = "mid-1"`,
		"# machineId pins the account identity — hand-set comment",
		"# session token — hand-set comment", // account comment preserved, not dropped
	} {
		if !strings.Contains(file, want) {
			t.Fatalf("file missing %q after save:\n%s", want, file)
		}
	}
	ph := srv.cur().cfg.Providers[0]
	if len(ph.Accounts) != 1 || ph.Accounts[0].APIKey != "sk-test-acct-h-secret" {
		t.Fatalf("reloaded ph accounts: %+v", ph.Accounts)
	}
	if ph.ExtraHeader["x-machine-id"] != "mid-1" || len(ph.Models) != 1 || ph.Models[0] != "h1" {
		t.Fatalf("reloaded ph lost nested table or models: %+v", ph)
	}

	// Saving the same form again must not grow the mess: idempotent output.
	w = adminCall(t, h, http.MethodPut, "/admin/config/providers", body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("second PUT ph: %d %s", w.Code, w.Body.String())
	}
	if again := mustReadFile(t, path); again != file {
		t.Fatalf("second save changed the file:\n%s", diffLines(file, again))
	}
}

// The opencode-shaped block: knobs stranded inside the last account table
// are provider keys by intent — a Save must return them to provider scope
// instead of deleting them or writing a second copy above.
func TestProviderEditStrandedKeysRehoisted(t *testing.T) {
	srv, h, path := newTestServerFromFile(t, editNestedToml)

	// Precondition: TOML has already re-parented them, so the live provider
	// has neither models nor always_thinking.
	if got := srv.cur().cfg.Providers[1]; len(got.Models) != 0 || len(got.AlwaysThinking) != 0 {
		t.Fatalf("fixture no longer reproduces the stranding: %+v", got)
	}

	w := adminCall(t, h, http.MethodPut, "/admin/config/providers",
		`{"name":"ps","kind":"openai","base_url":"http://ps.local","models":["s1","s2"],"accounts":[{"name":"acct-s1"},{"name":"acct-s2"}]}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT ps: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	if got := strings.Count(file, "always_thinking"); got != 1 {
		t.Fatalf("always_thinking lines = %d, want 1 (deleted or duplicated by save):\n%s", got, file)
	}
	if got := strings.Count(file, "models = "); got != 1 {
		t.Fatalf("models lines = %d, want 1 (save wrote a second copy):\n%s", got, file)
	}
	ps := srv.cur().cfg.Providers[1]
	if len(ps.AlwaysThinking) != 1 || ps.AlwaysThinking[0] != "glm-5.3*" {
		t.Fatalf("reloaded ps lost always_thinking: %+v", ps)
	}
	if strings.Join(ps.Models, ",") != "s1,s2" {
		t.Fatalf("reloaded ps models: %v", ps.Models)
	}
	if len(ps.Accounts) != 2 || ps.Accounts[0].APIKey != "sk-test-acct-s1-secret" || ps.Accounts[1].APIKey != "sk-test-acct-s2-secret" {
		t.Fatalf("reloaded ps accounts: %+v", ps.Accounts)
	}
}

// The on/off toggle on a block that owns a nested table: exactly one
// `disabled` key appears in that block only, and toggling back restores
// the file byte-for-byte.
func TestProviderDisabledToggleNestedRoundTrip(t *testing.T) {
	srv, h, path := newTestServerFromFile(t, editNestedToml)
	before := mustReadFile(t, path)

	if w := adminCall(t, h, http.MethodPatch, "/admin/config/providers/ph/disabled", `{"disabled":true}`, true); w.Code != http.StatusOK {
		t.Fatalf("PATCH disable: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	if got := strings.Count(file, "disabled = true"); got != 1 {
		t.Fatalf("disabled lines = %d, want 1:\n%s", got, file)
	}
	st := srv.cur().cfg.Providers
	if !st[0].Disabled {
		t.Fatalf("ph not disabled after toggle: %+v", st[0])
	}
	if st[1].Disabled {
		t.Fatalf("toggle leaked into ps: %+v", st[1])
	}

	if w := adminCall(t, h, http.MethodPatch, "/admin/config/providers/ph/disabled", `{"disabled":false}`, true); w.Code != http.StatusOK {
		t.Fatalf("PATCH enable: %d %s", w.Code, w.Body.String())
	}
	if got := mustReadFile(t, path); got != before {
		t.Fatalf("toggle round trip not byte-exact:\n%s", diffLines(before, got))
	}
}

// editLegacyToml mirrors the legacy credential shapes the live configs
// actually contain (e.g. the keys-style opencode block): `keys = [...]`
// and bare `api_key` — provider blocks with NO [[providers.accounts]]
// tables. config.Defaults expands keys into "key-N" accounts at every
// Load, and the editor prefill (providerEditViews) synthesizes exactly
// those names, so a Save must carry the key material into the rendered
// account tables and supersede the legacy lines. Superseding nothing
// duplicates every key into a keyless row on the next Load (pool
// round-robins the keyless one → 401s); dropping unrepresented material
// is silent credential loss.
const editLegacyToml = `# gateway config (legacy-credential edit fixture)
[server]
data_dir = "memory"
admin_password = "pw-test"

[auth]
keys = ["key-a"]

[[providers]]
name = "lk"
kind = "openai"
base_url = "http://lk.local"
# keys = ["sk-old-1"] — a superseded note; comments never match
keys = ["sk-test-lk1-secret", "sk-test-lk2-secret"]
always_thinking = ["glm-5.3*"]

[[providers]]
name = "lm"
kind = "openai"
base_url = "http://lm.local"
keys = [
  "sk-test-lm1-secret",
  "sk-test-lm2-secret",
]

[[providers]]
name = "lp"
kind = "openai"
base_url = "http://lp.local"
api_key = "sk-test-lp-secret"
`

// The live keys-style shape: the UI round-trip sends only the row names
// the prefill synthesized. The save converts the block to account tables
// with the original keys carried over and removes the superseded line.
func TestProviderEditLegacyKeysConverted(t *testing.T) {
	srv, h, path := newTestServerFromFile(t, editLegacyToml)

	// Precondition: Defaults expands the keys array into key-N accounts —
	// the names the prefill sends back, with blank key fields.
	lk := srv.cur().cfg.Providers[0]
	if len(lk.Accounts) != 2 || lk.Accounts[0].Name != "key-1" || lk.Accounts[0].APIKey != "sk-test-lk1-secret" {
		t.Fatalf("fixture keys-expansion precondition broke: %+v", lk.Accounts)
	}

	w := adminCall(t, h, http.MethodPut, "/admin/config/providers",
		`{"name":"lk","kind":"openai","base_url":"http://lk.local","models":["m1"],"accounts":[{"name":"key-1"},{"name":"key-2"}]}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT lk: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	if strings.Contains(file, `keys = ["sk-test-lk1-secret"`) {
		t.Fatalf("legacy keys line survived a superseding accounts save:\n%s", file)
	}
	if got := strings.Count(file, "sk-test-lk1-secret"); got != 1 {
		t.Fatalf("lk1 key material appears %d times, want exactly 1:\n%s", got, file)
	}
	lk = srv.cur().cfg.Providers[0]
	if len(lk.Accounts) != 2 || lk.Accounts[0].Name != "key-1" || lk.Accounts[0].APIKey != "sk-test-lk1-secret" ||
		lk.Accounts[1].Name != "key-2" || lk.Accounts[1].APIKey != "sk-test-lk2-secret" {
		t.Fatalf("reloaded lk accounts (want exactly one keyed pair): %+v", lk.Accounts)
	}
	if len(lk.AlwaysThinking) != 1 {
		t.Fatalf("lk lost always_thinking during conversion: %+v", lk)
	}
}

// A multi-line keys array must convert too — including removing every
// line the array spans (an orphaned continuation line is a parse error).
func TestProviderEditLegacyMultiLineKeysConverted(t *testing.T) {
	srv, h, path := newTestServerFromFile(t, editLegacyToml)

	w := adminCall(t, h, http.MethodPut, "/admin/config/providers",
		`{"name":"lm","kind":"openai","base_url":"http://lm.local","accounts":[{"name":"key-1"},{"name":"key-2"}]}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT lm: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	for _, want := range []string{"sk-test-lm1-secret", "sk-test-lm2-secret"} {
		if got := strings.Count(file, want); got != 1 {
			t.Fatalf("%q appears %d times, want exactly 1:\n%s", want, got, file)
		}
	}
	lm := srv.cur().cfg.Providers[1]
	if len(lm.Accounts) != 2 || lm.Accounts[0].APIKey != "sk-test-lm1-secret" || lm.Accounts[1].APIKey != "sk-test-lm2-secret" {
		t.Fatalf("reloaded lm accounts: %+v", lm.Accounts)
	}
}

// A bare api_key provider prefills as one row named "default"; the save
// converts the credential into the account table instead of writing a
// keyless "default" row next to the surviving line.
func TestProviderEditLegacyAPIKeyConverted(t *testing.T) {
	srv, h, path := newTestServerFromFile(t, editLegacyToml)

	w := adminCall(t, h, http.MethodPut, "/admin/config/providers",
		`{"name":"lp","kind":"openai","base_url":"http://lp.local","accounts":[{"name":"default"}]}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT lp: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	if got := strings.Count(file, "sk-test-lp-secret"); got != 1 {
		t.Fatalf("lp key material appears %d times, want exactly 1:\n%s", got, file)
	}
	lp := srv.cur().cfg.Providers[2]
	if len(lp.Accounts) != 1 || lp.Accounts[0].Name != "default" || lp.Accounts[0].APIKey != "sk-test-lp-secret" || lp.APIKey != "" {
		t.Fatalf("reloaded lp accounts: %+v (provider APIKey %q)", lp.Accounts, lp.APIKey)
	}
}
