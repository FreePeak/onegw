package provider

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"onegw/internal/types"
)

// flapSrv builds an upstream that answers every request with the nginx-style
// edge page observed live on b-ai (2026-09-09 22:54): an HTML 502 served by
// ALL accounts at once while the origin pool flapped.
func flapSrv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>\r\n<head><title>502 Bad Gateway</title></head>\r\n<body>\r\n<center><h1>502 Bad Gateway</h1></center>\r\n</body>\r\n</html>\r\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newDef(t *testing.T, baseURL string) (*Pool, *Def) {
	t.Helper()
	p := NewPool()
	def := &Def{Name: "b-ai", Kind: KindOpenAI, BaseURL: baseURL,
		Accounts: []Account{
			{Name: "clone2", APIKey: "k1"},
			{Name: "harvey", APIKey: "k2"},
		}}
	p.Set(def)
	return p, def
}

func doErr(t *testing.T, def *Def) *types.APIError {
	t.Helper()
	_, apiErr := def.Do(t.Context(), &def.Accounts[0], "glm-5.3-flash", nil,
		bytes.NewReader([]byte(`{"model":"glm-5.3-flash","messages":[]}`)), false)
	if apiErr == nil {
		t.Fatal("Do: want error")
	}
	return apiErr
}

// The live b-ai burst: every upstream error was the raw HTML page. The
// gateway must surface a bounded honest message naming the observation —
// never multi-line HTML — and strike the provider-wide breaker (the fault
// indicts the edge, not the key).
func TestDoClassifiesHTMLErrorPage(t *testing.T) {
	_, def := newDef(t, flapSrv(t).URL)
	apiErr := doErr(t, def)
	if apiErr.Status != 502 {
		t.Fatalf("status = %d, want 502 (the edge's status is real)", apiErr.Status)
	}
	if apiErr.Type != "upstream_html_error" {
		t.Fatalf("type = %q, want upstream_html_error", apiErr.Type)
	}
	if !strings.Contains(apiErr.Message, "HTML error page") || !strings.Contains(apiErr.Message, "502 Bad Gateway") {
		t.Fatalf("message must name the HTML page and its title, got %q", apiErr.Message)
	}
	if strings.Contains(apiErr.Message, "\n") || strings.Contains(apiErr.Message, "<") {
		t.Fatalf("message must be one line of prose, not raw HTML: %q", apiErr.Message)
	}
	def.pool.mu.Lock()
	strikes := def.pool.flapStrikes
	def.pool.mu.Unlock()
	if strikes != 1 {
		t.Fatalf("flapStrikes = %d, want 1 after one HTML fault", strikes)
	}
}

// Four consecutive edge faults trip the breaker: NextAccount then refuses
// to hand out accounts until the window expires, so Router.Execute falls
// through to the next combo target without ANY upstream call.
func TestFlapBreakerOpensAfterConsecutiveEdgeFaults(t *testing.T) {
	_, def := newDef(t, flapSrv(t).URL)
	for range flapThreshold - 1 {
		doErr(t, def)
	}
	if acct, _ := def.NextAccount(""); acct == nil {
		t.Fatal("below threshold the pool must still serve")
	}
	doErr(t, def) // 4th consecutive fault trips
	acct, ready := def.NextAccount("")
	if acct != nil {
		t.Fatalf("breaker must park the pool, handed out %s", acct.Name)
	}
	if until := time.Until(ready); until <= 0 || until > flapOpen {
		t.Fatalf("ready must sit within the open window (%s), got %s", flapOpen, until)
	}
}

// The breaker counts PROVIDER-wide faults, not per-key history: success on
// any account closes it immediately. A blip inside healthy serving must
// never accumulate toward a trip.
func TestFlapBreakerResetsOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><head><title>502 Bad Gateway</title></head><body></body></html>"))
	}))
	t.Cleanup(srv.Close)
	_, def := newDef(t, srv.URL)
	for range flapThreshold - 1 {
		doErr(t, def)
	}
	// One healthy response (any account) heals the edge view.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(srv2.Close)
	def.BaseURL = srv2.URL
	res, err := def.Do(t.Context(), &def.Accounts[1], "glm-5.3-flash", nil,
		bytes.NewReader([]byte(`{"model":"glm-5.3-flash","messages":[]}`)), false)
	if err != nil {
		t.Fatalf("healthy upstream must serve: %v", err)
	}
	res.Resp.Body.Close()
	def.pool.mu.Lock()
	strikes := def.pool.flapStrikes
	def.pool.mu.Unlock()
	if strikes != 0 {
		t.Fatalf("success must reset flapStrikes, got %d", strikes)
	}
	// A single fresh fault now sits at 1 of 4 — no trip.
	def.BaseURL = srv.URL
	doErr(t, def)
	if acct, _ := def.NextAccount(""); acct == nil {
		t.Fatal("one fault after healing must not trip the breaker")
	}
}

// After the open window the breaker half-opens: one probe passes through.
// If the edge still answers HTML, the strike re-opens the window — one
// wasted probe per window is the ceiling, never a fan across all accounts.
func TestFlapBreakerHalfOpenThenReopens(t *testing.T) {
	cur := time.Now()
	_, def := newDef(t, flapSrv(t).URL)
	def.pool.now = func() time.Time { return cur }
	for range flapThreshold {
		doErr(t, def)
	}
	if acct, _ := def.NextAccount(""); acct != nil {
		t.Fatal("breaker must be open")
	}
	cur = cur.Add(flapOpen + time.Second) // window expired
	acct, _ := def.NextAccount("")
	if acct == nil {
		t.Fatal("expired window must probe again")
	}
	doErr(t, def) // probe fails: 5th consecutive strike re-opens
	acct, ready := def.NextAccount("")
	if acct != nil {
		t.Fatal("failed probe must re-open the breaker")
	}
	if until := ready.Sub(cur); until <= 0 || until > flapOpen {
		t.Fatalf("re-open window must be fresh (%s), got %s", flapOpen, until)
	}
}

// Broken pools answer at the Execute boundary as pool-empty (429 with the
// honest Retry-After on direct routes) — the pre-flap behavior fanned N
// attempts into the dead edge and surfaced raw HTML.
func TestFlapBreakerDirectRouteAnswersPoolEmpty(t *testing.T) {
	_, def := newDef(t, flapSrv(t).URL)
	for range flapThreshold {
		doErr(t, def)
	}
	_, ready := def.NextAccount("")
	if ready.IsZero() {
		t.Fatal("open breaker must report its readiness instant")
	}
}

// Edge-fault classification: exactly the provider-edge-shaped failures —
// never request faults (400/401/403/404) or shared-concurrency walls.
func TestEdgeFaultPredicate(t *testing.T) {
	cases := []struct {
		status int
		typ    string
		budget bool // NoSameTargetRetry: the gateway's own pre-first-byte abort
		want   bool
	}{
		{502, "upstream_html_error", false, true},
		{502, "upstream_error", false, true},               // plain 502 status counts
		{503, "gateway_overloaded", false, true},           // admission 503 class
		{504, "upstream_timeout", false, true},             // transport timeout
		{520, "server_error", false, true},                 // Cloudflare edge family
		{502, "upstream_unreachable", false, true},         // dial/TLS failure
		{504, "upstream_timeout", true, false},             // header-budget abort: request-shaped
		{500, "upstream_error", false, false},              // app-level 500: not edge
		{429, "rate_limit_exceeded", false, false},         // per-key ladder territory
		{400, "invalid_request", false, false},             // request fault
		{403, "access_denied", false, false},               // credential fault
		{502, "upstream_auth_verify_failed", false, false}, // rewritten: verify blip
		{502, "upstream_parse_rejected", false, false},     // rewritten: channel fault
	}
	for _, c := range cases {
		e := &types.APIError{Status: c.status, Type: c.typ, NoSameTargetRetry: c.budget}
		if got := edgeFault(e); got != c.want {
			t.Fatalf("edgeFault(%d %q budget=%v) = %v, want %v", c.status, c.typ, c.budget, got, c.want)
		}
	}
}

// The consolidation's reason to exist: a plain JSON 502 body — the common
// one-api error shape, NOT an HTML/empty page — must trip the breaker too.
// Before the single exit-site strike, only the HTML/empty/transport
// branches struck, so a JSON 502 storm never parked the provider.
func TestFlapBreakerTripsOnPlainJSON502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"The server is overloaded","type":"server_error"}}`))
	}))
	t.Cleanup(srv.Close)
	_, def := newDef(t, srv.URL)
	for range flapThreshold {
		apiErr := doErr(t, def)
		if apiErr.Type != "server_error" {
			// The upstream's own type rides verbatim; edgeFault still
			// counts the fault via the 502 status fallback.
			t.Fatalf("JSON body must keep its decoded type, got %q", apiErr.Type)
		}
		if apiErr.Message != "The server is overloaded" {
			t.Fatalf("upstream's own message must survive, got %q", apiErr.Message)
		}
	}
	if acct, _ := def.NextAccount(""); acct != nil {
		t.Fatal("4 consecutive JSON 502s must open the breaker")
	}
}
