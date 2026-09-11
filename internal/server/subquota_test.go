package server

// Subscription-quota e2e (issue #79): opt-in providers probe their vendor's
// usage endpoint, /admin/api/v1/subscription reports the snapshots, the
// quota page renders them, and an exhausted window parks the account so the
// pool skips it.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
				"percentage":   100,
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

	// Dashboard page renders the subscription section.
	w = do(t, srv.Handler(), adminReq(t, "/admin/ui/quota"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Subscription quota") {
		t.Fatalf("quota page: %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no quota windows") {
		t.Fatal("local-window empty state must survive the page rework")
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
