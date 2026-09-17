package server

// Tests for the dashboard's OAuth account surface: the [[oauth.accounts]]
// entries the provider editor writes, and the device-flow sign-in endpoints
// that turn such an entry into a live credential. The consumer-observable
// facts here are the on-disk TOML after a save, the state the accounts
// endpoint reports, and — for the end-to-end path — the bearer the gateway
// actually sends upstream.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// oauthIdP is a device-flow authorization server with a release valve: while
// held, polls answer authorization_pending (the state a real login sits in
// for minutes), and once released the first poll hands back a token. It also
// counts device starts, so a test can prove a second click re-offers the
// pending prompt instead of starting a second flow.
type oauthIdP struct {
	mu      sync.Mutex
	starts  int
	held    atomic.Bool
	seen    atomic.Value // Authorization header seen by the upstream
	refresh int
}

func (f *oauthIdP) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /device", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.starts++
		n := f.starts
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               fmt.Sprintf("DEV-%d", n),
			"user_code":                 fmt.Sprintf("CODE-%d", n),
			"verification_uri":          "https://idp.example/activate",
			"verification_uri_complete": "https://idp.example/activate?code=CODE-" + fmt.Sprint(n),
			"expires_in":                120,
			"interval":                  1,
		})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") == "refresh_token" {
			f.mu.Lock()
			f.refresh++
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "at-refreshed", "refresh_token": "rt-2", "expires_in": 3600,
			})
			return
		}
		if f.held.Load() {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-live-1", "refresh_token": "rt-1", "expires_in": 3600,
		})
	})
	return mux
}

// startIdP runs the IdP plus an upstream that records the bearer it was sent,
// so a test can prove the stored token reaches the wire.
func (f *oauthIdP) start(t *testing.T) (idpURL, upstreamURL string) {
	t.Helper()
	idp := httptest.NewServer(f.handler())
	t.Cleanup(idp.Close)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.seen.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(up.Close)
	return idp.URL, up.URL
}

// oauthFixture is the config the endpoint tests start from: an xAI provider
// whose only account signs in through the IdP, plus a borrower entry on a
// second provider (which a provider save must never touch) and a secret
// comment to prove the splice leaves foreign lines alone.
func oauthFixture(idpURL, upstreamURL string) string {
	return fmt.Sprintf(`# gateway config (oauth test fixture)
[server]
data_dir = "memory"
admin_password = "pw-test"

[auth]
keys = ["key-a"]

[[providers]]
name = "xai"
kind = "openai"
base_url = %q
models = ["grok-3"]
responses_models = ["grok-4.5*"]
subscription_quota = "grok-cli"
# hand-set line that must survive an editor save
rpm = 6

[[providers.accounts]]
name = "main"

# The login: owns + refreshes the session.
[[oauth.accounts]]
provider = "xai"
account  = "main"
service  = "xai"
device_url = %q
token_url  = %q
client_id  = "test-client"
scope      = "api:access"

[[providers]]
name = "grokbuild"
kind = "openai-responses"
base_url = "http://grokbuild.invalid"
models = ["grok-4.5"]

# Keyless account: its bearer is the borrowed session below.
[[providers.accounts]]
name = "main"

# Borrower: no login of its own, resolves xai/main's token.
[[oauth.accounts]]
provider = "grokbuild"
account  = "main"
owner    = "xai/main"
`, upstreamURL, idpURL+"/device", idpURL+"/token")
}

func decodeJSON[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return v
}

type oauthAccountsResp struct {
	Accounts []struct {
		Key       string `json:"key"`
		Provider  string `json:"provider"`
		Account   string `json:"account"`
		Service   string `json:"service"`
		Owner     string `json:"owner"`
		State     string `json:"state"`
		ExpiresAt string `json:"expires_at"`
		Cooling   bool   `json:"cooling"`
		Error     string `json:"error"`
		Prompt    *struct {
			UserCode string `json:"user_code"`
		} `json:"prompt"`
	} `json:"accounts"`
}

func oauthStateOf(t *testing.T, h http.Handler, key string) (string, string) {
	t.Helper()
	w := adminCall(t, h, http.MethodGet, "/admin/config/oauth/accounts", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET oauth accounts: %d %s", w.Code, w.Body.String())
	}
	for _, a := range decodeJSON[oauthAccountsResp](t, w.Body.String()).Accounts {
		if a.Key == key {
			return a.State, a.ExpiresAt
		}
	}
	t.Fatalf("account %s missing from %s", key, w.Body.String())
	return "", ""
}

func waitOAuthState(t *testing.T, h http.Handler, key, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got, _ = oauthStateOf(t, h, key)
		if got == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("oauth account %s state = %q, want %q", key, got, want)
}

// TestOAuthSignInFromDashboardEndToEnd walks the whole UI path: a device-flow
// login (the forced dialect; xAI's default is the browser flow, covered below)
// started from the endpoint stores the token and the gateway then sends it
// upstream. Without the token the request would carry the empty static key,
// so the recorded bearer is what proves the wiring.
func TestOAuthSignInFromDashboardEndToEnd(t *testing.T) {
	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	_, h, _ := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))

	if state, _ := oauthStateOf(t, h, "xai/main"); state != "signed-out" {
		t.Fatalf("fresh account state = %q, want signed-out", state)
	}

	w := adminCall(t, h, http.MethodPost, "/admin/config/oauth/login?key=xai/main&flow=device", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	body := decodeJSON[struct {
		Key    string `json:"key"`
		State  string `json:"state"`
		Prompt *struct {
			UserCode                string `json:"user_code"`
			VerificationURLComplete string `json:"verification_uri_complete"`
		} `json:"prompt"`
	}](t, w.Body.String())
	if body.Prompt == nil || body.Prompt.UserCode != "CODE-1" {
		t.Fatalf("login response carries no prompt: %s", w.Body.String())
	}
	if !strings.Contains(body.Prompt.VerificationURLComplete, "CODE-1") {
		t.Fatalf("prompt must carry the pre-filled activation link: %+v", body.Prompt)
	}

	waitOAuthState(t, h, "xai/main", "signed-in")
	if _, exp := oauthStateOf(t, h, "xai/main"); exp == "" {
		t.Fatal("signed-in state must report the token expiry")
	}

	// The credential must reach the wire as the upstream bearer.
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"grok-3","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer key-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat through the oauth account: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := idp.seen.Load().(string); got != "Bearer at-live-1" {
		t.Fatalf("upstream saw %q, want the freshly stored token", got)
	}
}

// TestOAuthLoginReoffersPendingPrompt covers the single-active-session rule:
// a second click while a flow is open must return the SAME code rather than
// start a second device login, which would invalidate the first.
func TestOAuthLoginReoffersPendingPrompt(t *testing.T) {
	idp := &oauthIdP{}
	idp.held.Store(true)
	idpURL, upstreamURL := idp.start(t)
	_, h, _ := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))

	first := adminCall(t, h, http.MethodPost, "/admin/config/oauth/login?key=xai/main&flow=device", "", true)
	if first.Code != http.StatusOK {
		t.Fatalf("first login: %d %s", first.Code, first.Body.String())
	}
	if state, _ := oauthStateOf(t, h, "xai/main"); state != "pending" {
		t.Fatalf("state while polling = %q, want pending", state)
	}

	// A second click re-offers the prompt: same user code, one device start.
	second := adminCall(t, h, http.MethodPost, "/admin/config/oauth/login?key=xai/main&flow=device", "", true)
	if second.Code != http.StatusOK {
		t.Fatalf("second login: %d %s", second.Code, second.Body.String())
	}
	get := func(body string) string {
		v := decodeJSON[struct {
			Prompt *struct {
				UserCode string `json:"user_code"`
			} `json:"prompt"`
		}](t, body)
		if v.Prompt == nil {
			return ""
		}
		return v.Prompt.UserCode
	}
	if a, b := get(first.Body.String()), get(second.Body.String()); a == "" || a != b {
		t.Fatalf("second click must re-offer %q, got %q", a, b)
	}
	if n := idp.starts; n != 1 {
		t.Fatalf("device starts = %d, want exactly 1 (two logins would rotate the session)", n)
	}

	idp.held.Store(false)
	waitOAuthState(t, h, "xai/main", "signed-in")
}

// TestOAuthLogoutDeletesStoredToken: sign-out drops the credential, the
// account reports signed-out, and the config entry (the subscription wiring)
// stays — the operator should not have to re-add the account to sign back in.
func TestOAuthLogoutDeletesStoredToken(t *testing.T) {
	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	_, h, path := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))
	before := mustReadFile(t, path)

	adminCall(t, h, http.MethodPost, "/admin/config/oauth/login?key=xai/main&flow=device", "", true)
	waitOAuthState(t, h, "xai/main", "signed-in")

	w := adminCall(t, h, http.MethodPost, "/admin/config/oauth/logout?key=xai/main", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
	if state, _ := oauthStateOf(t, h, "xai/main"); state != "signed-out" {
		t.Fatalf("state after logout = %q, want signed-out", state)
	}
	if after := mustReadFile(t, path); after != before {
		t.Fatalf("sign-out must not rewrite the config:\n%s", after)
	}
}

// TestOAuthLoginAddressesOnlyOwnerEntries: a borrower resolves another
// entry's session, so logging in or out "through" it would rotate the owner's
// device session out from under the gateway.
func TestOAuthLoginAddressesOnlyOwnerEntries(t *testing.T) {
	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	_, h, _ := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))

	for _, path := range []string{
		"/admin/config/oauth/login?key=grokbuild/main",
		"/admin/config/oauth/login?key=nope/nope",
		"/admin/config/oauth/logout?key=grokbuild/main",
	} {
		if w := adminCall(t, h, http.MethodPost, path, "", true); w.Code != http.StatusNotFound {
			t.Fatalf("%s: %d %s, want 404", path, w.Code, w.Body.String())
		}
	}
	// The borrower still reports the owner's state.
	if state, _ := oauthStateOf(t, h, "xai/main"); state != "signed-out" {
		t.Fatalf("owner state changed: %q", state)
	}
}

// TestProviderEditorWritesAndClearsOAuthEntry is the config half of the UI
// feature: ticking the subscription box on an account row must write a
// matching [[oauth.accounts]] entry (with the field's endpoint overrides and
// the borrower entry left alone), and unticking it must remove only that one.
func TestProviderEditorWritesAndClearsOAuthEntry(t *testing.T) {
	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	_, h, path := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))

	body := `{"name":"grokbuild2","kind":"openai-responses","base_url":"http://gb2.invalid","models":["grok-4.5"],
	          "responses_models":["grok-4.6*"],"subscription_quota":"grok-cli",
	          "accounts":[{"name":"ops@example.com","oauth":"xai"}]}`
	if w := adminCall(t, h, http.MethodPut, "/admin/config/providers", body, true); w.Code != http.StatusOK {
		t.Fatalf("add provider: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	for _, want := range []string{
		`account = "ops@example.com"`,
		`service = "xai"`,
		`provider = "grokbuild2"`,
		`responses_models = ["grok-4.6*"]`,
		`subscription_quota = "grok-cli"`,
		// The fixture's own entry and its overrides survive untouched.
		"device_url = " + strconv.Quote(idpURL+"/device"),
		`owner    = "xai/main"`,
		"# hand-set line that must survive an editor save",
	} {
		if !strings.Contains(file, want) {
			t.Fatalf("file missing %q after save:\n%s", want, file)
		}
	}
	if strings.Count(file, "[[oauth.accounts]]") != 3 {
		t.Fatalf("want 3 oauth entries after add, got %d:\n%s", strings.Count(file, "[[oauth.accounts]]"), file)
	}

	// The new account is addressable for sign-in right away (its own entry,
	// not a borrower), which is what makes the UI flow work without a restart.
	if w := adminCall(t, h, http.MethodPost, "/admin/config/oauth/login?key=grokbuild2/ops@example.com&flow=device", "", true); w.Code != http.StatusOK {
		t.Fatalf("login on the new account: %d %s", w.Code, w.Body.String())
	}

	// Unticking the service alone would leave a keyless account that is neither
	// a static key nor a login — it authenticates as nothing while the pool
	// still dials it — so that save is refused and the file does not move.
	untick := `{"name":"grokbuild2","kind":"openai-responses","base_url":"http://gb2.invalid","models":["grok-4.5"],
	          "accounts":[{"name":"ops@example.com"}]}`
	w := adminCall(t, h, http.MethodPut, "/admin/config/providers", untick, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("untick without a key must be refused: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "would authenticate as nothing") {
		t.Fatalf("refusal must say why: %s", w.Body.String())
	}
	if !strings.Contains(mustReadFile(t, path), `provider = "grokbuild2"`) {
		t.Fatalf("refused save must leave the oauth entry alone:\n%s", mustReadFile(t, path))
	}

	// The supported downgrade: give the row a static key and clear the service.
	body = `{"name":"grokbuild2","kind":"openai-responses","base_url":"http://gb2.invalid","models":["grok-4.5"],
	          "accounts":[{"name":"ops@example.com","api_key":"sk-static-1"}]}`
	if w := adminCall(t, h, http.MethodPut, "/admin/config/providers", body, true); w.Code != http.StatusOK {
		t.Fatalf("downgrade save: %d %s", w.Code, w.Body.String())
	}
	file = mustReadFile(t, path)
	if strings.Count(file, "[[oauth.accounts]]") != 2 {
		t.Fatalf("clearing the service must drop exactly one entry:\n%s", file)
	}
	if strings.Contains(file, `provider = "grokbuild2"`) {
		t.Fatalf("oauth entry for the downgraded row survived:\n%s", file)
	}
	if !strings.Contains(file, `name = "ops@example.com"`) {
		t.Fatalf("the account row itself must survive:\n%s", file)
	}
	if !strings.Contains(providerBlock(t, file, "grokbuild2"), `api_key = "sk-static-1"`) {
		t.Fatalf("the downgraded row must keep the key it was given:\n%s", file)
	}
	// Clearing the two provider-level fields removes their lines from that
	// block only (the fixture's own xai block keeps its values: a save must
	// never reach outside the provider it names).
	blk := providerBlock(t, file, "grokbuild2")
	for _, gone := range []string{"responses_models", "subscription_quota"} {
		if strings.Contains(blk, gone) {
			t.Fatalf("cleared %s must be removed from grokbuild2:\n%s", gone, blk)
		}
	}
	if !strings.Contains(providerBlock(t, file, "xai"), `responses_models = ["grok-4.5*"]`) {
		t.Fatalf("xai's own fields must survive:\n%s", providerBlock(t, file, "xai"))
	}
}

// TestProviderEditorPutsNewOAuthAccountOnTop: the dashboard lists a provider's
// newest account first and writes the roster in that order, so the
// [[oauth.accounts]] entries it reconciles must agree — the grid's sign-in
// pills render in entry order. The comment above an existing entry has to stay
// with that entry; an insert on top of the group must not steal it.
func TestProviderEditorPutsNewOAuthAccountOnTop(t *testing.T) {
	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	_, h, path := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))

	body := `{"name":"xai","kind":"openai","accounts":[{"name":"fresh","oauth":"xai"},{"name":"main","oauth":"xai"}]}`
	if w := adminCall(t, h, http.MethodPut, "/admin/config/providers", body, true); w.Code != http.StatusOK {
		t.Fatalf("add oauth account: %d %s", w.Code, w.Body.String())
	}
	file := mustReadFile(t, path)
	fresh := strings.Index(file, `account = "fresh"`)
	comment := strings.Index(file, "# The login: owns + refreshes the session.")
	main := strings.Index(file, `account  = "main"`)
	if fresh < 0 || main < 0 {
		t.Fatalf("both entries must exist:\n%s", file)
	}
	if !(fresh < comment && comment < main) {
		t.Fatalf("new entry must sit above xai/main and keep its comment attached: fresh=%d comment=%d main=%d:\n%s",
			fresh, comment, main, file)
	}
}

// providerBlock returns the text of one [[providers]] block. The account and
// oauth tables that follow it belong to it: the block runs until the next
// [[providers]] header.
func providerBlock(t *testing.T, file, name string) string {
	t.Helper()
	start := strings.Index(file, "[[providers]]\nname = "+strconv.Quote(name))
	if start < 0 {
		t.Fatalf("no [[providers]] block named %s in:\n%s", name, file)
	}
	rest := file[start+1:]
	if end := strings.Index(rest, "[[providers]]"); end >= 0 {
		return file[start : start+1+end]
	}
	return file[start:]
}

// TestProviderEditorLeavesOAuthAloneWithoutAccounts: an update that does not
// carry an account roster (the API's partial update) must not touch the
// [[oauth.accounts]] section at all.
func TestProviderEditorLeavesOAuthAloneWithoutAccounts(t *testing.T) {
	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	_, h, path := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))
	before := mustReadFile(t, path)

	body := `{"name":"xai","kind":"openai","base_url":"http://xai2.invalid","models":["grok-3"]}`
	if w := adminCall(t, h, http.MethodPut, "/admin/config/providers", body, true); w.Code != http.StatusOK {
		t.Fatalf("partial update: %d %s", w.Code, w.Body.String())
	}
	after := mustReadFile(t, path)
	if !strings.Contains(after, `[[oauth.accounts]]`) || strings.Contains(after, "owner    = \"xai/main\"\n\n[[providers]]\nname = \"xai\"") {
		t.Fatalf("oauth section must survive a roster-less update:\n%s", after)
	}
	if got := strings.Count(after, "[[oauth.accounts]]"); got != strings.Count(before, "[[oauth.accounts]]") {
		t.Fatalf("oauth entry count changed: %d -> %d", strings.Count(before, "[[oauth.accounts]]"), got)
	}
	// That update DID rewrite managed fields, so the file is expected to
	// differ — but only outside the oauth blocks.
	if !strings.Contains(after, `base_url = "http://xai2.invalid"`) {
		t.Fatalf("managed field not updated:\n%s", after)
	}
}

// TestProviderEditorRejectsUnknownOAuthService: an unknown service profile
// must fail validation (config.Load) with the file left byte-identical —
// a typo in the UI cannot brick the gateway.
func TestProviderEditorRejectsUnknownOAuthService(t *testing.T) {
	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	_, h, path := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))
	before := mustReadFile(t, path)

	body := `{"name":"xai","kind":"openai","base_url":"http://xai.invalid","accounts":[{"name":"main","oauth":"bogus-service"}]}`
	w := adminCall(t, h, http.MethodPut, "/admin/config/providers", body, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown service: %d %s, want 400", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unknown oauth service") {
		t.Fatalf("error must name the cause: %s", w.Body.String())
	}
	if after := mustReadFile(t, path); after != before {
		t.Fatalf("rejected save must leave the file untouched:\n%s", after)
	}
}

// TestProvidersPageRendersOAuthSignIn pins the HTML surface: the grid must
// render the sign-in button and the device-code dialog for a signed-out
// account, and the editor must carry the OAuth select, the responses-wire
// field and the subscription-quota field. A view field missing from the
// template's data makes dashboard.Render fail, which the page reports as a
// 500 — so a 200 alone is the regression net for the wiring.
func TestProvidersPageRendersOAuthSignIn(t *testing.T) {
	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	_, h, _ := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))

	w := adminCall(t, h, http.MethodGet, "/admin/ui/providers", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("providers page: %d %s", w.Code, w.Body.String())
	}
	page := w.Body.String()
	for _, want := range []string{
		`data-signin="xai/main"`, // the grid's sign-in button for the signed-out account
		`id="oauthdlg"`,          // device-code dialog
		`id="oauth-code"`,        // where the code is shown
		`id="pf-rmodels"`,        // responses_models editor field
		`id="pf-sq"`,             // subscription_quota editor field
		`id="pf-su"`,             // subscription_user editor field (agentrouter)
		`borrows xai/main`,       // borrower row: no button, points at the owner
		`main · xai · signed-out`,
		`const SERVICES = ["cline","clinepass","kilocode","xai"]`, // cline lands with the provider
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing %q", want)
		}
	}
	// A borrower must not offer its own sign-in: that would rotate the
	// owner's single-active device session.
	if strings.Contains(page, `data-signin="grokbuild/main"`) {
		t.Fatalf("borrower must not render a sign-in button:\n%s", page)
	}
	if strings.Contains(page, "access_token") || strings.Contains(page, "rt-1") {
		t.Fatalf("page must never carry token material")
	}
}

// TestProviderViewCarriesOAuthBadges: the grid renders sign-in state from the
// providers API, so the view must carry the service + state per account
// (masked, secret-free) rather than only a count.
func TestProviderViewCarriesOAuthBadges(t *testing.T) {
	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	_, h, _ := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))

	w := adminCall(t, h, http.MethodGet, "/admin/api/v1/providers", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("providers API: %d %s", w.Code, w.Body.String())
	}
	type badge struct {
		Account string `json:"account"`
		Service string `json:"service"`
		State   string `json:"state"`
		Owner   string `json:"owner"`
	}
	views := decodeJSON[[]struct {
		Name  string  `json:"name"`
		OAuth []badge `json:"oauth"`
	}](t, w.Body.String())
	byName := map[string][]badge{}
	for _, v := range views {
		byName[v.Name] = v.OAuth
	}
	if got := byName["xai"]; len(got) != 1 || got[0].Service != "xai" || got[0].State != "signed-out" {
		t.Fatalf("xai badges = %+v, want one signed-out xai account", got)
	}
	if got := byName["grokbuild"]; len(got) != 1 || got[0].Owner != "xai/main" {
		t.Fatalf("borrower badge = %+v, want owner xai/main", got)
	}
	if strings.Contains(w.Body.String(), "rt-1") || strings.Contains(w.Body.String(), "access_token") {
		t.Fatalf("view must never carry token material: %s", w.Body.String())
	}
}

// --- roster round-trip regressions (the save path the OAuth rows ride on) ---

// TestProviderSavePreservesUnmodeledAccountFields: the editor models name /
// key / rpm only, so a field it cannot express must survive a save instead of
// being re-rendered away. Per-account base_url and weight are both live
// (server.go copies them into provider.Account), and losing them silently
// re-points an account or re-weights the pool.
func TestProviderSavePreservesUnmodeledAccountFields(t *testing.T) {
	_, h, path := newTestServerFromFile(t, rosterFixture)
	before := mustReadFile(t, path)

	// A routine save: same roster by name, nothing else mirrored.
	body := `{"name":"p1","kind":"openai","base_url":"http://p1.local","accounts":[{"name":"acct1"}]}`
	if w := adminCall(t, h, http.MethodPut, "/admin/config/providers", body, true); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	after := mustReadFile(t, path)
	for _, want := range []string{
		`base_url = "http://acct1.internal"`, // per-account upstream override (unmodeled)
		"weight = 3",                         // pool selection weight (unmodeled)
		`api_key = "sk-acct1"`,               // blank request key keeps the on-disk one
	} {
		if !strings.Contains(after, want) {
			t.Fatalf("account field %q lost by a roster save:\n%s", want, after)
		}
	}
	// rpm IS modeled (the row has an input, prefilled from the file): a request
	// that leaves it out means "uncapped", so it is legitimately cleared.
	if strings.Contains(after, "rpm = 7") {
		t.Fatalf("a modeled field omitted by the request must clear:\n%s", after)
	}
	if before == after {
		t.Fatal("fixture check: the save must have changed something")
	}
}

// TestProviderSaveRefusesToStrandEnvCredential: once a provider has any
// [[providers.accounts]] entry, the pool is built from those entries alone
// (server.go), so a save that writes a keyless row silently orphans a key that
// came from ONEGW_PROVIDER_<NAME>_KEY — the provider then authenticates as
// nothing, and config.Load cannot see it because a keyless account satisfies
// its "needs api_key, keys, or accounts" check. The save must be refused with
// the env var named, and the running gateway must keep working.
func TestProviderSaveRefusesToStrandEnvCredential(t *testing.T) {
	idp := &oauthIdP{}
	_, upstreamURL := idp.start(t)
	t.Setenv("ONEGW_PROVIDER_ENVP_KEY", "sk-from-env")
	_, h, path := newTestServerFromFile(t, envKeyFixture(upstreamURL))
	before := mustReadFile(t, path)

	chat := func() string {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer key-a")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("chat: %d %s", rec.Code, rec.Body.String())
		}
		got, _ := idp.seen.Load().(string)
		return got
	}
	if got := chat(); got != "Bearer sk-from-env" {
		t.Fatalf("precondition: upstream saw %q, want the env key", got)
	}

	// The editor prefills a synthetic "default" row from the provider key (an
	// env key is invisible to the file), so a routine save posts exactly this.
	body := `{"name":"envp","kind":"openai","base_url":"` + upstreamURL + `","models":["m1"],"accounts":[{"name":"default"}]}`
	w := adminCall(t, h, http.MethodPut, "/admin/config/providers", body, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("keyless roster must be refused, got %d %s\nfile now:\n%s", w.Code, w.Body.String(), mustReadFile(t, path))
	}
	if !strings.Contains(w.Body.String(), "ONEGW_PROVIDER_ENVP_KEY") {
		t.Fatalf("refusal must name the env var that would be orphaned: %s", w.Body.String())
	}
	if after := mustReadFile(t, path); after != before {
		t.Fatalf("refused save must leave the file untouched:\n%s", after)
	}
	if got := chat(); got != "Bearer sk-from-env" {
		t.Fatalf("gateway lost its credential after a refused save: upstream saw %q", got)
	}
}

// TestProviderSaveRefusesToBreakBorrower: unticking the subscription box on an
// account another provider borrows (owner = "<provider>/<account>") would make
// the candidate fail validation with a message about the BORROWER — a config
// the operator did not touch and cannot act on. The splice must name the
// dependency instead.
func TestProviderSaveRefusesToBreakBorrower(t *testing.T) {
	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	_, h, path := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))
	before := mustReadFile(t, path)

	// Save xai with its account no longer marked as a subscription login.
	body := `{"name":"xai","kind":"openai","base_url":"http://xai.invalid","models":["grok-3"],"accounts":[{"name":"main"}]}`
	w := adminCall(t, h, http.MethodPut, "/admin/config/providers", body, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("dropping a borrowed owner must be refused, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "grokbuild/main") {
		t.Fatalf("refusal must name the borrower that depends on it: %s", w.Body.String())
	}
	if after := mustReadFile(t, path); after != before {
		t.Fatalf("refused save must leave the file untouched:\n%s", after)
	}
}

const rosterFixture = `# roster round-trip fixture
[server]
data_dir = "memory"
admin_password = "pw-test"

[auth]
keys = ["key-a"]

[[providers]]
name = "p1"
kind = "openai"
base_url = "http://p1.local"
models = ["m1"]

[[providers.accounts]]
name = "acct1"
api_key = "sk-acct1"
base_url = "http://acct1.internal"
weight = 3
rpm = 7
`

func envKeyFixture(upstreamURL string) string {
	return `# env-credential fixture: the key is NOT in this file
[server]
data_dir = "memory"
admin_password = "pw-test"

[auth]
keys = ["key-a"]

[[providers]]
name = "envp"
kind = "openai"
base_url = "` + upstreamURL + `"
models = ["m1"]
`
}

// ---------------------------------------------------------------------------
// Browser (authorization-code + PKCE) sign-in — the default for xAI
// ---------------------------------------------------------------------------

// ephemeralCallbackPort binds the loopback listener to :0 instead of xAI's
// registered 56121, so the suite never fights a real login helper for the port.
func ephemeralCallbackPort(t *testing.T) {
	t.Helper()
	old := browserCallbackPort
	browserCallbackPort = 0
	t.Cleanup(func() { browserCallbackPort = old })
}

// browserPrompt is the login response's operator-facing half.
type browserPrompt struct {
	Key    string `json:"key"`
	State  string `json:"state"`
	Prompt *struct {
		Mode      string `json:"mode"`
		VerifyURL string `json:"verification_uri_complete"`
		UserCode  string `json:"user_code"`
	} `json:"prompt"`
}

// TestOAuthBrowserSignInEndToEnd is the whole point of the xAI revamp: the
// dashboard hands back a sign-in link, the vendor's redirect lands on our
// loopback port, and the token that comes out of the exchange is the bearer
// the gateway sends upstream — with no code for the operator to type.
func TestOAuthBrowserSignInEndToEnd(t *testing.T) {
	ephemeralCallbackPort(t)
	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	_, h, _ := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))

	w := adminCall(t, h, http.MethodPost, "/admin/config/oauth/login?key=xai/main", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	bp := decodeJSON[browserPrompt](t, w.Body.String())
	if bp.Prompt == nil || bp.Prompt.Mode != "browser" {
		t.Fatalf("xAI must default to the browser dialect: %s", w.Body.String())
	}
	if bp.Prompt.UserCode != "" {
		t.Fatalf("a browser login has no code to type, got %q", bp.Prompt.UserCode)
	}
	authURL, err := url.Parse(bp.Prompt.VerifyURL)
	if err != nil {
		t.Fatalf("authorize url %q: %v", bp.Prompt.VerifyURL, err)
	}
	q := authURL.Query()
	// The parameters the public Grok client is provisioned for; a missing or
	// misspelled one is an invalid_request on xAI's side, invisible to us
	// until an operator stares at a failed login page.
	for key, want := range map[string]string{
		"response_type":         "code",
		"client_id":             "test-client", // the config override wins
		"scope":                 "api:access",
		"code_challenge_method": "S256",
		"plan":                  "generic",
		"referrer":              "cli-proxy-api",
	} {
		if got := q.Get(key); got != want {
			t.Fatalf("authorize %s = %q, want %q (url %s)", key, got, want, authURL)
		}
	}
	if q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" {
		t.Fatalf("authorize must carry challenge/state/nonce: %s", authURL)
	}
	callback, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || callback.Hostname() != "127.0.0.1" {
		t.Fatalf("redirect_uri must be a loopback callback, got %q", q.Get("redirect_uri"))
	}

	// The operator finishes in the browser; we play the browser's last hop.
	res, err := http.Get(callback.String() + "?code=CODE-LIVE&state=" + url.QueryEscape(q.Get("state")))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(page), "Signed in") {
		t.Fatalf("callback page = %d %s", res.StatusCode, firstLines(string(page), 3))
	}

	waitOAuthState(t, h, "xai/main", "signed-in")
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"grok-3","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer key-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat through the browser-signed account: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := idp.seen.Load().(string); got != "Bearer at-live-1" {
		t.Fatalf("upstream saw %q, want the exchanged token", got)
	}
}

// TestOAuthCallbackRefusesUnknownState: the loopback port answers anyone on
// this host who finds it. A code with a state nobody asked for must not be
// exchanged into somebody's session.
func TestOAuthCallbackRefusesUnknownState(t *testing.T) {
	ephemeralCallbackPort(t)
	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	_, h, _ := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))

	w := adminCall(t, h, http.MethodPost, "/admin/config/oauth/login?key=xai/main", "", true)
	q := mustParseQuery(t, decodeJSON[browserPrompt](t, w.Body.String()).Prompt.VerifyURL)
	callback, _ := url.Parse(q.Get("redirect_uri"))

	res, err := http.Get(callback.String() + "?code=attacker-code&state=not-a-real-state")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode == http.StatusOK || !strings.Contains(string(body), "already used or has expired") {
		t.Fatalf("unknown state must be refused, got %d: %s", res.StatusCode, firstLines(string(body), 3))
	}
	if state, _ := oauthStateOf(t, h, "xai/main"); state != "pending" {
		t.Fatalf("a refused callback must leave the login pending, got %q", state)
	}
	// And the real state still works afterwards: the refusal spent nothing.
	res2, err := http.Get(callback.String() + "?code=CODE-OK&state=" + url.QueryEscape(q.Get("state")))
	if err != nil {
		t.Fatalf("second callback: %v", err)
	}
	io.Copy(io.Discard, res2.Body)
	res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("the pending session's own callback = %d, want 200", res2.StatusCode)
	}
	waitOAuthState(t, h, "xai/main", "signed-in")
}

// TestOAuthExchangeEndpointPastedCode is the remote-browser path: the operator
// finishes at the vendor, gets bounced to a loopback address their machine
// cannot serve, and pastes the address bar back into the dashboard.
func TestOAuthExchangeEndpointPastedCode(t *testing.T) {
	ephemeralCallbackPort(t)
	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	_, h, _ := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))

	w := adminCall(t, h, http.MethodPost, "/admin/config/oauth/login?key=xai/main", "", true)
	verify := decodeJSON[browserPrompt](t, w.Body.String()).Prompt.VerifyURL

	// Nothing pending? The endpoint refuses rather than inventing a session.
	if r := adminCall(t, h, http.MethodPost, "/admin/config/oauth/exchange?key=nope/nope", `{"code":"x"}`, true); r.Code != http.StatusNotFound {
		t.Fatalf("exchange on an unknown account = %d, want 404", r.Code)
	}
	// The whole callback URL is what a browser address bar shows; accept it.
	pasted := `http://127.0.0.1:56121/callback?code=PASTE-1&state=` + url.QueryEscape(mustParseQuery(t, verify).Get("state"))
	if r := adminCall(t, h, http.MethodPost, "/admin/config/oauth/exchange?key=xai/main", `{"code":`+strconv.Quote(pasted)+`}`, true); r.Code != http.StatusOK {
		t.Fatalf("pasted-code exchange: %d %s", r.Code, r.Body.String())
	}
	waitOAuthState(t, h, "xai/main", "signed-in")

	// A spent session cannot be exchanged twice.
	if r := adminCall(t, h, http.MethodPost, "/admin/config/oauth/exchange?key=xai/main", `{"code":"PASTE-2"}`, true); r.Code != http.StatusConflict {
		t.Fatalf("second exchange of one session = %d, want 409", r.Code)
	}
}

// TestOAuthExtractCode covers the paste parser's dialects: a bare code, a
// query string, a fragment (some vendors bounce with #code=), and junk.
func TestOAuthExtractCode(t *testing.T) {
	for in, want := range map[string]string{
		"abc123": "abc123",
		"http://127.0.0.1:56121/callback?code=xyz&state=s": "xyz",
		"http://127.0.0.1/callback#code=frag":              "frag",
		"  code=trimmed&scope=openid  ":                    "trimmed",
		"":                                                 "",
	} {
		if got := oauthExtractCode(in); got != want {
			t.Fatalf("oauthExtractCode(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOAuthBrowserListenerClosesAfterSignIn proves the loopback port is not
// left bound once nothing waits on it: a gateway that signed in once must not
// hold a socket for the rest of its life.
func TestOAuthBrowserListenerClosesAfterSignIn(t *testing.T) {
	ephemeralCallbackPort(t)
	old := browserCallbackIdle
	browserCallbackIdle = 50 * time.Millisecond
	t.Cleanup(func() { browserCallbackIdle = old })
	idp := &oauthIdP{}
	idpURL, upstreamURL := idp.start(t)
	srv, h, _ := newTestServerFromFile(t, oauthFixture(idpURL, upstreamURL))

	w := adminCall(t, h, http.MethodPost, "/admin/config/oauth/login?key=xai/main", "", true)
	q := mustParseQuery(t, decodeJSON[browserPrompt](t, w.Body.String()).Prompt.VerifyURL)
	callback, _ := url.Parse(q.Get("redirect_uri"))
	res, err := http.Get(callback.String() + "?code=CODE-CLOSE&state=" + url.QueryEscape(q.Get("state")))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	waitOAuthState(t, h, "xai/main", "signed-in")

	// The grace has passed, so the port must refuse a fresh connection.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", callback.Host, 200*time.Millisecond)
		if err != nil {
			return // closed: the listener released it
		}
		_ = c.Close()
		_ = srv // the Server handle exists for the process, not the socket
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("loopback callback port %s is still accepting after the idle grace", callback.Host)
}

func mustParseQuery(t *testing.T, rawurl string) url.Values {
	t.Helper()
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatalf("parse %q: %v", rawurl, err)
	}
	return u.Query()
}

func firstLines(s string, n int) string {
	parts := strings.Split(strings.TrimSpace(s), "\n")
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, "\n")
}
