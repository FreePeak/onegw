package server

// Tests for the dashboard revamp's three new surfaces: the one-page credential
// CRUD, model discovery (fetch → copy → optionally pin), and the collapse rule
// that decides which provider card opens itself.
//
// Every page assertion goes through GET /admin/ui/… rather than the JSON twin:
// html/template fails a missing view field at EXECUTE time, so a page can be a
// silent 500 while its API endpoint stays green. That is the render-error trap
// this file exists to catch.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// keysFixture is one config that exercises every row shape the credential
// page has to handle. extraProviders is spliced in after the [[providers]]
// blocks and before [[oauth.accounts]], so an appended table stays a sibling.
func keysFixture(upstreamURL string, extraProviders ...string) string {
	return fmt.Sprintf(`# keys-page test fixture
[server]
data_dir = "memory"
admin_password = "pw-test"

[auth]
keys = ["key-a", "key-b"]

[[providers]]
name = "p1"
kind = "openai"
base_url = %q
models = ["m1", "m2"]

[[providers.accounts]]
name = "alpha"
api_key = "sk-alpha"

[[providers.accounts]]
name = "beta"
api_key = "sk-beta"

[[providers]]
name = "p2"
kind = "openai"
base_url = "http://p2.invalid"
api_key = "sk-legacy"

[[providers]]
name = "p3"
kind = "openai"
base_url = "http://p3.invalid"

[[providers.accounts]]
name = "sub"
%s
[[oauth.accounts]]
provider = "p3"
account = "sub"
service  = "kilocode"
`, upstreamURL, "")
}

func getJSON[T any](t *testing.T, h http.Handler, target string) T {
	t.Helper()
	w := adminCall(t, h, http.MethodGet, target, "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", target, w.Code, w.Body.String())
	}
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", target, err)
	}
	return v
}

// TestKeysPageRenders: the page exists, names both families, and does not
// inline any secret into the document.
func TestKeysPageRenders(t *testing.T) {
	_, h, _ := newTestServerFromFile(t, keysFixture("http://p1.invalid"))
	w := adminCall(t, h, http.MethodGet, "/admin/ui/keys", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("keys page: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"Client keys", "Provider keys", "/admin/config/keys", "/admin/ui/providers"} {
		if !strings.Contains(body, want) {
			t.Fatalf("keys page missing %q", want)
		}
	}
	for _, secret := range []string{"sk-alpha", "key-a", "sk-legacy"} {
		if strings.Contains(body, secret) {
			t.Fatalf("keys page must not server-render the secret %q", secret)
		}
	}
}

// TestKeysGetListsEveryCredential is the copy-the-key contract: one call
// returns each client key in full plus one row per provider account, including
// the subscription row that has no key to show.
func TestKeysGetListsEveryCredential(t *testing.T) {
	_, h, _ := newTestServerFromFile(t, keysFixture("http://p1.invalid"))
	got := getJSON[struct {
		Keys      []clientKeyView   `json:"keys"`
		Providers []providerKeyView `json:"providers"`
	}](t, h, "/admin/config/keys")

	if len(got.Keys) != 2 || got.Keys[0].Key != "key-a" {
		t.Fatalf("client keys = %+v", got.Keys)
	}
	want := []struct{ prov, acct, key, oauth string }{
		{"p1", "alpha", "sk-alpha", ""},
		{"p1", "beta", "sk-beta", ""},
		{"p2", "default", "sk-legacy", ""},
		{"p3", "sub", "", "kilocode"},
	}
	if len(got.Providers) != len(want) {
		t.Fatalf("provider rows = %+v, want %d rows", got.Providers, len(want))
	}
	for i, w := range want {
		g := got.Providers[i]
		if g.Provider != w.prov || g.Account != w.acct || g.Key != w.key || g.OAuth != w.oauth {
			t.Fatalf("row %d = %+v, want %+v", i, g, w)
		}
	}
}

// TestKeysPatchWritesAndClearsOneAccountKey: editing one account's key must
// land in onegw.toml without disturbing its sibling, the provider's other
// fields, or the hand-written comment.
func TestKeysPatchWritesAndClearsOneAccountKey(t *testing.T) {
	_, h, path := newTestServerFromFile(t, keysFixture("http://p1.invalid"))

	w := adminCall(t, h, http.MethodPatch, "/admin/config/keys",
		`{"provider_set":[{"provider":"p1","account":"beta","key":"sk-beta-new"}]}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("set provider key: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	for _, want := range []string{`api_key = "sk-beta-new"`, `api_key = "sk-alpha"`, `models = ["m1", "m2"]`} {
		if !strings.Contains(file, want) {
			t.Fatalf("after a one-key edit the file must still contain %q:\n%s", want, file)
		}
	}
	// The live gateway picked it up: the listing reads back the new key.
	got := getJSON[struct {
		Providers []providerKeyView `json:"providers"`
	}](t, h, "/admin/config/keys")
	found := false
	for _, r := range got.Providers {
		if r.Provider == "p1" && r.Account == "beta" {
			found = r.Key == "sk-beta-new"
		}
	}
	if !found {
		t.Fatalf("reload did not take the new key: %+v", got.Providers)
	}

	// Clearing the last key of a two-account roster keeps the other row.
	w = adminCall(t, h, http.MethodPatch, "/admin/config/keys",
		`{"provider_clear":[{"provider":"p1","account":"alpha"}]}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("clear provider key: %d %s", w.Code, w.Body.String())
	}
	file = mustReadFile(t, path)
	if strings.Contains(file, "sk-alpha") || !strings.Contains(file, "sk-beta-new") {
		t.Fatalf("clear must drop exactly one account:\n%s", file)
	}
}

// TestKeysPatchClientKeysAndGuards covers the client family: add, remove, and
// the two refusals that keep the gateway from locking itself out or writing a
// key TOML cannot hold.
func TestKeysPatchClientKeysAndGuards(t *testing.T) {
	_, h, path := newTestServerFromFile(t, keysFixture("http://p1.invalid"))

	if w := adminCall(t, h, http.MethodPatch, "/admin/config/keys", `{"add":["sk-client-1"]}`, true); w.Code != http.StatusOK {
		t.Fatalf("add: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(mustReadFile(t, path), "sk-client-1") {
		t.Fatalf("client key not written:\n%s", mustReadFile(t, path))
	}
	if w := adminCall(t, h, http.MethodPatch, "/admin/config/keys", `{"remove":["sk-client-1"]}`, true); w.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(mustReadFile(t, path), "sk-client-1") {
		t.Fatalf("client key not removed:\n%s", mustReadFile(t, path))
	}
	// A key with a newline would break the array it is spliced into.
	if w := adminCall(t, h, http.MethodPatch, "/admin/config/keys", `{"provider_set":[{"provider":"p1","account":"beta","key":"sk\nbad"}]}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("control character must be refused, got %d", w.Code)
	}
	// Both accounts of p1 cleared → refused, not an account-less provider.
	if w := adminCall(t, h, http.MethodPatch, "/admin/config/keys",
		`{"provider_clear":[{"provider":"p2","account":"default"}]}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("clearing a sole credential must be refused, got %d %s", w.Code, w.Body.String())
	}
}

// TestModelRowsAndCopyIDs: the models page renders one card per provider, each
// advertised id is a copy button carrying the `provider/model` string a client
// actually pastes, and the searxng search surface stays off the list.
func TestModelRowsAndCopyIDs(t *testing.T) {
	cfg := keysFixture("http://p1.invalid", `
[[providers]]
name = "search"
kind = "searxng"
base_url = "http://searxng.invalid"
`)
	_, h, _ := newTestServerFromFile(t, cfg)
	w := adminCall(t, h, http.MethodGet, "/admin/ui/models", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("models page: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-copy="p1/m1"`, `data-copy="p1/m2"`, // the id a client pastes, verbatim
		`data-fetch="p1"`, `data-fetch="p2"`, `data-save="p1"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("models page missing %q", want)
		}
	}
	if strings.Contains(body, `data-fetch="search"`) {
		t.Fatal("a search surface has no model catalog to list")
	}
}

// TestModelsAPIAdvertisesPassThrough: with nothing configured the page must say
// pass-through rather than "no models", because routing accepts any id there.
func TestModelsAPIAdvertisesPassThrough(t *testing.T) {
	_, h, _ := newTestServerFromFile(t, keysFixture("http://p1.invalid"))
	rows := getJSON[[]modelRowView](t, h, "/admin/api/v1/models")
	byName := map[string]modelRowView{}
	for _, r := range rows {
		byName[r.Provider] = r
	}
	if p1 := byName["p1"]; len(p1.Configured) != 2 || p1.Passthrough {
		t.Fatalf("p1 rows = %+v", p1)
	}
	// p3 declares no models, so it is either pass-through or serving the kind's
	// built-in catalog — the flag must agree with the list it ships.
	if p3 := byName["p3"]; p3.Passthrough != (len(p3.Configured) == 0) {
		t.Fatalf("p3 passthrough flag disagrees with its list: %+v", p3)
	}
}

// TestModelFetchParsesUpstreamCatalog proves the two halves of the fetch flow:
// the ids come back from the provider's own endpoint, and Pin is what writes
// them into the config file (a fetch alone must not change routing).
func TestModelFetchParsesUpstreamCatalog(t *testing.T) {
	var gotPath, gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"zeta"},{"id":"up-alpha"},{"id":"up-alpha"},{"id":"up-beta"}]}`))
	}))
	defer up.Close()

	cfg := strings.Replace(keysFixture(up.URL), "models = [\"m1\", \"m2\"]\n", "", 1)
	_, h, path := newTestServerFromFile(t, cfg)

	w := adminCall(t, h, http.MethodPost, "/admin/config/providers/p1/models/fetch", `{}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("fetch: %d %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/models" || gotAuth != "Bearer sk-alpha" {
		t.Fatalf("upstream saw %s %q, want the models endpoint with an account key", gotPath, gotAuth)
	}
	res := decodeJSON[struct {
		Models  []string `json:"models"`
		Count   int      `json:"count"`
		Applied bool     `json:"applied"`
	}](t, w.Body.String())
	if strings.Join(res.Models, ",") != "up-alpha,up-beta,zeta" || res.Count != 3 {
		t.Fatalf("models = %+v, want the deduped sorted upstream ids", res.Models)
	}
	if res.Applied {
		t.Fatal("a plain fetch must not write config")
	}
	// Nothing was pinned, so the only "alpha" left in the file is the account
	// the fixture created: no `models` line may name a discovered id.
	if before := mustReadFile(t, path); strings.Contains(before, `models = ["alpha"`) || strings.Contains(before, `models = ["zeta"`) {
		t.Fatalf("fetch alone changed the file's model list:\n%s", before)
	}

	// Pin: the same list, now in onegw.toml, so routing narrows on purpose.
	if w := adminCall(t, h, http.MethodPost, "/admin/config/providers/p1/models/fetch", `{"apply":true}`, true); w.Code != http.StatusOK {
		t.Fatalf("pin: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	if !strings.Contains(file, `models = ["up-alpha", "up-beta", "zeta"]`) {
		t.Fatalf("pinned models missing:\n%s", file)
	}
	if !strings.Contains(file, `api_key = "sk-alpha"`) {
		t.Fatalf("pinning models must keep the accounts:\n%s", file)
	}
	rows := getJSON[[]modelRowView](t, h, "/admin/api/v1/models")
	for _, r := range rows {
		if r.Provider == "p1" && strings.Join(r.Configured, ",") != "up-alpha,up-beta,zeta" {
			t.Fatalf("reloaded p1 configured = %v", r.Configured)
		}
	}
}

// TestModelFetchReportsUpstreamFailure: a dead vendor must say so on the row
// instead of rendering an empty list that reads like "no models".
func TestModelFetchReportsUpstreamFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer up.Close()
	_, h, _ := newTestServerFromFile(t, strings.Replace(keysFixture(up.URL), "models = [\"m1\", \"m2\"]\n", "", 1))

	w := adminCall(t, h, http.MethodPost, "/admin/config/providers/p1/models/fetch", `{}`, true)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("dead upstream = %d %s, want 502", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "401") {
		t.Fatalf("failure must name the upstream status: %s", w.Body.String())
	}
	rows := getJSON[[]modelRowView](t, h, "/admin/api/v1/models")
	for _, r := range rows {
		if r.Provider == "p1" && r.Error == "" {
			t.Fatal("the row must carry the fetch error")
		}
	}
}

// TestEveryCopyPayloadIsReachable guards the clipboard contract itself: the
// delegated handler in base.html fires on `.copy`, so a data-copy attribute on
// an element without that class is a button that looks copyable and does
// nothing. This caught a real break when the models cards first shipped as
// `.idcopy`.
func TestEveryCopyPayloadIsReachable(t *testing.T) {
	_, h, _ := newTestServerFromFile(t, keysFixture("http://p1.invalid"))
	for _, page := range []string{"/admin", "/admin/ui/providers", "/admin/ui/models", "/admin/ui/keys", "/admin/ui/tools", "/admin/ui/settings"} {
		w := adminCall(t, h, http.MethodGet, page, "", true)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", page, w.Code, w.Body.String())
		}
		for _, tag := range strings.Split(w.Body.String(), "<")[1:] {
			if !strings.Contains(tag, "data-copy=") {
				continue
			}
			open := "<" + tag[:strings.Index(tag, ">")]
			if !slices.Contains(classesOf(t, open), "copy") {
				t.Fatalf("%s: copy payload without the .copy handler class: %s", page, open)
			}
		}
	}
}

func classesOf(t *testing.T, openTag string) []string {
	t.Helper()
	m := regexp.MustCompile(`class="([^"]*)"`).FindStringSubmatch(openTag)
	if m == nil {
		return nil
	}
	return strings.Fields(m[1])
}

// TestProviderCardsCollapseUnlessTheyNeedAttention is the lean-grid rule: a
// configured provider renders closed, a provider still missing a credential or
// a sign-in renders open, and a deliberately disabled one stays closed.
func TestProviderCardsCollapseUnlessTheyNeedAttention(t *testing.T) {
	_, h, _ := newTestServerFromFile(t, keysFixture("http://p1.invalid"))
	w := adminCall(t, h, http.MethodGet, "/admin/ui/providers", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("providers page: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	closed := `data-acc="prov:p1"` // two accounts with keys: nothing to do
	legacy := `data-acc="prov:p2"` // a scalar api_key: configured
	open := `data-acc="prov:p3"`   // one account, no key, never signed in

	if !strings.Contains(body, closed) || !strings.Contains(body, legacy) {
		t.Fatalf("provider cards missing from the grid:\n%s", body)
	}
	if strings.Contains(body, `data-acc="prov:p1" open`) || strings.Contains(body, `data-acc="prov:p2" open`) {
		t.Fatal("a configured provider must start collapsed")
	}
	if !strings.Contains(body, open+` open`) || !strings.Contains(body, `data-acc="prov:p1"`) {
		t.Fatalf("an unsigned subscription must start open:\n%s", body)
	}
	// The reason is on the summary row, so a collapsed card still says what it
	// wants — that is what makes collapsing safe.
	if !strings.Contains(body, "sign sub in") {
		t.Fatalf("the attention pill is missing:\n%s", body)
	}
}
