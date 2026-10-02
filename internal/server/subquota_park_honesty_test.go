package server

// A subscription-quota park (#79) must answer honestly: the account is
// cooling because the VENDOR reports a window fully consumed, not because the
// upstream rate-limited anything. Before this, such a pool-empty fell to
// DefaultPoolEmptyError's 429 "all accounts rate-limited upstream; retry
// after Ns" — an untrue cause and a remedy (wait) that never arrives while
// the pool sits at zero spendable credits.
//
// Live 2026-10-02: commandcode/linh on the operator's gateway answered
//   429 {"code":"rate_limit_exceeded",
//        "message":"provider commandcode: all accounts rate-limited upstream; retry after 23s"}
// while /admin/ui/quota showed the same account "Credits (monthly) 100%
// parked · exhausted" — the vendor's numbers and the client-facing error
// telling two different stories about the same state.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"onegw/internal/config"
)

// exhaustedSubUpstream is the vendor's usage API reporting a single window
// at 100%. Only the poller ever calls it — the provider's chat base_url is a
// dead port, so a served request there is a connection error, never a
// silent success.
func exhaustedSubUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "success": true,
			"data": map[string]any{"level": "go", "limits": []any{map[string]any{
				"type": "CREDIT_LIMIT", "unit": 3, "number": 5, "percentage": 100,
				"nextResetTime": time.Now().Add(30 * time.Minute).UnixMilli(),
			}}},
		})
	}))
}

// parkedSubCfg is one subscription-quota provider whose vendor reports an
// exhausted window, plus a healthy fallback leg in combo "pair".
func parkedSubCfg(t *testing.T, subURL, fbURL string) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Providers = []config.ProviderCfg{
		{Name: "z", Kind: "openai", BaseURL: "http://127.0.0.1:1/v1", APIKey: "sk-z",
			Models: []string{"z/m"}, SubscriptionQuota: "zai", SubscriptionURL: subURL},
		{Name: "fb", Kind: "openai", BaseURL: fbURL, APIKey: "sk-fb", Models: []string{"fb/m"}},
	}
	cfg.Combos = []config.ComboCfg{{Name: "pair", Targets: []string{"z/m", "fb/m"}}}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	return cfg
}

// waitParked polls until the provider's only account is parked.
func waitParked(t *testing.T, srv *Server, name string) {
	t.Helper()
	def, ok := srv.cur().pool.Get(name)
	if !ok {
		t.Fatalf("provider %s missing from pool", name)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if acct, ready := def.NextAccount(""); acct == nil && ready.After(time.Now()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider %s account never parked", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestParkedSubscriptionAnswersQuotaExhaustedNotRateLimited(t *testing.T) {
	sub := exhaustedSubUpstream(t)
	defer sub.Close()
	fb := newCountingUpstream("m")
	defer fb.srv.Close()

	srv, err := New(parkedSubCfg(t, sub.URL, fb.srv.URL))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()
	waitSubSnapshots(t, srv, 1)
	waitParked(t, srv, "z")

	// Direct route to the parked leg: the honest answer is 503
	// provider_quota_exhausted naming the window — NOT the 429
	// "all accounts rate-limited upstream" the default pool-empty lies with.
	w := do(t, srv.Handler(), authed(t, "z/m", "sk-test-gw"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("parked leg status = %d, want 503 (body: %s)", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, w.Body.String())
	}
	e, _ := body["error"].(map[string]any)
	if e == nil || e["type"] != "provider_quota_exhausted" || e["code"] != "quota_exceeded" {
		t.Fatalf("error shape = %v, want provider_quota_exhausted/quota_exceeded", e)
	}
	msg, _ := e["message"].(string)
	for _, want := range []string{"z", "Session (5h)", "subscription quota exhausted"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q must name %q", msg, want)
		}
	}
	if strings.Contains(msg, "rate-limited") {
		t.Fatalf("message must not claim an upstream rate limit: %q", msg)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After = %q, want the vendor reset in seconds", ra)
	}
}

// A combo still falls through: the parked leg answers 503 and the healthy
// sibling serves, which is the contract the fall-through depends on.
func TestParkedSubscriptionFallsThroughCombo(t *testing.T) {
	sub := exhaustedSubUpstream(t)
	defer sub.Close()
	fb := newCountingUpstream("m")
	defer fb.srv.Close()

	srv, err := New(parkedSubCfg(t, sub.URL, fb.srv.URL))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()
	waitSubSnapshots(t, srv, 1)
	waitParked(t, srv, "z")

	w := do(t, srv.Handler(), authed(t, "pair", "sk-test-gw"))
	if w.Code != http.StatusOK {
		t.Fatalf("combo request: %d (%s) — the parked leg must fall through", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pong from m") {
		t.Fatalf("combo did not serve from the fallback leg: %s", w.Body.String())
	}
}

// Fail-open: a vendor probe ERROR never parks, so a provider whose usage API
// is down keeps serving — the new branch must not indict a provider on no
// evidence.
func TestSubscriptionProbeErrorKeepsAccountServing(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	fb := newCountingUpstream("m")
	defer fb.srv.Close()

	srv, err := New(parkedSubCfg(t, broken.URL, fb.srv.URL))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		failed := false
		for _, s := range srv.subscriptionSnapshots() {
			if s.Provider == "z" && s.Err != "" {
				failed = true
			}
		}
		if failed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probe error never landed in the snapshot")
		}
		time.Sleep(10 * time.Millisecond)
	}

	def, _ := srv.cur().pool.Get("z")
	if acct, _ := def.NextAccount(""); acct == nil {
		t.Fatal("a failed probe must not park the account")
	}

	// The combo still serves end to end.
	w := do(t, srv.Handler(), authed(t, "pair", "sk-test-gw"))
	if w.Code != http.StatusOK {
		t.Fatalf("combo request after a failed probe: %d (%s)", w.Code, w.Body.String())
	}
}
