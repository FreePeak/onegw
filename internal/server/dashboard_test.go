package server

// Tests for the dashboard revamp (#41/#45/#19): cookie session auth, SSE
// hub, request-log ring, grouped API, and server-rendered pages.

import (
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"onegw/internal/server/dashboard"
	"onegw/internal/types"
	"onegw/internal/usage"
	"time"
)

// adminTOML is the fixture base; tests append/patch server bits.
const adminTOML = `
listen = "127.0.0.1:0"

[server]
admin_password = "admin"
data_dir = "memory"

[auth]
keys = ["sk-test-gw"]

[[providers]]
name = "p1"
kind = "openai"
base_url = "http://127.0.0.1:1"
api_key = "sk-test-upstream-secret"
models = ["m1", "m2"]

[[combo]]
name = "c1"
targets = ["p1/m1", "p1/m2"]
`

// newAdminSrv boots a server from adminTOML plus extra TOML appended.
// The variant with a real store points data_dir at a temp dir.
func newAdminSrv(t *testing.T, extra string) (*Server, http.Handler) {
	t.Helper()
	srv, h, _ := newTestServerFromFile(t, adminTOML+extra)
	return srv, h
}

func newAdminSrvWithStore(t *testing.T, extra string) (*Server, http.Handler) {
	t.Helper()
	tomlText := strings.Replace(adminTOML+extra, `data_dir = "memory"`, "data_dir = \""+t.TempDir()+"\"", 1)
	srv, h, _ := newTestServerFromFile(t, tomlText)
	return srv, h
}

// loginForm posts the password and returns the session cookie (or "" on
// a failed login with the recorded status).
func loginForm(t *testing.T, h http.Handler, password string) (*http.Cookie, int) {
	t.Helper()
	form := url.Values{"password": {password}}
	r := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c, w.Code
		}
	}
	return nil, w.Code
}

func cookieReq(method, target string, c *http.Cookie) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	if c != nil {
		r.AddCookie(c)
	}
	return r
}

func TestLoginSetsCookieAndGrantsAccess(t *testing.T) {
	_, h := newAdminSrv(t, "")

	c, code := loginForm(t, h, "admin")
	if code != http.StatusSeeOther || c == nil {
		t.Fatalf("login: code %d cookie %v", code, c)
	}
	// Header path still works.
	w := do(t, h, adminReq(t, "/admin/health"))
	if w.Code != http.StatusOK {
		t.Fatalf("header auth broken: %d", w.Code)
	}
	// Cookie path works.
	w = do(t, h, cookieReq(http.MethodGet, "/admin/health", c))
	if w.Code != http.StatusOK {
		t.Fatalf("cookie auth: %d %s", w.Code, w.Body.String())
	}
	// Wrong cookie rejected.
	w = do(t, h, cookieReq(http.MethodGet, "/admin/health", &http.Cookie{Name: sessionCookie, Value: "nope"}))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad cookie accepted: %d", w.Code)
	}
}

func TestLoginRejectsWrongPasswordAndRateLimits(t *testing.T) {
	_, h := newAdminSrv(t, "")
	for i := range maxLoginFails {
		c, code := loginForm(t, h, "wrong")
		if c != nil || code != http.StatusOK {
			t.Fatalf("attempt %d: expected login page, got cookie=%v code=%d", i, c, code)
		}
	}
	// The 6th attempt is blocked regardless of correctness.
	c, code := loginForm(t, h, "admin")
	if c != nil {
		t.Fatalf("blocked IP still got a session cookie")
	}
	if code != http.StatusOK {
		t.Fatalf("blocked login status %d", code)
	}
}

func TestPasswordChangeInvalidatesSessions(t *testing.T) {
	srv, _ := newAdminSrv(t, "")
	tok := srv.sessions.issue("admin")
	if !srv.sessions.valid(tok, "admin") {
		t.Fatal("fresh token invalid")
	}
	if srv.sessions.valid(tok, "changed") {
		t.Fatal("token valid under a different password")
	}
	if srv.sessions.valid("bogus", "admin") {
		t.Fatal("bogus token valid")
	}
}

func TestLogoutDropsSession(t *testing.T) {
	srv, h := newAdminSrv(t, "")
	_ = srv
	c, code := loginForm(t, h, "admin")
	if c == nil || code != http.StatusSeeOther {
		t.Fatal("login failed")
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
	r.AddCookie(c)
	w := do(t, h, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", w.Code)
	}
	if srv.sessions.count() != 0 || srv == nil {
		t.Fatalf("session survived logout: %d", srv.sessions.count())
	}
}

func TestSSEHubBoundedFanout(t *testing.T) {
	srv, _ := newAdminSrv(t, "")
	hub := srv.events
	subs := make([]*sseSub, 0, maxSSESubs+8)
	for range maxSSESubs + 8 {
		subs = append(subs, hub.subscribe([]string{"health"}))
	}
	if got := func() int {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.subs)
	}(); got != maxSSESubs {
		t.Fatalf("hub grew past cap: %d", got)
	}
	// Publishing to a stalled subscriber drops it instead of blocking.
	stalled := hub.subscribe([]string{"logs"})
	for range sseRingFrames * 2 {
		hub.publish("logs", "{}")
	}
	select {
	case <-stalled.dead:
	default:
		t.Fatal("stalled subscriber not dropped")
	}
	for _, s := range subs {
		hub.unsubscribe(s)
	}
	hub.unsubscribe(stalled)
}

func TestSSEStreamServesHealth(t *testing.T) {
	srv, h := newAdminSrv(t, "")
	_ = srv
	r := adminReq(t, "/admin/events?topics=health")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(w, r)
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	select {
	case <-done:
		t.Fatal("SSE stream returned before client disconnect")
	case <-deadline:
	}
	// The stream has flushed the retry preamble + first health frame.
	body := w.Body.String()
	if !strings.Contains(body, "retry: 3000") || !strings.Contains(body, "event: health") {
		t.Fatalf("SSE preamble missing: %q", body)
	}
	r2 := adminReq(t, "/admin/events?topics=health")
	r2 = r2.Clone(r2.Context())
	srv.events.shutdown()
}

func TestRequestLogRingAndAPI(t *testing.T) {
	srv, h := newAdminSrv(t, "")
	_ = srv
	now := time.Now().Unix()
	for i := range logRingCap + 10 {
		srv.reqlog.record(logEntry{TS: now - logRingCap + int64(i), Model: "m1", Provider: "p1", Code: 200, In: int64(i)})
	}
	entries := srv.reqlog.latest(5)
	if entries[0].Seq != logRingCap+5+1 {
		t.Fatalf("ring order wrong: oldest of 5 = %d, want %d", entries[0].Seq, logRingCap+6)
	}
	if entries[4].Seq != logRingCap+10 {
		t.Fatalf("newest seq = %d, want %d", entries[4].Seq, logRingCap+10)
	}
	w := do(t, h, adminReq(t, "/admin/api/v1/logs?limit=3"))
	var got struct {
		Entries []logEntry `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 3 || got.Entries[2].Seq != logRingCap+10 {
		t.Fatalf("api logs: %+v", got.Entries)
	}
}

// Entries older than 7 days are auto-cleared: latest() never serves them
// (the ring bounds memory; the age window bounds the view). An entry just
// inside the window must survive.
func TestRequestLogAgeClear(t *testing.T) {
	srv, _ := newAdminSrv(t, "")
	now := time.Now().Unix()
	srv.reqlog.record(logEntry{TS: now - 8*24*3600, Model: "stale"})
	srv.reqlog.record(logEntry{TS: now, Model: "fresh"})
	entries := srv.reqlog.latest(10)
	if len(entries) != 1 || entries[0].Model != "fresh" {
		t.Fatalf("stale entry not cleared: %+v", entries)
	}
	srv.reqlog.record(logEntry{TS: now - 7*24*3600 + 3600, Model: "edge"})
	entries = srv.reqlog.latest(10)
	if len(entries) != 2 || entries[0].Model != "fresh" || entries[1].Model != "edge" {
		t.Fatalf("in-window entry dropped: %+v", entries)
	}
}

func TestObserveLogClassifiesKinds(t *testing.T) {
	srv, _ := newAdminSrv(t, "")
	srv.observeLog("p1", "m1", "acct-1", 200, "", typesUsage(3, 9), 7, "", 0, 0, 25, 12.5)
	srv.observeLog("", "", "", 503, "budget_saturated", types.Usage{}, 0, "", 0, 0, 0, 0)
	entries := srv.reqlog.latest(2)
	if entries[0].Code != 200 || entries[0].Out != 9 || entries[0].Saved != 7 || entries[0].Account != "acct-1" {
		t.Fatalf("ok entry wrong: %+v", entries[0])
	}
	if entries[0].E2EMs != 25 || entries[0].DTps != 12.5 {
		t.Fatalf("delivery fields missing from ok entry: %+v", entries[0])
	}
	if entries[1].Kind != "budget_saturated" {
		t.Fatalf("kind wrong: %+v", entries[1])
	}
}

func typesUsage(in, out int64) types.Usage {
	return types.Usage{InputTokens: in, OutputTokens: out}
}

func TestDashboardPagesRender(t *testing.T) {
	srv, h := newAdminSrv(t, "")
	_ = srv
	pages := map[string]string{
		"/admin":              "Requests · today",
		"/admin/ui/usage":     "All time",
		"/admin/ui/providers": "p1",
		"/admin/ui/combos":    "c1",
		"/admin/ui/quota":     "no quota windows",
		"/admin/ui/saver":     "Input saver",
		"/admin/ui/logs":      "logstat",
		"/admin/ui/tools":     "model_providers.onegw",
		"/admin/ui/settings":  "admin auth",
	}
	for path, want := range pages {
		w := do(t, h, adminReq(t, path))
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", path, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: missing %q in:\n%s", path, want, w.Body.String())
		}
	}
	// Unauthenticated browser gets the login page, not a 401 page.
	w := do(t, h, cookieReq(http.MethodGet, "/admin", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "sign in") {
		t.Fatalf("login page: %d %s", w.Code, w.Body.String())
	}
	_ = srv
}

func TestAPIEndpointsShape(t *testing.T) {
	_, h := newAdminSrv(t, "")
	w := do(t, h, adminReq(t, "/admin/api/v1/providers"))
	var provs []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &provs); err != nil || len(provs) != 1 || provs[0]["name"] != "p1" {
		t.Fatalf("providers: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-test") {
		t.Fatal("provider API leaked an api key")
	}
	w = do(t, h, adminReq(t, "/admin/api/v1/combos"))
	var combos []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &combos); err != nil || len(combos) != 1 || combos[0]["name"] != "c1" {
		t.Fatalf("combos: %s", w.Body.String())
	}
	w = do(t, h, adminReq(t, "/admin/api/v1/quota"))
	if !strings.Contains(w.Body.String(), "[]") {
		t.Fatalf("quota: %s", w.Body.String())
	}
	w = do(t, h, adminReq(t, "/admin/api/v1/saver"))
	if !strings.Contains(w.Body.String(), "saved_tokens_all_time") {
		t.Fatalf("saver: %s", w.Body.String())
	}
	// Unauthenticated JSON error shape.
	w = do(t, h, httptest.NewRequest(http.MethodGet, "/admin/api/v1/providers", nil))
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("unauth shape: %d %s", w.Code, w.Body.String())
	}
}

func TestUsageDailyCursorAPI(t *testing.T) {
	srv, h := newAdminSrvWithStore(t, "")
	_ = srv
	// Seed two days into the store.
	rows := []usage.Bucket{
		{Key: usage.Key{Day: "2000-01-01", Hour: "00", Provider: "p1", Model: "m1", APIKey: "k"}, Requests: 2, InputTokens: 10, OutputTokens: 20},
		{Key: usage.Key{Day: "2000-01-02", Hour: "00", Provider: "p1", Model: "m1", APIKey: "k"}, Requests: 3, InputTokens: 30, OutputTokens: 40},
	}
	if err := srv.st.FlushBuckets(rows); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, adminReq(t, "/admin/api/v1/usage/daily?from=2000-01-01&to=2000-01-02"))
	var page struct {
		Days       []string         `json:"days"`
		Rows       []map[string]any `json:"rows"`
		NextCursor any              `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Days) != 2 || len(page.Rows) != 2 || page.NextCursor != nil {
		t.Fatalf("daily page: %+v", page)
	}
	// Cursor past the last day → empty page.
	w = do(t, h, adminReq(t, "/admin/api/v1/usage/daily?from=2000-01-01&to=2000-01-02&cursor=2000-01-02"))
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Days) != 0 {
		t.Fatalf("cursor page not empty: %+v", page)
	}
}

func TestUsagePageAggregates(t *testing.T) {
	srv, h := newAdminSrvWithStore(t, "")
	_ = srv
	// Seed through instants on the LOCAL today: the dashboard window is
	// the local calendar day and the UTC key follows the instant.
	d1, h1 := bucketKey(time.Now().In(time.Local).Truncate(time.Hour).Add(-2 * time.Hour))
	d2, h2 := bucketKey(time.Now().In(time.Local).Truncate(time.Hour).Add(-1 * time.Hour))
	rows := []usage.Bucket{
		{Key: usage.Key{Day: d1, Hour: h1, Provider: "p1", Model: "m1", APIKey: "k"}, Requests: 2, InputTokens: 10, OutputTokens: 20, SavedTokens: 5},
		{Key: usage.Key{Day: d2, Hour: h2, Provider: "p1", Model: "m2", APIKey: "k"}, Requests: 1, InputTokens: 7, OutputTokens: 3},
	}
	if err := srv.st.FlushBuckets(rows); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, adminReq(t, "/admin/ui/usage?range=today"))
	body := w.Body.String()
	if !strings.Contains(body, "17") || !strings.Contains(body, "23") {
		t.Fatalf("usage page missing today's aggregates:\n%s", body)
	}
	if !strings.Contains(body, "per hour (local)") {
		t.Fatal("today page missing local hourly chart unit")
	}
	w = do(t, h, adminReq(t, "/admin/ui/usage?range=7d"))
	if !strings.Contains(w.Body.String(), "chart-tok") {
		t.Fatal("7d page missing charts")
	}
	_ = srv
}

func TestChartJSONShape(t *testing.T) {
	srv, _ := newAdminSrv(t, "")
	from := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	to := time.Now().Format("2006-01-02")
	js := srv.chartJSON(from, to)
	var cd struct {
		Days     []string `json:"days"`
		Requests []int64  `json:"requests"`
	}
	if err := json.Unmarshal([]byte(js), &cd); err != nil {
		t.Fatalf("chart json: %v %s", err, js)
	}
	if len(cd.Days) != 3 || len(cd.Requests) != 3 {
		t.Fatalf("dense axis: %d days", len(cd.Days))
	}
}

func TestRootRedirectsToAdmin(t *testing.T) {
	_, h := newAdminSrv(t, "")
	w := do(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin" {
		t.Fatalf("root: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestBaseTemplateWithVendoredAssets(t *testing.T) {
	for _, name := range []string{"htmx.min.js", "sse.min.js", "uPlot.iife.min.js", "uPlot.min.css", "admin.css"} {
		if _, ok := dashboard.Asset(name); !ok {
			t.Errorf("asset missing: %s", name)
		}
	}
	out, err := dashboard.Render("login", dashboard.Shell{V: dashboard.LoginView{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "--bg:") {
		// login is standalone: it must carry the inlined stylesheet.
		t.Error("login page missing inlined stylesheet")
	}
	// compact ladder
	if dashboard.Compact(1500) != "1.5K" || dashboard.Compact(2_500_000) != "2.5M" || dashboard.Compact(3_100_000_000) != "3.1B" {
		t.Errorf("compact ladder broken: %s %s %s", dashboard.Compact(1500), dashboard.Compact(2_500_000), dashboard.Compact(3_100_000_000))
	}
}

// todayUTC is the raw UTC day key — retention and other UTC-keyed paths.
func todayUTC() string { return time.Now().UTC().Format("2006-01-02") }

// bucketKey maps an instant to the (day, hour) rollup key the store uses.
// Seeding through instants keeps tests correct in every timezone: the
// local-window display paths re-bucket by the instant a key represents.
func bucketKey(t time.Time) (string, string) {
	return t.UTC().Format("2006-01-02"), t.UTC().Format("15")
}

func TestOverviewQuotaExhaustedBadge(t *testing.T) {
	srv, h := newAdminSrv(t, "")
	_ = srv
	// no quota configured → zero badge; renders fine
	w := do(t, h, adminReq(t, "/admin"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hx-ext=\"sse\"") {
		t.Fatalf("overview: %d (missing live SSE wiring)", w.Code)
	}
	_ = srv
}

// TestCompactLadder pins the unit-preserving compact formatter: a whole
// 1.0B must render "1B", never "1" (the #45 regression where units were
// dropped on ".0" values made token columns look broken across all
// usage-page filters).
func TestCompactLadder(t *testing.T) {
	cases := map[int64]string{
		0:             "0",
		999:           "999",
		1_000:         "1K",
		26_000:        "26K",
		8_800:         "8.8K",
		1_000_000:     "1M",
		2_500_000:     "2.5M",
		59_000_000:    "59M",
		170_300_000:   "170.3M",
		947_000_000:   "947M",
		1_000_000_000: "1B",
		1_002_000_000: "1B",
		1_500_000_000: "1.5B",
		1_029_555_855: "1B",
		-2_500_000:    "-2.5M",
	}
	for n, want := range cases {
		if got := dashboard.Compact(n); got != want {
			t.Errorf("Compact(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestTodayChartHourly pins the today view: HasCharts on with a dense
// 24-hour LOCAL axis (labels + epoch x values), values summed per local
// hour of the local day.
func TestTodayChartHourly(t *testing.T) {
	srv, h := newAdminSrvWithStore(t, "")
	now := time.Now()
	localNoon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	mk := func(delta time.Duration, model string, req, in int64) usage.Bucket {
		d, hr := bucketKey(localNoon.Add(delta))
		return usage.Bucket{Key: usage.Key{Day: d, Hour: hr, Provider: "p1", Model: model, APIKey: "k"}, Requests: req, InputTokens: in}
	}
	rows := []usage.Bucket{
		mk(-7*time.Hour, "m1", 4, 100), // local ~05:00
		mk(-3*time.Hour, "m2", 6, 200), // local ~09:00
	}
	if err := srv.st.FlushBuckets(rows); err != nil {
		t.Fatal(err)
	}
	today := now.Format("2006-01-02")
	var cd struct {
		Days     []string `json:"days"`
		Xs       []int64  `json:"xs"`
		Requests []int64  `json:"requests"`
		Input    []int64  `json:"input"`
	}
	if err := json.Unmarshal([]byte(srv.chartJSON(today, today)), &cd); err != nil {
		t.Fatal(err)
	}
	if len(cd.Days) != 24 || len(cd.Xs) != 24 {
		t.Fatalf("hour axis: %d labels, %d x", len(cd.Days), len(cd.Xs))
	}
	lh1 := localNoon.Add(-7 * time.Hour).Format("15") // "05"
	lh2 := localNoon.Add(-3 * time.Hour).Format("15") // "09"
	i1, i2 := 0, 0
	for i, l := range cd.Days {
		if l == lh1 {
			i1 = i
		}
		if l == lh2 {
			i2 = i
		}
	}
	if cd.Days[i1] != lh1 || cd.Requests[i1] != 4 || cd.Input[i1] != 100 {
		t.Fatalf("local hour %s wrong: %+v", lh1, cd)
	}
	if cd.Requests[i2] != 6 || cd.Input[i2] != 200 {
		t.Fatalf("local hour %s wrong: %+v", lh2, cd)
	}
	if cd.Xs[i2]-cd.Xs[i1] != 4*3600 {
		t.Fatalf("x spacing wrong: %d", cd.Xs[i2]-cd.Xs[i1])
	}
	// The page itself renders charts for today.
	w := do(t, h, adminReq(t, "/admin/ui/usage?range=today"))
	if !strings.Contains(w.Body.String(), "chart-tok") {
		t.Fatal("today page missing charts")
	}
	if !strings.Contains(w.Body.String(), "per hour (local)") {
		t.Fatal("today page missing local hourly chart unit")
	}
}

// TestUsageTodayExcludesYesterday pins the LOCAL-day filter: usage from
// 26+ local hours ago (previous local day, whatever UTC keys it lands on)
// must not appear in today's page.
func TestUsageTodayExcludesYesterday(t *testing.T) {
	srv, h := newAdminSrvWithStore(t, "")
	now := time.Now()
	localNoon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	td, th := bucketKey(localNoon)
	yd, yh := bucketKey(localNoon.AddDate(0, 0, -1))
	rows := []usage.Bucket{
		{Key: usage.Key{Day: td, Hour: th, Provider: "p1", Model: "m1", APIKey: "k"}, Requests: 1, InputTokens: 1_234_567},
		{Key: usage.Key{Day: yd, Hour: yh, Provider: "p1", Model: "yesterday-only-model", APIKey: "k"}, Requests: 9, InputTokens: 9_876_543},
	}
	if err := srv.st.FlushBuckets(rows); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, adminReq(t, "/admin/ui/usage?range=today"))
	body := w.Body.String()
	if !strings.Contains(body, "1.2M") {
		t.Fatal("today's distinctive input missing")
	}
	if strings.Contains(body, "yesterday-only-model") || strings.Contains(body, "9.9M") {
		t.Fatal("today filter leaked yesterday's rows")
	}
	_ = srv
}

// TestUsageTodaySpansUTCDayBoundary pins the local-day window: a local
// the today page must aggregate both (pre-fix, one edge silently dropped
// because the query used the UTC "today" key). Runs correctly in any TZ.
func TestUsageTodaySpansUTCDayBoundary(t *testing.T) {
	srv, h := newAdminSrvWithStore(t, "")
	now := time.Now()
	localMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	localEnd := localMidnight.AddDate(0, 0, 1)
	dA, hA := bucketKey(localMidnight)             // first hour of the local day
	dB, hB := bucketKey(localEnd.Add(-time.Hour)) // last hour of the local day
	rows := []usage.Bucket{
		{Key: usage.Key{Day: dA, Hour: hA, Provider: "p1", Model: "edge-start", APIKey: "k"}, Requests: 1, InputTokens: 1_234_567},
		{Key: usage.Key{Day: dB, Hour: hB, Provider: "p1", Model: "edge-end", APIKey: "k"}, Requests: 2, InputTokens: 9_876_543},
	}
	if err := srv.st.FlushBuckets(rows); err != nil {
		t.Fatal(err)
	}
	today := now.Format("2006-01-02")
	w := do(t, h, adminReq(t, "/admin/ui/usage?range=today"))
	body := w.Body.String()
	for _, want := range []string{"edge-start", "edge-end", "1.2M", "9.9M"} {
		if !strings.Contains(body, want) {
			t.Fatalf("today page dropped a local-day edge (%s):\n%s", want, body)
		}
	}
	var cd struct {
		Requests []int64 `json:"requests"`
	}
	if err := json.Unmarshal([]byte(srv.chartJSON(today, today)), &cd); err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, v := range cd.Requests {
		sum += v
	}
	if sum != 3 {
		t.Fatalf("hourly chart lost a boundary bucket: sum=%d (keys %s/%sh + %s/%sh)", sum, dA, hA, dB, hB)
	}
	_ = srv
}

// TestChartDayModeLocalBuckets pins day-mode bucketing: a rollup keyed by
// a late UTC hour that lands on the NEXT local day must appear under that
// local day's column (pre-fix, slot() dropped the hour and bucketed every
// row by its UTC day's local midnight, shifting late rows a day early).
// The seed hour is chosen from the zone offset so the discriminating row
// exists in EVERY timezone: for offset +o, the local day that starts at
// UTC (24-o) hours lands its midnight inside UTC day D but its hours run
// into D+1; a row at UTC hour (24-o-1)%24 of day D maps to local D+1.
func TestChartDayModeLocalBuckets(t *testing.T) {
	srv, _ := newAdminSrvWithStore(t, "")
	_, off := time.Now().Zone()
	offH := int(off / 3600) // whole-hour offsets only (no half-hour TZ here)
	now := time.Now().Truncate(time.Hour)
	// Row A: 00:00 local today — UTC hour (24-offH)%24 of some day D.
	// Under the buggy midnight rule it buckets by D; correctly it belongs
	// to the local day whose local date != D's local view.
	a := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	b := a.AddDate(0, 0, -1) // 00:00 local yesterday
	da, ha := bucketKey(a)
	db, hb := bucketKey(b)
	rows := []usage.Bucket{
		{Key: usage.Key{Day: da, Hour: ha, Provider: "p1", Model: "m1", APIKey: "k"}, Requests: 1, InputTokens: 1_111_111},
		{Key: usage.Key{Day: db, Hour: hb, Provider: "p1", Model: "m2", APIKey: "k"}, Requests: 2, InputTokens: 2_222_222},
	}
	if err := srv.st.FlushBuckets(rows); err != nil {
		t.Fatal(err)
	}
	from := a.AddDate(0, 0, -1).Format("2006-01-02")
	to := now.Format("2006-01-02")
	var cd struct {
		Days     []string `json:"days"`
		Requests []int64  `json:"requests"`
	}
	if err := json.Unmarshal([]byte(srv.chartJSON(from, to)), &cd); err != nil {
		t.Fatal(err)
	}
	wantA := a.Format("2006-01-02")
	wantB := b.Format("2006-01-02")
	var gotA, gotB int64 = -1, -1
	for i, d := range cd.Days {
		if d == wantA {
			gotA = cd.Requests[i]
		}
		if d == wantB {
			gotB = cd.Requests[i]
		}
	}
	if gotA != 1 || gotB != 2 {
		t.Fatalf("day-mode local bucketing wrong: %s=%d, %s=%d (axis %v, off %dh)", wantA, gotA, wantB, gotB, cd.Days, offH)
	}
}

// TestRetentionPrunesOldRollups pins the previously-dead retention path:
// store.Prune + [usage].retention_days existed but nothing scheduled it.
func TestRetentionPrunesOldRollups(t *testing.T) {
	srv, _ := newAdminSrvWithStore(t, "")
	today := todayUTC()
	rows := []usage.Bucket{
		{Key: usage.Key{Day: "2000-01-01", Hour: "00", Provider: "p1", Model: "ancient", APIKey: "k"}, Requests: 5},
		{Key: usage.Key{Day: today, Hour: "00", Provider: "p1", Model: "m1", APIKey: "k"}, Requests: 2},
	}
	if err := srv.st.FlushBuckets(rows); err != nil {
		t.Fatal(err)
	}
	n, err := srv.pruneOnce()
	if err != nil || n == 0 {
		t.Fatalf("pruneOnce: %d rows, err %v", n, err)
	}
	left, err := srv.st.QueryRange("2000-01-01", today)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range left {
		if r.Model == "ancient" {
			t.Fatal("retention window did not delete the ancient row")
		}
	}
	if len(left) == 0 {
		t.Fatal("prune deleted today's rows too")
	}
}
