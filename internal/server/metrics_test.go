package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// metricsUpstreamStub answers with an OpenAI completion carrying usage
// counters so token metrics can be asserted deterministically.
func metricsUpstreamStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-metrics", "object": "chat.completion", "model": "m1",
			"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "pong"},
			}},
		})
	}))
}

// metricsFailingStub answers every request with a plain-text HTTP error
// status (no retryable statuses allowed in by the caller).
func metricsFailingStub(status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "kaput", status)
	}))
}

// parseMetrics extracts sample values keyed "name|label=value,...".
func parseMetrics(body string) map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rest, _ := strings.Cut(line, " ")
		val, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			continue
		}
		out[name] = val
	}
	return out
}

// metricsLineRegexp: every non-comment line is name{labels} int.
var metricsLineRegexp = regexp.MustCompile(
	`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{[a-zA-Z_][a-zA-Z0-9_]*="[^"]*"(,[a-zA-Z_][a-zA-Z0-9_]*="[^"]*")*\})? -?[0-9]+$`)

func assertMetricsBodyWellFormed(t *testing.T, body string) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !metricsLineRegexp.MatchString(line) {
			t.Fatalf("malformed exposition line: %q", line)
		}
	}
}

func TestMetricsEndpointFreshServer(t *testing.T) {
	up := upstreamStub("m1")
	defer up.Close()
	cfg := makeCfg(t, "key-metrics", "pw", false, providerSpec{name: "p1", up: up.URL, model: "m1"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	w := do(t, h, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics: got %d, want 200 (endpoint must be open, no auth)", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("content type: got %q", ct)
	}
	body := w.Body.String()
	assertMetricsBodyWellFormed(t, body)
	for _, name := range []string{
		"onegw_requests_total", "onegw_tokens_total", "onegw_request_errors_total",
		"onegw_inflight", "onegw_budget_inflight_bytes", "onegw_budget_cap_bytes",
		"onegw_uptime_seconds",
	} {
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Fatalf("missing family %s in:\n%s", name, body)
		}
	}
	// A fresh server must show no completed requests.
	if strings.Contains(body, "onegw_requests_total{") {
		t.Fatalf("unexpected request series on fresh server:\n%s", body)
	}
	// Budget gauges reflect the default config.
	vals := parseMetrics(body)
	if got := vals["onegw_budget_cap_bytes"]; got != cfg.Server.BufferCap {
		t.Fatalf("budget cap gauge: got %d, want %d", got, cfg.Server.BufferCap)
	}
	if got := vals["onegw_budget_inflight_bytes"]; got != 0 {
		t.Fatalf("budget held gauge: got %d, want 0", got)
	}
	if got := vals["onegw_inflight"]; got != 0 {
		t.Fatalf("inflight gauge: got %d, want 0", got)
	}
	if got := vals["onegw_uptime_seconds"]; got < 0 {
		t.Fatalf("uptime gauge negative: %d", got)
	}
}

func TestMetricsCountersThroughRequestCycle(t *testing.T) {
	up := metricsUpstreamStub(t)
	defer up.Close()
	bad := metricsFailingStub(400) // non-retryable upstream error
	defer bad.Close()

	cfg := makeCfg(t, "key-metrics", "pw", false,
		providerSpec{name: "p1", up: up.URL, model: "m1"},
		providerSpec{name: "p2", up: bad.URL, model: "m2"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	auth := func(r *http.Request) *http.Request {
		r.Header.Set("Authorization", "Bearer key-metrics")
		return r
	}

	// 1. Successful request through p1.
	if w := do(t, h, auth(chatReq(t, "p1/m1"))); w.Code != 200 {
		t.Fatalf("success request: got %d, body %s", w.Code, w.Body.String())
	}
	// 2. Unroutable model → 404 no_route.
	if w := do(t, h, auth(chatReq(t, "nosuchprovider/m"))); w.Code != 404 {
		t.Fatalf("no-route request: got %d, want 404", w.Code)
	}
	// 3. Upstream 400 through p2 → upstream_error.
	if w := do(t, h, auth(chatReq(t, "p2/m2"))); w.Code != 400 {
		t.Fatalf("upstream error request: got %d, want 400", w.Code)
	}

	w := do(t, h, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("scrape: got %d", w.Code)
	}
	body := w.Body.String()
	assertMetricsBodyWellFormed(t, body)
	vals := parseMetrics(body)

	check := func(name string, want int64) {
		t.Helper()
		if got := vals[name]; got != want {
			t.Fatalf("%s: got %d, want %d\n%s", name, got, want, body)
		}
	}
	check(`onegw_requests_total{code="200",model="m1",provider="p1"}`, 1)
	check(`onegw_requests_total{code="404",model="unresolved",provider=""}`, 1)
	check(`onegw_requests_total{code="400",model="m2",provider="p2"}`, 1)
	check(`onegw_request_errors_total{kind="no_route",provider=""}`, 1)
	check(`onegw_request_errors_total{kind="upstream_error",provider="p2"}`, 1)

	// Token counters come from the upstream usage block (11 in / 7 out).
	check(`onegw_tokens_total{model="m1",provider="p1",type="input"}`, 11)
	check(`onegw_tokens_total{model="m1",provider="p1",type="output"}`, 7)
	// Request without usage: p2's error request never completes usage.
	if _, ok := vals[`onegw_tokens_total{model="m2",provider="p2",type="input"}`]; ok {
		t.Fatalf("failed request must not record tokens:\n%s", body)
	}
	// The scrape must not leak the configured upstream or gateway keys.
	if strings.Contains(body, "up-key") || strings.Contains(body, "key-metrics") {
		t.Fatalf("metrics leaked credentials:\n%s", body)
	}
}

func TestMetricsGaugesReflectInflightAndBudget(t *testing.T) {
	release := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer blocked.Close()
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()

	cfg := makeCfg(t, "key-metrics", "pw", false, providerSpec{name: "p1", up: blocked.URL, model: "m1"})
	cfg.Server.BufferCap = 4096 // tiny: the request's 4×body+64KiB margin clamps to the full budget
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	r := chatReq(t, "p1/m1")
	r.Header.Set("Authorization", "Bearer key-metrics")
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), r)
		close(done)
	}()

	// Wait until request 1 holds the whole budget (margin > cap → clamp).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if held, _ := srv.cur().budget.Stats(); held == 4096 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request 1 never acquired the budget")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// While the budget is held: inflight 1, held == cap.
	w := do(t, h, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	vals := parseMetrics(w.Body.String())
	if vals["onegw_inflight"] != 1 {
		t.Fatalf("inflight gauge: got %d, want 1\n%s", vals["onegw_inflight"], w.Body.String())
	}
	if vals["onegw_budget_inflight_bytes"] != 4096 {
		t.Fatalf("budget held gauge: got %d, want 4096", vals["onegw_budget_inflight_bytes"])
	}

	// A second request with a cancelled context hits the saturated path
	// (Acquire fails → rejectSaturated 503).
	r2 := chatReq(t, "p1/m1")
	r2.Header.Set("Authorization", "Bearer key-metrics")
	ctx, cancel := context.WithCancel(context.Background())
	r2 = r2.WithContext(ctx)
	done2 := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), r2)
		close(done2)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("saturated request never returned")
	}

	w2 := do(t, h, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	vals2 := parseMetrics(w2.Body.String())
	if got := vals2[`onegw_request_errors_total{kind="budget_saturated",provider=""}`]; got != 1 {
		t.Fatalf("budget_saturated: got %d, want 1\n%s", got, w2.Body.String())
	}
	if got := vals2[`onegw_requests_total{code="503",model="",provider=""}`]; got != 1 {
		t.Fatalf("saturated request count: got %d, want 1\n%s", got, w2.Body.String())
	}

	closeRelease()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked request never finished")
	}

	// After both finish: inflight back to 0, budget released.
	w3 := do(t, h, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	vals3 := parseMetrics(w3.Body.String())
	if vals3["onegw_inflight"] != 0 || vals3["onegw_budget_inflight_bytes"] != 0 {
		t.Fatalf("gauges did not return to baseline: inflight=%d held=%d",
			vals3["onegw_inflight"], vals3["onegw_budget_inflight_bytes"])
	}
	if got := vals3[`onegw_requests_total{code="200",model="m1",provider="p1"}`]; got != 1 {
		t.Fatalf("blocked request did not complete: got %d, want 1\n%s", got, w3.Body.String())
	}
}

func TestMetricsConcurrentRequestsAndScrapes(t *testing.T) {
	up := upstreamStub("m1")
	defer up.Close()
	cfg := makeCfg(t, "key-metrics", "pw", false, providerSpec{name: "p1", up: up.URL, model: "m1"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Add(2)
		go func() {
			defer wg.Done()
			r := chatReq(t, "p1/m1")
			r.Header.Set("Authorization", "Bearer key-metrics")
			if w := do(t, h, r); w.Code != 200 {
				t.Errorf("request: got %d", w.Code)
			}
		}()
		go func() {
			defer wg.Done()
			if w := do(t, h, httptest.NewRequest(http.MethodGet, "/metrics", nil)); w.Code != 200 {
				t.Errorf("scrape: got %d", w.Code)
			}
		}()
	}
	wg.Wait()

	w := do(t, h, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	vals := parseMetrics(w.Body.String())
	if got := vals[`onegw_requests_total{code="200",model="m1",provider="p1"}`]; got != n {
		t.Fatalf("concurrent count: got %d, want %d", got, n)
	}
	if vals["onegw_inflight"] != 0 {
		t.Fatalf("inflight after drain: got %d, want 0", vals["onegw_inflight"])
	}
}

// Unroutable model strings must not become metric label values: series are
// never evicted, so raw client strings would grow the exposition unboundedly.
func TestMetricsModelLabelCardinalityBounded(t *testing.T) {
	up := upstreamStub("m1")
	defer up.Close()
	cfg := makeCfg(t, "key-metrics", "pw", false, providerSpec{name: "p1", up: up.URL, model: "m1"})
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	h := srv.Handler()

	// Distinct garbage model strings, none of which resolve.
	for i := range 5 {
		w := do(t, h, func() *http.Request {
			r := chatReq(t, fmt.Sprintf("junk-model-%d/ghost-%d", i, i))
			r.Header.Set("Authorization", "Bearer key-metrics")
			return r
		}())
		if w.Code != 404 {
			t.Fatalf("junk request %d: got %d, want 404", i, w.Code)
		}
	}
	// A routable bare model (advertised by p1) must keep its real name.
	if w := do(t, h, func() *http.Request {
		r := chatReq(t, "m1")
		r.Header.Set("Authorization", "Bearer key-metrics")
		return r
	}()); w.Code != 200 {
		t.Fatalf("bare-model request: got %d, body %s", w.Code, w.Body.String())
	}

	body := do(t, h, httptest.NewRequest(http.MethodGet, "/metrics", nil)).Body.String()
	for i := range 5 {
		if strings.Contains(body, fmt.Sprintf("junk-model-%d", i)) {
			t.Fatalf("raw client model string leaked into metrics:\n%s", body)
		}
	}
	vals := parseMetrics(body)
	if got := vals[`onegw_requests_total{code="404",model="unresolved",provider=""}`]; got != 5 {
		t.Fatalf("unresolved series: got %d, want 5\n%s", got, body)
	}
	if got := vals[`onegw_requests_total{code="200",model="m1",provider="p1"}`]; got != 1 {
		t.Fatalf("routed bare model must keep its name: got %d, want 1\n%s", got, body)
	}
}
