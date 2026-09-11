package subquota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Parsers (shapes lifted from 9router's verified unit tests)
// ---------------------------------------------------------------------------

func TestParseOpenCodeGoHappy(t *testing.T) {
	body := []byte(`{"usage":{
		"rolling":{"status":"ok","percent":13,"resetsAt":"2026-09-04T14:28:02.617Z"},
		"weekly":{"status":"ok","percent":5,"resetsAt":"2026-09-07T00:00:00.617Z"},
		"monthly":{"status":"ok","percent":2,"resetsAt":"2026-10-02T12:14:24.617Z"}}}`)
	windows, err := parseOpenCodeGo(body, 200)
	if err != "" {
		t.Fatalf("unexpected error: %s", err)
	}
	if len(windows) != 3 {
		t.Fatalf("want 3 windows, got %d: %+v", len(windows), windows)
	}
	if windows[0].Name != "Rolling" || windows[0].Used != 13 {
		t.Fatalf("rolling = %+v", windows[0])
	}
	if windows[0].Resets == nil || windows[0].Resets.IsZero() {
		t.Fatal("rolling reset not parsed from ISO resetsAt")
	}
	if got := windows[0].Resets.UTC().Format(time.RFC3339); got != "2026-09-04T14:28:02Z" {
		t.Fatalf("rolling reset = %s", got)
	}
	if windows[1].Used != 5 || windows[2].Used != 2 {
		t.Fatalf("weekly/monthly = %+v %+v", windows[1], windows[2])
	}
}

func TestParseOpenCodeGoPercentAsString(t *testing.T) {
	windows, err := parseOpenCodeGo([]byte(`{"usage":{"rolling":{"percent":"13.4"}}}`), 200)
	if err != "" || len(windows) != 1 || windows[0].Used != 13 {
		t.Fatalf("string percent: windows=%+v err=%s", windows, err)
	}
}

func TestParseOpenCodeGoErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{401, `{}`, "authentication failed"},
		{403, `{"error":{"type":"EntitlementError"}}`, "subscription required"},
		{403, `{"error":"nope"}`, "forbidden"},
		{500, `{}`, "usage API error (500)"},
		{200, `{}`, "did not contain quota data"},
		{200, `{"usage":{}}`, "did not contain valid quota data"},
		{200, `{"usage":{"rolling":{"status":"ok"}}}`, "did not contain valid quota data"},
	}
	for _, tc := range cases {
		_, err := parseOpenCodeGo([]byte(tc.body), tc.status)
		if err == "" {
			t.Fatalf("status %d: want error containing %q, got none", tc.status, tc.want)
		}
	}
}

func TestParseZaiCreditLimits(t *testing.T) {
	body := []byte(`{"code":200,"msg":"Operation successful","data":{
		"limits":[
			{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":2000,"remaining":1999,"percentage":25,"nextResetTime":1787905548392},
			{"type":"CREDIT_LIMIT","unit":6,"number":1,"usage":10000,"remaining":9999,"percentage":10,"nextResetTime":1788492142997}
		],"level":"lite"},"success":true}`)
	windows, plan, err := parseZai(body, 200)
	if err != "" {
		t.Fatalf("unexpected error: %s", err)
	}
	if plan != "Lite" {
		t.Fatalf("plan = %q, want Lite", plan)
	}
	if len(windows) != 2 {
		t.Fatalf("want 2 windows, got %d", len(windows))
	}
	if windows[0].Name != "Session (5h)" || windows[0].Used != 25 {
		t.Fatalf("session window = %+v", windows[0])
	}
	if windows[0].Resets == nil || windows[0].Resets.UnixMilli() != 1787905548392 {
		t.Fatalf("session reset = %+v", windows[0].Resets)
	}
	if windows[1].Name != "Weekly (7d)" || windows[1].Used != 10 {
		t.Fatalf("weekly window = %+v", windows[1])
	}
}

func TestParseZaiTokensLimit(t *testing.T) {
	body := []byte(`{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","percentage":40,"nextResetTime":1787905548392}],"level":"standard"},"success":true}`)
	windows, plan, err := parseZai(body, 200)
	if err != "" || plan != "Standard" {
		t.Fatalf("windows=%+v plan=%q err=%s", windows, plan, err)
	}
	if len(windows) != 1 || windows[0].Name != "Tokens" || windows[0].Used != 40 {
		t.Fatalf("tokens window = %+v", windows)
	}
}

func TestParseZaiErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{401, `{}`, "invalid or expired"},
		{500, `{"error":"x"}`, "quota API error (500)"},
		{200, `{"code":401,"success":false}`, "code 401"},
		{200, `{"code":200,"data":{"limits":[]},"success":true}`, "did not contain valid limit data"},
		{200, `{"code":200,"data":{"limits":[{"type":"WEIRD","percentage":1}]},"success":true}`, "did not contain valid limit data"},
		{200, `{`, "not valid JSON"},
	}
	for _, tc := range cases {
		_, _, err := parseZai([]byte(tc.body), tc.status)
		if err == "" {
			t.Fatalf("status %d body %.40s: want error containing %q", tc.status, tc.body, tc.want)
		}
	}
}

func TestParseCommandCodeWindows(t *testing.T) {
	// Live-verified 2026-09-11 (GOAT plan, weekly window exhausted).
	body := []byte(`{"credits":{"belowThreshold":false,"creditThreshold":0,
		"monthlyCredits":10.2868762875,"purchasedCredits":0,"freeCredits":0},
		"windowLimits":{"limited":true,"exceeded":"weekly",
		"fiveHour":{"used":0,"cap":14,"exceeded":false,"resetAt":0},
		"weekly":{"used":35.0018639599,"cap":35,"exceeded":true,"resetAt":1789539876848}}}`)
	windows, plan, err := parseCommandCode(body, 200)
	if err != "" {
		t.Fatalf("unexpected error: %s", err)
	}
	if plan != "Command Code" {
		t.Fatalf("plan = %q, want default label", plan)
	}
	if len(windows) != 3 {
		t.Fatalf("want 3 windows, got %d: %+v", len(windows), windows)
	}
	if windows[0].Name != "5-hour window" || windows[0].Used != 0 {
		t.Fatalf("5-hour window = %+v", windows[0])
	}
	if windows[0].Resets != nil {
		t.Fatal("epoch-zero resetAt must produce no reset instant")
	}
	if windows[1].Name != "Weekly window" || windows[1].Used != 100 {
		t.Fatalf("weekly window = %+v (used 35.0018/cap 35 must park at 100%%)", windows[1])
	}
	if windows[1].Resets == nil || windows[1].Resets.UnixMilli() != 1789539876848 {
		t.Fatalf("weekly reset = %+v", windows[1].Resets)
	}
	if windows[2].Name != creditsWindow || windows[2].Used != 0 {
		t.Fatalf("credits window = %+v (healthy pool must read 0%%, not invented)", windows[2])
	}
}

func TestParseCommandCodeDrainedCreditsAndShapeGuards(t *testing.T) {
	// All three pools zero: nothing left to bill — parks.
	drained, _, err := parseCommandCode([]byte(
		`{"credits":{"monthlyCredits":0,"purchasedCredits":0,"freeCredits":0},
		"windowLimits":{"fiveHour":{"used":1,"cap":14}}}`), 200)
	if err != "" {
		t.Fatalf("drained pool errored: %s", err)
	}
	found := false
	for _, w := range drained {
		if w.Name == creditsWindow {
			found, w.Used = true, w.Used
			if w.Used != 100 {
				t.Fatalf("drained credits window = %+v, want 100%%", w)
			}
		}
	}
	if !found {
		t.Fatal("drained credits pool must surface a credits window")
	}
	// Absent credits object must NOT fake a drained pool (shape change
	// must fail open, never park a serving account).
	noCredits, _, err := parseCommandCode([]byte(
		`{"windowLimits":{"fiveHour":{"used":1,"cap":14}}}`), 200)
	if err != "" || len(noCredits) != 1 {
		t.Fatalf("absent credits = %+v err=%s, want single window, no error", noCredits, err)
	}
	// Unusable shape errors.
	for _, tc := range []struct {
		status int
		body   string
	}{
		{401, `{}`}, {403, `{}`}, {500, `{}`},
		{200, `{`},
		{200, `{"windowLimits":null,"credits":null}`},
	} {
		if _, _, err := parseCommandCode([]byte(tc.body), tc.status); err == "" {
			t.Fatalf("status %d body %.30s: want error", tc.status, tc.body)
		}
	}
}

func TestParseCommandCodeFloorsNearCap(t *testing.T) {
	// 34.9/35 = 99.71%: spendable headroom remains. Rounding (int(pct+0.5))
	// read this as 100 and re-parked the account every poll cycle — the
	// window is exhausted only when used >= cap.
	windows, _, err := parseCommandCode([]byte(
		`{"credits":{"monthlyCredits":5},"windowLimits":{"weekly":{"used":34.9,"cap":35}}}`), 200)
	if err != "" {
		t.Fatalf("unexpected error: %s", err)
	}
	if windows[0].Name != "Weekly window" || windows[0].Used != 99 {
		t.Fatalf("near-cap window = %+v, want floored 99 (not rounded 100)", windows[0])
	}
	if _, ok := (Snapshot{Windows: windows}).exhaustedWindow(); ok {
		t.Fatal("99.71% used must not park")
	}
}

func TestCommandCodePlanLabel(t *testing.T) {
	for id, want := range map[string]string{
		"individual-goat":     "Command Code · GOAT",
		"individual-pro":      "Command Code · Pro",
		"team-pro":            "Command Code · Team Pro",
		"individual-ultra-5x": "Command Code · Ultra 5x",
		"weird-plan":          "", // unrecognized: caller keeps the default
		"":                    "",
	} {
		if got := commandCodePlanLabel(id); got != want {
			t.Fatalf("plan %q = %q, want %q", id, got, want)
		}
	}
}

func TestExhaustedWindowPicksWorst(t *testing.T) {
	soon := time.Now().Add(time.Hour)
	snap := Snapshot{Windows: []Window{
		{Name: "Rolling", Used: 99, Resets: &soon},
		{Name: "Weekly", Used: 100},
	}}
	w, ok := snap.exhaustedWindow()
	if !ok || w.Name != "Weekly" || !w.exhausted() {
		t.Fatalf("want Weekly exhausted, got %+v ok=%v", w, ok)
	}
	if _, ok := (Snapshot{Windows: []Window{{Used: 99}}}).exhaustedWindow(); ok {
		t.Fatal("99% must not count as exhausted")
	}
}

// ---------------------------------------------------------------------------
// Tracker loop, caching and parking
// ---------------------------------------------------------------------------

func TestTrackerPollCachesAndParks(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	probes := func(ctx context.Context, tr *Tracker, tgt Target) Snapshot {
		mu.Lock()
		calls[tgt.AcctName]++
		mu.Unlock()
		reset := time.Now().Add(30 * time.Minute)
		switch tgt.AcctName {
		case "full":
			return Snapshot{Provider: tgt.Provider, Account: tgt.AcctName, Dialect: tgt.Dialect,
				Windows: []Window{{Name: "Rolling", Used: 100, Resets: &reset}}}
		case "ok":
			return Snapshot{Provider: tgt.Provider, Account: tgt.AcctName, Dialect: tgt.Dialect,
				Windows: []Window{{Name: "Rolling", Used: 13}}}
		default:
			return Snapshot{Provider: tgt.Provider, Account: tgt.AcctName, Dialect: tgt.Dialect, Err: "boom"}
		}
	}

	var parkedMu sync.Mutex
	parked := map[string]time.Time{}
	targets := []Target{
		{Provider: "oc", AcctName: "full", AcctKey: "k1", Dialect: OpenCodeGo},
		{Provider: "oc", AcctName: "ok", AcctKey: "k2", Dialect: OpenCodeGo},
		{Provider: "oc", AcctName: "bad", AcctKey: "k3", Dialect: OpenCodeGo},
	}
	tr := NewAt(targets, func(tgt Target, until time.Time) {
		parkedMu.Lock()
		parked[tgt.AcctName] = until
		parkedMu.Unlock()
	}, probes, nil, 50*time.Millisecond, nil, nil)
	defer tr.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := tr.All(); len(got) == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	all := tr.All()
	if len(all) != 3 {
		t.Fatalf("want 3 snapshots, got %d", len(all))
	}
	if all[0].Account != "bad" || all[0].Err == "" {
		t.Fatalf("failed probe must keep Err: %+v", all[0])
	}
	if all[1].Account != "full" || len(all[1].Windows) != 1 || all[1].Windows[0].Used != 100 {
		t.Fatalf("full snapshot: %+v", all[1])
	}
	if all[2].Account != "ok" || all[2].Err != "" {
		t.Fatalf("ok snapshot: %+v", all[2])
	}

	parkedMu.Lock()
	defer parkedMu.Unlock()
	if _, ok := parked["full"]; !ok {
		t.Fatal("exhausted account was not parked")
	}
	if d := time.Until(parked["full"]); d <= 0 || d > pollEvery {
		t.Fatalf("park until must be capped at one poll cycle, got %v", d)
	}
	if _, ok := parked["ok"]; ok {
		t.Fatal("non-exhausted account must not park")
	}
	if _, ok := parked["bad"]; ok {
		t.Fatal("failed probe must not park")
	}
}

func TestTrackerInheritKeepsFreshTargetsOnly(t *testing.T) {
	old := NewAt([]Target{{Provider: "oc", AcctName: "k1", AcctKey: "x", Dialect: OpenCodeGo}}, nil,
		func(ctx context.Context, tr *Tracker, tgt Target) Snapshot {
			return Snapshot{Provider: tgt.Provider, Account: tgt.AcctName, Windows: []Window{{Name: "Rolling", Used: 7}}}
		}, nil, time.Hour, nil, nil)
	defer old.Stop()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(old.All()) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	fresh := NewAt([]Target{{Provider: "oc", AcctName: "k1", AcctKey: "x", Dialect: OpenCodeGo},
		{Provider: "oc", AcctName: "k2", AcctKey: "y", Dialect: OpenCodeGo}}, nil, nil, nil, time.Hour, nil, nil)
	defer fresh.Stop()
	fresh.Inherit(old)
	// k2 has no inherited snapshot; it will appear only after its own probe
	// (never run in this test — probe fn is nil and the real HTTP probe would
	// fail open with Err). Only k1 must be present with inherited data.
	all := fresh.All()
	if len(all) != 1 || all[0].Account != "k1" || len(all[0].Windows) != 1 || all[0].Windows[0].Used != 7 {
		t.Fatalf("inherit must carry k1's snapshot only, got %+v", all)
	}
}

func TestProbeHTTPOpensErrorAndDialects(t *testing.T) {
	// probeHTTP is exercised through the real HTTP path only indirectly in
	// the server e2e; here verify the fail-open snapshot on a dead URL.
	tr := NewAt([]Target{{Provider: "x", AcctName: "a", AcctKey: "k", Dialect: OpenCodeGo, URL: "http://127.0.0.1:1/usage"}},
		nil, nil, nil, time.Hour, nil, nil)
	defer tr.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if all := tr.All(); len(all) == 1 && all[0].Err != "" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("dead URL probe never recorded an error")
}

func TestDefaultURLPerDialect(t *testing.T) {
	if DefaultURL(OpenCodeGo) != "https://opencode.ai/zen/go/v1/usage" {
		t.Fatal("opencode-go default URL drifted from the 9router-verified endpoint")
	}
	if DefaultURL(Zai) != "https://api.z.ai/api/monitor/usage/quota/limit" {
		t.Fatal("zai default URL drifted")
	}
	if DefaultURL(ZaiCN) != "https://open.bigmodel.cn/api/monitor/usage/quota/limit" {
		t.Fatal("zai-cn default URL drifted")
	}
	if DefaultURL(CommandCode) != "https://api.commandcode.ai" {
		t.Fatal("commandcode default URL drifted from the OmniRoute-verified base")
	}
	if DefaultURL("nope") != "" || ValidDialect("nope") {
		t.Fatal("unknown dialect must have no URL and be invalid")
	}
}

func TestProbeCommandCodeEndToEnd(t *testing.T) {
	// Full probe against a stub vendor: whoami grants no org, credits
	// carries an exhausted weekly window, subscriptions labels the plan
	// and stamps the credits reset. URL override = API base.
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-cc" {
			t.Errorf("commandcode probe auth = %q", got)
		}
		switch r.URL.Path {
		case "/alpha/whoami":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "org": nil})
		case "/alpha/billing/credits":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"credits": map[string]any{"monthlyCredits": 10.28, "purchasedCredits": 0, "freeCredits": 0},
				"windowLimits": map[string]any{
					"limited": true, "exceeded": "weekly",
					"fiveHour": map[string]any{"used": 0, "cap": 14, "exceeded": false, "resetAt": 0},
					"weekly":   map[string]any{"used": 35.0018, "cap": 35, "exceeded": true, "resetAt": 1789539876848},
				},
			})
		case "/alpha/billing/subscriptions":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"planId": "individual-goat", "currentPeriodEnd": "2026-10-02T02:03:39.000Z",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cc.Close()

	tr := NewAt([]Target{{Provider: "cc", AcctName: "linh", AcctKey: "sk-cc",
		Dialect: CommandCode, URL: cc.URL}}, nil, nil, nil, time.Hour, nil, nil)
	defer tr.Stop()
	deadline := time.Now().Add(2 * time.Second)
	var snaps []Snapshot
	for time.Now().Before(deadline) {
		if snaps = tr.All(); len(snaps) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(snaps) != 1 {
		t.Fatalf("snapshot never settled: %+v", snaps)
	}
	s := snaps[0]
	if s.Err != "" {
		t.Fatalf("probe error: %s", s.Err)
	}
	if s.Plan != "Command Code · GOAT" {
		t.Fatalf("plan = %q, want GOAT label", s.Plan)
	}
	var credits *Window
	for i := range s.Windows {
		if s.Windows[i].Name == creditsWindow {
			credits = &s.Windows[i]
		}
		if s.Windows[i].Name == "Weekly window" && s.Windows[i].Used != 100 {
			t.Fatalf("weekly = %+v, want exhausted", s.Windows[i])
		}
	}
	if credits == nil {
		t.Fatalf("credits window missing: %+v", s.Windows)
	}
	if credits.Resets == nil || credits.Resets.UTC().Format(time.RFC3339) != "2026-10-02T02:03:39Z" {
		t.Fatalf("credits reset = %+v, want billing period end", credits.Resets)
	}
	// And the exhausted weekly window must park via the normal hook.
	parked := make(chan struct{}, 1)
	tr2 := NewAt([]Target{{Provider: "cc", AcctName: "linh", AcctKey: "sk-cc",
		Dialect: CommandCode, URL: cc.URL}}, func(Target, time.Time) { parked <- struct{}{} },
		nil, nil, time.Hour, nil, nil)
	defer tr2.Stop()
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("exhausted weekly window never parked the account")
	}
}

func TestProbeCommandCodeSoftCallsFailOpen(t *testing.T) {
	// whoami and subscriptions are enrichment: a 500 on either must NOT
	// mark the snapshot failed (parkIfExhausted early-returns on Err, so
	// a leak would silently stop parking while the credits call is fine).
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alpha/whoami", "/alpha/billing/subscriptions":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/alpha/billing/credits":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"credits": map[string]any{"monthlyCredits": 10.28, "purchasedCredits": 0, "freeCredits": 0},
				"windowLimits": map[string]any{
					"weekly": map[string]any{"used": 35.0018, "cap": 35, "exceeded": true, "resetAt": 1789539876848},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cc.Close()

	parked := make(chan struct{}, 1)
	tr := NewAt([]Target{{Provider: "cc", AcctName: "harvey", AcctKey: "sk-cc",
		Dialect: CommandCode, URL: cc.URL}}, func(Target, time.Time) { parked <- struct{}{} },
		nil, nil, time.Hour, nil, nil)
	defer tr.Stop()
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("exhausted weekly window never parked despite failing enrichment calls")
	}
	for _, s := range tr.All() {
		if s.Err != "" {
			t.Fatalf("soft-call failure must not fail the snapshot: %s", s.Err)
		}
	}
}
func TestTrackerResolvesLiveKey(t *testing.T) {
	// A rotated/rotating bearer must reach the probe AND invalidate the
	// cache entry: same (provider, account), fresh key -> fresh snapshot
	// row, never the old key's windows.
	var mu sync.Mutex
	keysSeen := []string{}
	rotate := "key-A"
	probe := func(ctx context.Context, tr *Tracker, tgt Target) Snapshot {
		mu.Lock()
		keysSeen = append(keysSeen, tgt.AcctKey)
		mu.Unlock()
		return Snapshot{Provider: tgt.Provider, Account: tgt.AcctName, Windows: []Window{{Name: "Rolling", Used: 1}}}
	}
	resolver := func(provider, acct string) string {
		mu.Lock()
		defer mu.Unlock()
		return rotate
	}
	tr := NewAt([]Target{{Provider: "oc", AcctName: "k1", AcctKey: "stale", Dialect: OpenCodeGo}},
		nil, probe, resolver, 30*time.Millisecond, nil, nil)
	defer tr.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(keysSeen)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	first := append([]string(nil), keysSeen...)
	mu.Unlock()
	if len(first) == 0 || first[0] != "key-A" {
		t.Fatalf("probe must receive the resolved live key, saw %v", first)
	}

	mu.Lock()
	rotate = "key-B"
	mu.Unlock()
	// Wait for a probe under the new key, then assert a single settled row.
	sawB := false
	deadline = time.Now().Add(2 * time.Second)
	for !sawB && time.Now().Before(deadline) {
		mu.Lock()
		for _, k := range keysSeen {
			if k == "key-B" {
				sawB = true
			}
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	if !sawB {
		t.Fatal("rotation never reached the probe")
	}
	for {
		got := tr.All()
		if len(got) == 1 {
			break
		}
		if len(got) > 1 {
			t.Fatalf("rotated key must replace the old cache entry, got %d rows", len(got))
		}
		if time.Now().After(deadline) {
			t.Fatal("tracker never settled on one row")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
