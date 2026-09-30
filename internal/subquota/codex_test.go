package subquota

// The Codex (ChatGPT Plus/Pro) subscription-quota dialect: the window labels
// follow the reported duration rather than the primary/secondary position, a
// never-started window is dropped instead of parking on a permanent 0%, and
// the probe carries the codex-cli identity so the meter cannot read it as an
// anonymous account.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"onegw/internal/oauth"
)

// codexTestBearer is an unsigned ChatGPT-shaped access token carrying a
// workspace id; only the payload is ever decoded.
func codexTestBearer(t *testing.T, accountID string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func TestParseCodexWindows(t *testing.T) {
	// A weekly window in the PRIMARY slot and a 5h one in the secondary: the
	// labels must follow the reported duration, not the position, or the
	// Quota page inverts the two.
	body := []byte(`{"plan_type":"plus","rate_limit":{
		"primary_window":{"used_percent":42,"limit_window_seconds":604800,"reset_at":1789660800},
		"secondary_window":{"used_percent":7,"limit_window_seconds":18000,"reset_after_seconds":9000}}}`)
	windows, plan, err := parseCodex(body, 200)
	if err != "" {
		t.Fatalf("unexpected error: %s", err)
	}
	if plan != "plus" {
		t.Errorf("plan = %q, want plus", plan)
	}
	if len(windows) != 2 {
		t.Fatalf("want 2 windows, got %d: %+v", len(windows), windows)
	}
	if windows[0].Name != "Weekly" || windows[0].Used != 42 {
		t.Errorf("window 0 = %+v, want Weekly at 42%%", windows[0])
	}
	if windows[0].Resets == nil || windows[0].Resets.Unix() != 1789660800 {
		t.Errorf("window 0 reset = %v, want the absolute reset_at", windows[0].Resets)
	}
	if windows[1].Name != "Session" || windows[1].Used != 7 {
		t.Errorf("window 1 = %+v, want Session at 7%%", windows[1])
	}
	if windows[1].Resets == nil || windows[1].Resets.Before(time.Now()) {
		t.Errorf("window 1 reset = %v, want now+reset_after_seconds", windows[1].Resets)
	}
}

// ChatGPT advertises latent per-feature ceilings that recompute their reset on
// every fetch. Rendering one as a 0%-used window with a full-window reset
// would park nothing but read as a permanent row; drop it.
func TestParseCodexDropsLatentWindow(t *testing.T) {
	body := []byte(`{"plan_type":"pro","rate_limit":{
		"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},
		"secondary_window":{"used_percent":91,"limit_window_seconds":604800,"reset_at":1789660800}}}`)
	windows, _, err := parseCodex(body, 200)
	if err != "" {
		t.Fatalf("unexpected error: %s", err)
	}
	if len(windows) != 1 || windows[0].Name != "Weekly" {
		t.Fatalf("windows = %+v, want only the used weekly one", windows)
	}
}

func TestParseCodexErrors(t *testing.T) {
	if _, _, err := parseCodex([]byte(`{}`), 401); err == "" {
		t.Error("a 401 must report a re-login hint, not an empty snapshot")
	}
	if _, _, err := parseCodex([]byte(`not json`), 200); err == "" {
		t.Error("a non-JSON body must report an error")
	}
	if _, _, err := parseCodex([]byte(`{"plan_type":"plus"}`), 200); err == "" {
		t.Error("a payload with no windows must report an error")
	}
}

func TestProbeCodexCarriesCLIIdentity(t *testing.T) {
	var ua, originator, version, accountID, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		originator = r.Header.Get("originator")
		version = r.Header.Get("Version")
		accountID = r.Header.Get("chatgpt-account-id")
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{
			"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":60}}}`))
	}))
	defer srv.Close()

	bearer := codexTestBearer(t, "workspace-xyz")
	parked := make(chan struct{}, 1)
	tr := NewAt([]Target{{Provider: "codex", AcctName: "main", AcctKey: bearer,
		Dialect: Codex, URL: srv.URL}}, func(Target, time.Time) { parked <- struct{}{} },
		nil, nil, time.Hour, nil, nil)
	defer tr.Stop()
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("a 100% session window never parked the account")
	}
	snaps := tr.All()
	if len(snaps) != 1 || snaps[0].Err != "" {
		t.Fatalf("probe failed: %+v", snaps)
	}
	if snaps[0].Plan != "plus" || snaps[0].Windows[0].Name != "Session" {
		t.Fatalf("snapshot = %+v", snaps[0])
	}
	// The meter keys its answer on the CLI identity; without it the probe
	// reads upstream as an anonymous half-account.
	if ua != oauth.CodexUserAgent {
		t.Errorf("User-Agent = %q", ua)
	}
	if originator != oauth.CodexOriginator || version != oauth.CodexClientVersion {
		t.Errorf("originator=%q version=%q", originator, version)
	}
	if accountID != "workspace-xyz" {
		t.Errorf("chatgpt-account-id = %q", accountID)
	}
	if auth != "Bearer "+bearer {
		t.Errorf("Authorization = %q", auth)
	}
}

// A non-ChatGPT bearer must not get a workspace header — and must not fail the
// probe either (fail-open: the snapshot keeps showing whatever it had).
func TestProbeCodexOmitsWorkspaceForForeignBearer(t *testing.T) {
	var accountID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accountID = r.Header.Get("chatgpt-account-id")
		_, _ = w.Write([]byte(`{"rate_limit":{
			"primary_window":{"used_percent":5,"limit_window_seconds":18000,"reset_after_seconds":60}}}`))
	}))
	defer srv.Close()

	tr := NewAt([]Target{{Provider: "codex", AcctName: "main", AcctKey: "sk-proj-opaque",
		Dialect: Codex, URL: srv.URL}}, nil, nil, nil, time.Hour, nil, nil)
	defer tr.Stop()
	tr.poll()
	if accountID != "" {
		t.Errorf("invented a workspace header for a non-ChatGPT key: %q", accountID)
	}
	if snaps := tr.All(); len(snaps) != 1 || snaps[0].Err != "" {
		t.Fatalf("probe failed: %+v", snaps)
	}
}

// A payload that omits plan_type still labels the row: the plan claim is on
// the same bearer, so the Quota page is not blank just because the vendor
// stopped repeating the plan in the meter body.
func TestProbeCodexPlanFallsBackToTokenClaim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"rate_limit":{
			"primary_window":{"used_percent":5,"limit_window_seconds":18000,"reset_after_seconds":60}}}`))
	}))
	defer srv.Close()

	bearer := "h." + base64.RawURLEncoding.EncodeToString(
		[]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"ws-9","chatgpt_plan_type":"pro"}}`)) + ".s"
	tr := NewAt([]Target{{Provider: "codex", AcctName: "main", AcctKey: bearer,
		Dialect: Codex, URL: srv.URL}}, nil, nil, nil, time.Hour, nil, nil)
	defer tr.Stop()
	tr.poll()
	snaps := tr.All()
	if len(snaps) != 1 || snaps[0].Err != "" {
		t.Fatalf("probe failed: %+v", snaps)
	}
	if snaps[0].Plan != "pro" {
		t.Fatalf("plan = %q, want the token claim", snaps[0].Plan)
	}
}

func TestCodexDialectIsRegistered(t *testing.T) {
	if !ValidDialect(Codex) {
		t.Fatal("codex is not a valid subscription_quota dialect")
	}
	if DefaultURL(Codex) != "https://chatgpt.com/backend-api/wham/usage" {
		t.Fatalf("DefaultURL(codex) = %q", DefaultURL(Codex))
	}
}

// The payload carries three more metered buckets beyond the plan's own pair
// (official RateLimitStatusPayload shape): named additional_rate_limits and
// the account's spend_control individual credit cap. Omitting them means a
// user whose code-review or monthly-credit limit is the binding one sees a
// healthy account — so they must surface, labelled, and be parkable.
func TestParseCodexAdditionalLimitsAndCreditCap(t *testing.T) {
	body := []byte(`{"plan_type":"pro","rate_limit":{
		"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":900},
		"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":400000}},
		"additional_rate_limits":[
			{"limit_name":"code_review","metered_feature":"code_review","rate_limit":{
				"primary_window":{"used_percent":80,"limit_window_seconds":604800,"reset_after_seconds":100000}}},
			{"limit_name":"spark","rate_limit":{}},
			{"limit_name":"latent","rate_limit":{
				"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":604800}}}],
		"spend_control":{"reached":true,"individual_limit":{
			"limit":"120","used":"118","used_percent":98,
			"reset_after_seconds":86400}}}`)

	windows, plan, err := parseCodex(body, 200)
	if err != "" {
		t.Fatalf("unexpected error: %s", err)
	}
	if plan != "pro" {
		t.Errorf("plan = %q, want pro", plan)
	}
	got := map[string]Window{}
	for _, w := range windows {
		got[w.Name] = w
	}
	// The plan's own two keep their plain labels.
	if w := got["Session"]; w.Used != 10 {
		t.Errorf("plan session = %+v, want 10%%", w)
	}
	if w := got["Weekly"]; w.Used != 20 {
		t.Errorf("plan weekly = %+v, want 20%%", w)
	}
	// A named bucket prefixes its label, so two features cannot collide.
	if w := got["code_review · Weekly"]; w.Used != 80 {
		t.Errorf("code review = %+v, want 80%%", w)
	}
	// An empty bucket (metadata only) and a latent one both yield no row.
	for _, unwanted := range []string{"spark · Weekly", "spark · Session", "latent · Weekly"} {
		if _, ok := got[unwanted]; ok {
			t.Errorf("empty/latent bucket rendered a row: %s", unwanted)
		}
	}
	// The credit cap is the binding limit and must park.
	w, ok := got["Monthly credit limit"]
	if !ok {
		t.Fatalf("spend_control cap missing: %+v", windows)
	}
	if w.Used != 98 {
		t.Errorf("credit cap = %+v, want 98%%", w)
	}
	if w.exhausted() {
		t.Error("98%% must not park the account — only 100%% does")
	}
	if w.Resets == nil || w.Resets.Before(time.Now()) {
		t.Errorf("credit cap reset = %v, want now+reset_after_seconds", w.Resets)
	}
}

// The camelCase spelling of the window keys is a real vendor variant (OmniRoute
// reads both); a plan that answers in it must not render as "no windows".
func TestParseCodexCamelCaseWindows(t *testing.T) {
	body := []byte(`{"planType":"plus","rate_limit":{
		"primaryWindow":{"used_percent":33,"limitWindowSeconds":18000,"resetAfterSeconds":600},
		"secondaryWindow":{"used_percent":44,"limitWindowSeconds":604800,"resetAfterSeconds":500000}}}`)
	windows, plan, err := parseCodex(body, 200)
	if err != "" || plan != "plus" {
		t.Fatalf("camelCase payload: plan=%q err=%q", plan, err)
	}
	if len(windows) != 2 || windows[0].Used != 33 || windows[1].Used != 44 {
		t.Fatalf("camelCase windows = %+v", windows)
	}
}

// The quota probe needs the same workspace id the inference path sends, and
// it lives in the id_token. The tracker resolves both at poll time (tokens
// rotate in the background), so a resolver wired for the bearer only would
// probe with no workspace header.
func TestProbeCodexUsesResolvedIDToken(t *testing.T) {
	var seen struct {
		accountID string
		bearer    string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.accountID = r.Header.Get("chatgpt-account-id")
		seen.bearer = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":
			{"used_percent":7,"limit_window_seconds":18000,"reset_after_seconds":60}}}`))
	}))
	defer srv.Close()

	idTok := "h." + base64.RawURLEncoding.EncodeToString(
		[]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"ws-live","chatgpt_plan_type":"team"}}`)) + ".s"
	resolveKey := func(string, string) string { return "at-rotated" }
	resolveID := func(string, string) string { return idTok }

	tr := New([]Target{{Provider: "codex", AcctName: "main", AcctKey: "stale-key",
		Dialect: Codex, URL: srv.URL}}, nil, resolveKey, resolveID)
	defer tr.Stop()
	tr.poll()
	if seen.bearer != "Bearer at-rotated" {
		t.Errorf("Authorization = %q, want the RESOLVED (rotated) bearer", seen.bearer)
	}
	if seen.accountID != "ws-live" {
		t.Errorf("chatgpt-account-id = %q, want ws-live from the resolved id_token", seen.accountID)
	}
	snaps := tr.All()
	if len(snaps) != 1 || snaps[0].Plan != "team" {
		t.Fatalf("plan not read from the id_token claim: %+v", snaps)
	}
}
