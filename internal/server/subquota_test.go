package server

// Subscription-quota e2e (issue #79): opt-in providers probe their vendor's
// usage endpoint, /admin/api/v1/subscription reports the snapshots, the
// quota page renders them, and an exhausted window parks the account so the
// pool skips it.

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"onegw/internal/config"
)

func subTestConfig(t *testing.T, ocURL, zaiURL string) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Providers = []config.ProviderCfg{
		{Name: "oc", Kind: "openai", BaseURL: "http://127.0.0.1:1/v1", APIKey: "sk-oc",
			Models: []string{"oc/m"}, SubscriptionQuota: "opencode-go", SubscriptionURL: ocURL},
		{Name: "z", Kind: "openai", BaseURL: "http://127.0.0.1:1/v1", APIKey: "sk-z",
			Models: []string{"z/m"}, SubscriptionQuota: "zai", SubscriptionURL: zaiURL},
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	return cfg
}

func waitSubSnapshots(t *testing.T, srv *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if subs := srv.subscriptionSnapshots(); len(subs) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscription snapshots never reached %d (have %d)", want, len(srv.subscriptionSnapshots()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSubscriptionQuotaAPIPageAndPark(t *testing.T) {
	// OpenCode Go stub: healthy account (13% rolling).
	oc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-oc" {
			t.Errorf("opencode probe auth = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{
			"rolling": map[string]any{"percent": 13, "resetsAt": time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)},
			"weekly":  map[string]any{"percent": 5},
			"monthly": map[string]any{"percent": 2},
		}})
	}))
	defer oc.Close()

	// z.ai stub: exhausted session window (100%) with a reset time — must
	// park the account; bearer key check included.
	zai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-z" {
			t.Errorf("zai probe auth = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "success": true, "data": map[string]any{
			"level": "lite",
			"limits": []any{map[string]any{
				"type": "CREDIT_LIMIT", "unit": 3, "number": 5,
				"percentage":    100,
				"nextResetTime": time.Now().Add(30 * time.Minute).UnixMilli(),
			}},
		}})
	}))
	defer zai.Close()

	cfg := subTestConfig(t, oc.URL, zai.URL)
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()

	waitSubSnapshots(t, srv, 2)

	// JSON surface reports both accounts with their windows.
	w := do(t, srv.Handler(), adminReq(t, "/admin/api/v1/subscription"))
	if w.Code != http.StatusOK {
		t.Fatalf("subscription API: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"plan":"OpenCode Go"`) || !strings.Contains(body, `"plan":"Lite"`) {
		t.Fatalf("plans missing: %s", body)
	}
	if !strings.Contains(body, `"name":"Session (5h)"`) || !strings.Contains(body, `"name":"Weekly"`) {
		t.Fatalf("windows missing: %s", body)
	}

	// Dashboard page renders the subscription section, and renders it
	// per-window: the parked zai account carries exactly one parked
	// marker (account cell), the healthy opencode account none.
	w = do(t, srv.Handler(), adminReq(t, "/admin/ui/quota"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Subscription quota") {
		t.Fatalf("quota page: %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no quota windows") {
		t.Fatal("local-window empty state must survive the page rework")
	}
	if n := strings.Count(w.Body.String(), ">parked<"); n != 1 {
		t.Fatalf("parked marker count = %d, want exactly the parked zai account", n)
	}

	// The exhausted zai account is parked: pool returns nothing ready.
	def, ok := srv.cur().pool.Get("z")
	if !ok {
		t.Fatal("provider z missing from pool")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		acct, ready := def.NextAccount("")
		if acct == nil && ready.After(time.Now()) {
			break // parked until `ready`
		}
		if time.Now().After(deadline) {
			t.Fatalf("exhausted account never parked (acct=%v ready=%v)", acct, ready)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The healthy oc account keeps serving: its next() still yields.
	ocDef, ok := srv.cur().pool.Get("oc")
	if !ok {
		t.Fatal("provider oc missing from pool")
	}
	if a, _ := ocDef.NextAccount(""); a == nil {
		t.Fatal("healthy account must not be parked")
	}
}

func TestSubscriptionQuotaDisabledByDefault(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Providers = []config.ProviderCfg{
		{Name: "plain", Kind: "openai", BaseURL: "http://127.0.0.1:1/v1", APIKey: "sk-x", Models: []string{"plain/m"}},
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()
	if srv.cur().subq != nil {
		t.Fatal("no subscription_quota providers must build no tracker")
	}
	w := do(t, srv.Handler(), adminReq(t, "/admin/api/v1/subscription"))
	if !strings.Contains(w.Body.String(), `"accounts":[]`) {
		t.Fatalf("empty accounts expected: %s", w.Body.String())
	}
	// The quota page still renders with the empty-state lines for both tables.
	w = do(t, srv.Handler(), adminReq(t, "/admin/ui/quota"))
	if !strings.Contains(w.Body.String(), "no subscription providers") {
		t.Fatalf("subscription empty state missing: %s", w.Body.String())
	}
}

func TestSubscriptionQuotaValidation(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Providers = []config.ProviderCfg{
		{Name: "bad", Kind: "openai", BaseURL: "http://127.0.0.1:1/v1", APIKey: "sk-x",
			SubscriptionQuota: "carrier-pigeon"},
	}
	cfg.Defaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "subscription_quota") {
		t.Fatalf("unknown dialect must fail validation, got %v", err)
	}
}

// TestSuperGrokBorrowedSessionServesAndTracks is the end-to-end proof of the
// SuperGrok wiring: ONE stored device session carries two different xAI
// surfaces — the OpenAI chat provider and a borrowed Grok Build provider on
// the Responses wire — while the vendor's weekly pool lands on
// /admin/api/v1/subscription at a percent that must NOT park the account.
//
// The session is seeded into the data dir BEFORE the server starts, because
// the tracker polls immediately on New(): logging in mid-test would race that
// first probe and make the assertions flaky.
func TestSuperGrokBorrowedSessionServesAndTracks(t *testing.T) {
	var mu sync.Mutex
	auth := map[string]string{}
	var respBody, billingMode string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		auth[r.URL.Path] = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/v1/billing":
			billingMode = r.Header.Get("x-grok-client-mode")
		case "/v1/responses":
			respBody = string(body)
			auth["xai-token-auth"] = r.Header.Get("X-XAI-Token-Auth")
			auth["grok-cli-version"] = r.Header.Get("x-grok-cli-version")
		}
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/billing":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":42.7,` +
				`"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-09-20T00:00:00Z"}}}`))
		case "/v1/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.created\n"+
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"grok-build\"}}\n\n"+
				"event: response.output_text.delta\n"+
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"+
				"event: response.completed\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\","+
				"\"usage\":{\"input_tokens\":7,\"output_tokens\":1}}}\n\n")
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"grok-4.6",` +
				`"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer stub.Close()

	dir := t.TempDir()
	now := time.Now().UTC()
	seed := `{"tokens":{"xai/main":{"access_token":"at-live-1","refresh_token":"rt-live-1",` +
		`"expires_at":"` + now.Add(30*time.Minute).Format(time.RFC3339) +
		`","updated_at":"` + now.Format(time.RFC3339) + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "oauth-tokens.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Server.DataDir = dir
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Providers = []config.ProviderCfg{
		{Name: "xai", Kind: "openai", BaseURL: stub.URL,
			Accounts:          []config.Acct{{Name: "main", APIKey: "sk-static-fallback"}},
			Models:            []string{"grok-4.6"},
			SubscriptionQuota: "grok-cli", SubscriptionURL: stub.URL + "/v1/billing?format=credits"},
		{Name: "grokbuild", Kind: "openai-responses", BaseURL: stub.URL,
			Accounts: []config.Acct{{Name: "main", APIKey: "sk-static-fallback"}},
			Models:   []string{"grok-build"}},
	}
	// The borrower declares no service: OAuthAccounts() defaults it from the
	// provider name, and validating that value rejected every real borrower.
	cfg.OAuth.Accounts = []config.OAuthAccount{
		{Provider: "xai", Account: "main", Service: "xai"},
		{Provider: "grokbuild", Account: "main", Owner: "xai/main"},
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("borrower config rejected: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()

	waitSubSnapshots(t, srv, 1)

	// 1) The weekly pool is tracked, and a partial pool keeps the account live.
	w := do(t, srv.Handler(), adminReq(t, "/admin/api/v1/subscription"))
	if w.Code != http.StatusOK {
		t.Fatalf("subscription API: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"name":"Weekly pool"`) || !strings.Contains(body, `"used":42`) {
		t.Fatalf("weekly pool missing or misread: %s", body)
	}
	rec := do(t, srv.Handler(), oauthChat("xai/grok-4.6", "sk-test-gw"))
	if rec.Code != http.StatusOK {
		t.Fatalf("42%% pool parked the account: %d %s", rec.Code, rec.Body.String())
	}

	gb := do(t, srv.Handler(), oauthChat("grokbuild/grok-build", "sk-test-gw"))
	if gb.Code != http.StatusOK {
		t.Fatalf("grokbuild over the Responses wire: %d %s", gb.Code, gb.Body.String())
	}
	if !strings.Contains(gb.Body.String(), "OK") {
		t.Fatalf("grokbuild reply lost the streamed text: %s", gb.Body.String())
	}
	// 2) Both surfaces ride the managed session, not the static fallback key.
	mu.Lock()
	bearer, chatBearer := auth["/v1/billing"], auth["/v1/chat/completions"]
	grokBearer, tokenAuth, cliVer, respBodyCopy := auth["/v1/responses"], auth["xai-token-auth"], auth["grok-cli-version"], respBody
	mode := billingMode
	mu.Unlock()
	for name, got := range map[string]string{
		"billing probe":  bearer,
		"chat provider":  chatBearer,
		"responses wire": grokBearer,
	} {
		if got != "Bearer at-live-1" {
			t.Fatalf("%s sent %q, want the managed OAuth session", name, got)
		}
	}
	if mode != "cli" {
		t.Fatalf("billing x-grok-client-mode = %q, want cli", mode)
	}
	if tokenAuth != "xai-grok-cli" || cliVer == "" {
		t.Fatalf("Grok Build fingerprint headers = %q/%q, want xai-grok-cli + a cli version", tokenAuth, cliVer)
	}
	if !strings.Contains(respBodyCopy, `"input"`) || !strings.Contains(respBodyCopy, `"store":false`) {
		t.Fatalf("upstream body is not the forced Responses shape: %s", respBodyCopy)
	}
}

// TestSubscriptionQuotaTracksOAuthOnlyAccount pins the two halves of the
// target rule: an account with no static api_key still gets a quota row when
// an [[oauth.accounts]] entry owns its credential (the shape a subscription
// provider takes once the dead fallback key is deleted from TOML), while a
// credential-less account with no OAuth entry is skipped outright.
func TestSubscriptionQuotaTracksOAuthOnlyAccount(t *testing.T) {
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // no bearer stored yet
	}))
	defer vendor.Close()

	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Providers = []config.ProviderCfg{
		{Name: "oa", Kind: "openai", BaseURL: "http://127.0.0.1:1/v1",
			Accounts: []config.Acct{{Name: "main"}}, // no api_key at all
			Models:   []string{"oa/m"}, SubscriptionQuota: "zai", SubscriptionURL: vendor.URL},
		{Name: "kn", Kind: "openai", BaseURL: "http://127.0.0.1:1/v1",
			Accounts: []config.Acct{{Name: "main"}}, // no api_key, no oauth entry
			Models:   []string{"kn/m"}, SubscriptionQuota: "zai", SubscriptionURL: vendor.URL},
	}
	cfg.OAuth.Accounts = []config.OAuthAccount{{Provider: "oa", Account: "main", Service: "xai"}}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("keyless oauth account rejected: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()

	waitSubSnapshots(t, srv, 1)
	subs := srv.subscriptionSnapshots()
	if len(subs) != 1 || subs[0].Provider != "oa" {
		t.Fatalf("snapshots = %+v, want the OAuth-managed account only", subs)
	}
	// Fail-open, but VISIBLE: an operator must see why the window is unknown.
	if subs[0].Err == "" {
		t.Fatalf("unprobed account must still report its failure, got %+v", subs[0])
	}
}

// TestSubscriptionQuotaCursorDialect is the cursor dialect's page-level proof
// (2026-09-14): the probe authenticates with the account's OWN session JWT as
// a browser cookie, one spent account parks while its sibling keeps serving,
// and /admin/ui/quota renders both rows. The page render is the load-bearing
// half — a template asking for a field the view does not carry 500s with
// nothing in the gateway log, which the JSON-twin assertions would sail
// straight through.
func TestSubscriptionQuotaCursorDialect(t *testing.T) {
	jwt := func(sub string) string {
		head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + sub + `"}`))
		return head + "." + payload + ".sig"
	}
	usage := func(used, cap int) string {
		return `{"gpt-4":{"numRequests":` + strconv.Itoa(used) + `,"maxRequestUsage":` + strconv.Itoa(cap) +
			`},"startOfMonth":"2026-09-01T00:00:00.000Z"}`
	}
	cs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("user") {
		case "user_01SPENT":
			_, _ = w.Write([]byte(usage(1000, 1000))) // pool fully consumed
		default:
			_, _ = w.Write([]byte(usage(10, 1000)))
		}
	}))
	defer cs.Close()

	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Providers = []config.ProviderCfg{{
		Name: "cursor", Kind: "cursor", Models: []string{"cursor/auto"},
		SubscriptionQuota: "cursor", SubscriptionURL: cs.URL,
		Accounts: []config.Acct{
			{Name: "spent", APIKey: jwt("auth0|user_01SPENT")},
			{Name: "open", APIKey: jwt("grok|user_01OPEN")},
		},
	}}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid (cursor must be an accepted dialect): %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()

	waitSubSnapshots(t, srv, 2)

	w := do(t, srv.Handler(), adminReq(t, "/admin/api/v1/subscription"))
	if body := w.Body.String(); !strings.Contains(body, `"plan":"1000 req/mo"`) || !strings.Contains(body, `"used":100`) {
		t.Fatalf("subscription API missing the cursor pool: %d %s", w.Code, body)
	}

	// The parked marker belongs to the spent account only.
	page := do(t, srv.Handler(), adminReq(t, "/admin/ui/quota"))
	if page.Code != http.StatusOK {
		t.Fatalf("quota page: %d %s", page.Code, page.Body.String())
	}
	html := page.Body.String()
	if !strings.Contains(html, ">cursor<") {
		t.Fatalf("quota page lost the cursor row: %s", html)
	}
	if n := strings.Count(html, ">parked<"); n != 1 {
		t.Fatalf("parked marker count = %d, want exactly the spent account: %s", n, html)
	}
	if !strings.Contains(html, "exhausted") {
		t.Fatal("exhausted pill missing for the spent window")
	}

	// Pool effect: the spent slot is cooled, so every pick lands on the
	// account that still has headroom.
	def, ok := srv.cur().pool.Get("cursor")
	if !ok {
		t.Fatal("cursor missing from pool")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		served := map[string]int{}
		for range 6 {
			if a, _ := def.NextAccount(""); a != nil {
				served[a.Name]++
			}
		}
		if served["open"] == 6 && served["spent"] == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("spent account never parked from the pool: %v", served)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSubscriptionQuotaFreebuffAndAgentRouterDialects proves the two newer
// dialects are wired end to end through the SERVER, not just in the parser:
// subscriptions.targets must carry subscription_user to agentrouter's
// New-Api-User header, and the freebuff probe must be a POST to codebuff's
// session endpoint (the shared bearer GET would 401 there).
func TestSubscriptionQuotaFreebuffAndAgentRouterDialects(t *testing.T) {
	var mu sync.Mutex
	var fbMethod, fbBody, fbKey, arUser, arAuth string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/freebuff/session"):
			fbMethod, fbBody, fbKey = r.Method, string(body), r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"status":"active","accessTier":"limited",
				"freebucks":{"balance":10,"daily":{"limit":25,"spent":15,"remaining":10,
				  "resetAt":"2026-09-17T07:00:00.000Z"}},
				"rateLimit":{"model":"deepseek/deepseek-v4-flash","limit":6,"recentCount":1,"poolLabel":"Daily"}}`))
		default:
			arUser, arAuth = r.Header.Get("New-Api-User"), r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"data":{"quota":750000},"success":true}`))
		}
		mu.Unlock()
	}))
	defer stub.Close()

	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Providers = []config.ProviderCfg{
		{Name: "fb", Kind: "openai", BaseURL: "http://127.0.0.1:1/v1", APIKey: "fb-token",
			Models: []string{"fb/m"}, SubscriptionQuota: "freebuff", SubscriptionURL: stub.URL + "/freebuff/session"},
		{Name: "ar", Kind: "openai", BaseURL: "http://127.0.0.1:1/v1", APIKey: "sat-token",
			Models: []string{"ar/m"}, SubscriptionQuota: "agentrouter", SubscriptionURL: stub.URL + "/api/user/self",
			SubscriptionUser: "14823"},
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid (both dialects must validate): %v", err)
	}
	// The user id is meaningless without its dialect, so the config layer
	// rejects it rather than shipping a probe that can only 401.
	cfg.Providers[0].SubscriptionUser = "14823"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "subscription_user") {
		t.Fatalf("subscription_user outside agentrouter must fail validation, got %v", err)
	}
	cfg.Providers[0].SubscriptionUser = ""

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()
	waitSubSnapshots(t, srv, 2)

	w := do(t, srv.Handler(), adminReq(t, "/admin/api/v1/subscription"))
	body := w.Body.String()
	for _, want := range []string{`"plan":"freebuff limited"`, `"Wallet ($1.50 left)"`, `"AgentRouter"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("subscription API missing %s: %d %s", want, w.Code, body)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if fbMethod != http.MethodPost || fbBody != "{}" || fbKey != "Bearer fb-token" {
		t.Fatalf("freebuff probe = %s body=%q auth=%q", fbMethod, fbBody, fbKey)
	}
	if arUser != "14823" || arAuth != "Bearer sat-token" {
		t.Fatalf("agentrouter probe headers: New-Api-User=%q auth=%q", arUser, arAuth)
	}
}
