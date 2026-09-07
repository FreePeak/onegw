package server

// Quota e2e (issue #7): enforcement cools an exhausted provider so combo
// fallback serves the request from the next target; /admin/quota exposes
// window status; state rebuilds from the store across a simulated restart.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"onegw/internal/config"
	"onegw/internal/quota"
	"onegw/internal/store"
	"onegw/internal/usage"
)

// countingUpstream answers every chat completion with a fixed model string
// and counts hits.
type countingUpstream struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newCountingUpstream(model string) *countingUpstream {
	c := &countingUpstream{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-q", "object": "chat.completion", "model": model,
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "pong from " + model}}},
			"usage": map[string]any{"prompt_tokens": 50, "completion_tokens": 10},
		})
	}))
	return c
}

// quotaCfg builds a two-provider config (primary + fallback in combo
// "pair") with a request limit on the primary.
func quotaCfg(t *testing.T, primary, fallback *countingUpstream, primaryLimit int64, dataDir string) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.DataDir = dataDir
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Providers = []config.ProviderCfg{
		{Name: "primary", Kind: "openai", BaseURL: primary.srv.URL, APIKey: "sk-test-up",
			Models: []string{"primary/m"}, QuotaWindow: "5h", QuotaLimitRequests: primaryLimit},
		{Name: "fallback", Kind: "openai", BaseURL: fallback.srv.URL, APIKey: "sk-test-up",
			Models: []string{"fallback/m"}},
	}
	cfg.Combos = []config.ComboCfg{{Name: "pair", Targets: []string{"primary/m", "fallback/m"}}}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	return cfg
}

// waitQuotaExhausted polls until the provider reports exhausted (the quota
// Observe happens after the response is written).
func waitQuotaExhausted(t *testing.T, srv *Server, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if q := srv.cur().quota; q != nil {
			if st, ok := q.Status(name, time.Now()); ok && st.Exhausted {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider %s never became exhausted", name)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// adminReq builds a password-authenticated admin request (header-only,
// constant-time check on master).
func adminReq(t *testing.T, target string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("X-Admin-Password", "admin")
	return r
}

func TestQuotaExhaustedFallsThroughCombo(t *testing.T) {
	primary := newCountingUpstream("m")
	defer primary.srv.Close()
	fallback := newCountingUpstream("m")
	defer fallback.srv.Close()

	cfg := quotaCfg(t, primary, fallback, 2, "memory")
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	// Two direct hits exhaust the primary's 2-request window.
	for i := 0; i < 2; i++ {
		w := do(t, h, authed(t, "primary/m", "sk-test-gw"))
		if w.Code != http.StatusOK {
			t.Fatalf("direct hit %d: status %d body %s", i, w.Code, w.Body.String())
		}
	}
	waitQuotaExhausted(t, srv, "primary")

	// A direct hit now gets 503 without reaching the upstream.
	before := primary.hits.Load()
	w := do(t, h, authed(t, "primary/m", "sk-test-gw"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("exhausted direct hit: want 503, got %d body %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("503 must carry Retry-After")
	}
	if !strings.Contains(w.Body.String(), "quota") {
		t.Fatalf("503 body should mention quota: %s", w.Body.String())
	}
	if primary.hits.Load() != before {
		t.Fatal("exhausted provider must not receive upstream traffic")
	}

	// The combo falls through to the fallback target.
	beforeFb := fallback.hits.Load()
	w = do(t, h, authed(t, "pair", "sk-test-gw"))
	if w.Code != http.StatusOK {
		t.Fatalf("combo request: status %d body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pong from m") {
		t.Fatalf("combo should be served: %s", w.Body.String())
	}
	if fallback.hits.Load() != beforeFb+1 {
		t.Fatalf("fallback should have served the combo request, hits=%d", fallback.hits.Load())
	}

	// /admin/quota shows the tracked provider with window metadata.
	w = do(t, h, adminReq(t, "/admin/quota"))
	if w.Code != http.StatusOK {
		t.Fatalf("admin quota: status %d", w.Code)
	}
	var resp struct {
		Providers []quota.Status `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("admin quota json: %v (%s)", err, w.Body.String())
	}
	if len(resp.Providers) != 1 {
		t.Fatalf("want 1 tracked provider (primary), got %d", len(resp.Providers))
	}
	st := resp.Providers[0]
	if st.Provider != "primary" || st.Window != "5h" || !st.Exhausted || st.UsedRequests != 2 || st.LimitRequests != 2 {
		t.Fatalf("primary quota status wrong: %+v", st)
	}
	if !st.WindowEnd.After(time.Now()) {
		t.Fatalf("window end should be in the future: %+v", st)
	}
}

func TestQuotaOffProvidersUntracked(t *testing.T) {
	primary := newCountingUpstream("m")
	defer primary.srv.Close()
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Providers = []config.ProviderCfg{
		{Name: "primary", Kind: "openai", BaseURL: primary.srv.URL, APIKey: "sk-test-up", Models: []string{"m"}},
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
	h := srv.Handler()
	w := do(t, h, authed(t, "m", "sk-test-gw"))
	if w.Code != http.StatusOK {
		t.Fatalf("request without quota: status %d", w.Code)
	}
	w = do(t, h, adminReq(t, "/admin/quota"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"providers":[]`) {
		t.Fatalf("admin quota with no windows: %d %s", w.Code, w.Body.String())
	}
}

// TestDashboardScriptBalanced guards the embedded dashboard JS against
// unbalanced try/catch or braces (a SyntaxError kills the whole page:
// refresh would be undefined and setInterval throws).
func TestDashboardScriptBalanced(t *testing.T) {
	start := strings.Index(dashboardHTML, "<script>")
	end := strings.Index(dashboardHTML, "</script>")
	if start < 0 || end < 0 || end < start {
		t.Fatal("dashboard <script> block not found")
	}
	js := dashboardHTML[start+len("<script>") : end]
	if n := strings.Count(js, "try {"); strings.Count(js, "} catch") != n {
		t.Fatalf("dashboard JS has %d try blocks but %d catch blocks", n, strings.Count(js, "} catch"))
	}
	depth := 0
	for _, r := range js {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
		}
		if depth < 0 {
			t.Fatal("dashboard JS closes more braces than it opens")
		}
	}
	if depth != 0 {
		t.Fatalf("dashboard JS braces unbalanced: depth %d at end", depth)
	}
}

// TestQuotaFallThroughNoStaleRetryAfter proves a cooled first target does
// not leak Retry-After onto the successful fallback response.
func TestQuotaFallThroughNoStaleRetryAfter(t *testing.T) {
	primary := newCountingUpstream("m")
	defer primary.srv.Close()
	fallback := newCountingUpstream("m")
	defer fallback.srv.Close()

	cfg := quotaCfg(t, primary, fallback, 1, "memory")
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	if w := do(t, h, authed(t, "primary/m", "sk-test-gw")); w.Code != http.StatusOK {
		t.Fatalf("first hit: %d %s", w.Code, w.Body.String())
	}
	waitQuotaExhausted(t, srv, "primary")

	w := do(t, h, authed(t, "pair", "sk-test-gw"))
	if w.Code != http.StatusOK {
		t.Fatalf("combo should succeed via fallback: %d %s", w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra != "" {
		t.Fatalf("successful response must not carry Retry-After, got %q", ra)
	}
	// A direct exhausted hit still must carry it.
	w = do(t, h, authed(t, "primary/m", "sk-test-gw"))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("direct exhausted hit: %d retry-after=%q", w.Code, w.Header().Get("Retry-After"))
	}
}

// TestQuotaRebuildAcrossServerRestart exercises the full persistence path:
// usage flush + quota flush to the store, close, reopen — the window
// counters must survive because the seed path reads the quota table.
func TestQuotaRebuildAcrossServerRestart(t *testing.T) {
	primary := newCountingUpstream("m")
	defer primary.srv.Close()
	fallback := newCountingUpstream("m")
	defer fallback.srv.Close()

	cfg := quotaCfg(t, primary, fallback, 2, t.TempDir())
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	h := srv.Handler()
	for i := 0; i < 2; i++ {
		if w := do(t, h, authed(t, "primary/m", "sk-test-gw")); w.Code != http.StatusOK {
			t.Fatalf("hit %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	waitQuotaExhausted(t, srv, "primary")
	srv.Close() // flushes usage + quota state to the store

	// "Restart": same data dir, fresh server. The persisted window is
	// still current (5h window, seconds later), so the provider comes back
	// up already exhausted.
	srv2, err := New(cfg)
	if err != nil {
		t.Fatalf("reopen server: %v", err)
	}
	defer srv2.Close()
	q := srv2.cur().quota
	if q == nil {
		t.Fatal("quota tracker missing after restart")
	}
	st, ok := q.Status("primary", time.Now())
	if !ok {
		t.Fatal("primary quota window lost across restart")
	}
	if st.UsedRequests != 2 {
		t.Fatalf("persisted used_requests = %d, want 2 (%+v)", st.UsedRequests, st)
	}
	if !st.Exhausted {
		t.Fatalf("primary should still be exhausted after restart: %+v", st)
	}
	// And enforcement still fires on the live surface.
	w := do(t, srv2.Handler(), authed(t, "primary/m", "sk-test-gw"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("enforcement after restart: want 503, got %d %s", w.Code, w.Body.String())
	}
}

// TestQuotaRebuildFromRollups seeds only usage rollups (the quota table has
// no row for the provider) and checks the seed path reconstructs the daily
// window from the rollup history.
func TestQuotaRebuildFromRollups(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/usage.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Now().UTC()
	err = st.FlushBuckets([]usage.Bucket{
		mkBucket("seeded", now, 700, 1),
		mkBucket("seeded", now.Add(-time.Hour), 500, 1),
		mkBucket("other", now, 99999, 1), // must not leak into "seeded"
	})
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	st.Close()

	cfg := &config.Config{}
	cfg.Server.DataDir = dir
	cfg.Auth.KeyList = []config.AuthKey{{Key: "sk-test-gw"}}
	cfg.Providers = []config.ProviderCfg{
		{Name: "seeded", Kind: "openai", BaseURL: "http://127.0.0.1:9", APIKey: "sk-test-up",
			QuotaWindow: "daily", QuotaLimitTokens: 1500},
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
	got, ok := srv.cur().quota.Status("seeded", now)
	if !ok {
		t.Fatal("seeded provider not tracked")
	}
	if got.UsedTokens != 1200 {
		t.Fatalf("rebuilt used tokens = %d, want 1200 (%+v)", got.UsedTokens, got)
	}
	if got.Exhausted {
		t.Fatal("1200 tokens under a 1500 limit must not exhaust")
	}
}

func mkBucket(provider string, at time.Time, tokens, reqs int64) usage.Bucket {
	return usage.Bucket{
		Key: usage.Key{Provider: provider, Model: "m", APIKey: "k",
			Day: at.Format("2006-01-02"), Hour: at.Format("15")},
		InputTokens: tokens, Requests: reqs,
		FirstSeen: at, LastSeen: at,
	}
}

// TestQuotaTrackOnlyServesNormally proves limits=0 never blocks even at
// volume, while still tracking counts.
func TestQuotaTrackOnlyServesNormally(t *testing.T) {
	primary := newCountingUpstream("m")
	defer primary.srv.Close()
	cfg := quotaCfg(t, primary, newCountingUpstream("m"), 0, "memory") // 0 = track only
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()
	for i := 0; i < 5; i++ {
		if w := do(t, h, authed(t, "primary/m", "sk-test-gw")); w.Code != http.StatusOK {
			t.Fatalf("track-only hit %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	waitQuotaObserved(t, srv, "primary", 5)
	if st, _ := srv.cur().quota.Status("primary", time.Now()); st.Exhausted {
		t.Fatalf("track-only must never exhaust: %+v", st)
	}
}

func waitQuotaObserved(t *testing.T, srv *Server, name string, wantReqs int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if st, ok := srv.cur().quota.Status(name, time.Now()); ok && st.UsedRequests >= wantReqs {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("quota never observed %d requests", wantReqs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
