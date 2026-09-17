// Package subquota tracks UPSTREAM-reported subscription quota (issue #79),
// ported from 9router's open-sse/services/usage/{opencode-go,glm}.js and
// OmniRoute's opencodeQuotaFetcher.ts.
//
// It is orthogonal to internal/quota (which counts what THIS gateway spent
// against locally configured limits): subquota asks the vendor how much of
// the subscription itself is left — OpenCode Go's rolling 5h/weekly/monthly
// percentages and the z.ai GLM Coding Plan's session (5h)/weekly credit or
// tokens limits and CommandCode's USD billing windows — per provider
// account, with the vendor's own reset times.
//
// A background loop probes every target on a fixed 60s cadence. Probes are
// fail-open: a failed request keeps the last snapshot, records the error,
// and stops re-parking, so a vendor outage can never bench accounts that
// are still serving. The server wires an onExhausted hook that parks the
// account until the vendor's reset (capped at one poll cycle so a recovered
// subscription self-heals), making combos fall through to the next account
// instead of burning doomed upstream attempts (9router's auth pre-filter /
// OmniRoute's quota preflight).
package subquota

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Dialect names for providers.subscription_quota (config.go).
const (
	OpenCodeGo  = "opencode-go" // OpenCode Zen Go subscription
	Zai         = "zai"         // z.ai GLM Coding Plan (international)
	ZaiCN       = "zai-cn"      // GLM Coding Plan (China, bigmodel.cn)
	CommandCode = "commandcode" // CommandCode /alpha billing (GOAT/Go/Pro plans)
	GrokCli     = "grok-cli"    // SuperGrok shared weekly pool (cli-chat-proxy)
	Cursor      = "cursor"      // Cursor subscription (cursor.com session API)
	Freebuff    = "freebuff"    // Codebuff/freebuff free tier (codebuff.com session API)
	AgentRouter = "agentrouter" // AgentRouter (New-API) console wallet balance
)

// Dialects lists the accepted providers.subscription_quota values. It is the
// single source of that list: config.Validate matches against it and quotes
// it in its error, so a new dialect is registered in exactly one place.
func Dialects() []string {
	return []string{OpenCodeGo, Zai, ZaiCN, CommandCode, GrokCli, Cursor, Freebuff, AgentRouter}
}

// ValidDialect reports whether name is a subscription quota dialect.
func ValidDialect(name string) bool {
	for _, d := range Dialects() {
		if d == name {
			return true
		}
	}
	return false
}

// DefaultURL returns the dialect's documented usage endpoint.
func DefaultURL(dialect string) string {
	switch dialect {
	case OpenCodeGo:
		// 9router-verified live endpoint. OmniRoute's /v1/quota variant
		// 404s; /v1/usage is the real subscription usage surface.
		return "https://opencode.ai/zen/go/v1/usage"
	case Zai:
		return "https://api.z.ai/api/monitor/usage/quota/limit"
	case ZaiCN:
		return "https://open.bigmodel.cn/api/monitor/usage/quota/limit"
	case CommandCode:
		// OmniRoute's command-code.ts API base. For this dialect the URL
		// override replaces the BASE (the probe appends its own /alpha/
		// ... paths), not one full endpoint.
		return "https://api.commandcode.ai"
	case GrokCli:
		// OmniRoute's grokQuotaFetcher: the SuperGrok shared weekly pool.
		// Same base the Grok Build CLI uses for everything else.
		return "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	case Cursor:
		// Browser-dashboard usage-summary API — the whole meter state in one
		// call, no query params (the session cookie identifies the account).
		return "https://cursor.com/api/usage-summary"
	case Freebuff:
		// OmniRoute's validateFreebuffProvider / freebuff executor: the
		// free-tier session endpoint both the CLI and the web app hit.
		return "https://www.codebuff.com/api/v1/freebuff/session"
	case AgentRouter:
		// New-API's own console balance API (NOT the routing /v1 surface):
		// the provider is built on QuantumNous/new-api, whose /api/user/self
		// reports the console account's wallet quota.
		return "https://agentrouter.org/api/user/self"
	}
	return ""
}

// Window is one subscription quota window as reported by the vendor.
type Window struct {
	Name   string     `json:"name"`
	Used   int        `json:"used"`      // percent of the window consumed, 0-100
	Resets *time.Time `json:"resets_at"` // nil = vendor did not say
}

// exhausted reports whether this window is fully consumed.
func (w Window) exhausted() bool { return w.Used >= 100 }

// Target is one polled (provider, account) subscription probe. AcctKey is
// the credential sent as the bearer token; AcctName identifies the account
// in reports. ConsoleUser is the New-API console user id, used only by the
// agentrouter dialect (its console API authenticates with a bearer token AND
// that id). URL overrides the dialect default (config subscription_url).
type Target struct {
	Provider    string
	AcctName    string
	AcctKey     string
	Dialect     string
	ConsoleUser string
	URL         string // "" = DefaultURL(dialect)
}

// Snapshot is one account's last observed upstream subscription state.
type Snapshot struct {
	Provider  string    `json:"provider"`
	Account   string    `json:"account"`
	Plan      string    `json:"plan,omitempty"` // "OpenCode Go" / "Lite" / "Standard" / ...
	Dialect   string    `json:"dialect"`
	URL       string    `json:"url"`
	Windows   []Window  `json:"windows,omitempty"`
	Err       string    `json:"err,omitempty"` // last probe failure (fail-open)
	FetchedAt time.Time `json:"fetched_at"`    // when the probe ran (success or not)
}

// exhaustedWindow returns the worst (highest-used) exhausted window, or
// ok=false when every window still has headroom (or nothing was fetched).
func (s Snapshot) exhaustedWindow() (Window, bool) {
	worst := Window{Used: -1}
	for _, w := range s.Windows {
		if w.exhausted() && w.Used > worst.Used {
			worst = w
		}
	}
	return worst, worst.Used >= 0
}

// pollEvery is the fixed probe cadence (both 9router and OmniRoute use a
// 60s TTL for subscription usage).
const pollEvery = 60 * time.Second

// probeTimeout bounds one vendor probe (OmniRoute uses 8s).
const probeTimeout = 8 * time.Second

// Tracker polls every target on the fixed cadence and caches the latest
// snapshot per (provider, account). onExhausted, when non-nil, runs after
// every successful probe whose worst window is exhausted. A zero Tracker
// is useless — build with New.
type Tracker struct {
	targets     []Target
	onExhausted func(Target, time.Time)
	client      *http.Client
	now         func() time.Time
	every       time.Duration
	// resolveKey returns the CURRENT bearer for (provider, account) at
	// probe time — OAuth-managed accounts rotate their token in the
	// background, so a key captured at build time goes stale.
	resolveKey func(provider, acct string) string
	// probe overrides the HTTP probe (tests).
	probe func(ctx context.Context, t *Tracker, tgt Target) Snapshot

	mu    sync.Mutex
	snaps map[string]Snapshot // provider\x00account -> latest
	stop  chan struct{}
	once  sync.Once
}

// New builds a tracker over targets and starts its poll loop.
func New(targets []Target, onExhausted func(Target, time.Time), resolveKey func(provider, acct string) string) *Tracker {
	return NewAt(targets, onExhausted, nil, resolveKey, pollEvery, nil, nil)
}

// NewAt is New with injectable probe, cadence, client and clock (tests);
// every <= 0 resets to the 60s default.
func NewAt(targets []Target, onExhausted func(Target, time.Time), probe func(context.Context, *Tracker, Target) Snapshot, resolveKey func(provider, acct string) string, every time.Duration, client *http.Client, now func() time.Time) *Tracker {
	if every <= 0 {
		every = pollEvery
	}
	if client == nil {
		client = &http.Client{Timeout: probeTimeout}
	}
	if now == nil {
		now = time.Now
	}
	t := &Tracker{
		targets:     targets,
		onExhausted: onExhausted,
		resolveKey:  resolveKey,
		client:      client,
		now:         now,
		every:       every,
		probe:       probe,
		snaps:       make(map[string]Snapshot, len(targets)),
		stop:        make(chan struct{}),
	}
	go t.loop()
	return t
}

// All returns the latest snapshots ordered by provider then account.
func (t *Tracker) All() []Snapshot {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Snapshot, 0, len(t.snaps))
	for _, s := range t.snaps {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Account < out[j].Account
	})
	return out
}

// Stop ends the loop; idempotent.
func (t *Tracker) Stop() {
	if t == nil {
		return
	}
	t.once.Do(func() { close(t.stop) })
}

// Inherit copies fresh snapshots from an older tracker for (provider,
// account) keys the new target set still has (hot reload keeps the
// dashboard warm between trackers).
func (t *Tracker) Inherit(o *Tracker) {
	if t == nil || o == nil {
		return
	}
	prefixes := make(map[string]struct{}, len(t.targets))
	for _, tgt := range t.targets {
		prefixes[tgt.Provider+"\x00"+tgt.AcctName+"\x00"] = struct{}{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, snap := range o.snaps {
		for prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				t.snaps[key] = snap
				break
			}
		}
	}
}

func (t *Tracker) loop() {
	t.poll()
	tick := time.NewTicker(t.every)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
			t.poll()
		}
	}
}

// poll probes every target concurrently (a handful of subscription
// accounts; one goroutine each) and records the results.
func (t *Tracker) poll() {
	var wg sync.WaitGroup
	for _, tgt := range t.targets {
		wg.Add(1)
		go func(tgt Target) {
			defer wg.Done()
			if t.resolveKey != nil {
				if k := t.resolveKey(tgt.Provider, tgt.AcctName); k != "" {
					tgt.AcctKey = k
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			defer cancel()
			var snap Snapshot
			if t.probe != nil {
				snap = t.probe(ctx, t, tgt)
			} else {
				snap = t.probeHTTP(ctx, tgt)
			}
			sum := fnv.New32a()
			_, _ = sum.Write([]byte(tgt.AcctKey))
			prefix := tgt.Provider + "\x00" + tgt.AcctName + "\x00"
			key := prefix + strconv.FormatUint(uint64(sum.Sum32()), 16)
			t.mu.Lock()
			// One snapshot per (provider, account): a rotated key lands
			// under a new hash and replaces the old entry outright.
			for k := range t.snaps {
				if k != key && strings.HasPrefix(k, prefix) {
					delete(t.snaps, k)
				}
			}
			t.snaps[key] = snap
			t.mu.Unlock()
			t.parkIfExhausted(tgt, snap)
		}(tgt)
	}
	wg.Wait()
}

// parkIfExhausted asks the hook to park the account when the vendor's own
// numbers say a window is fully consumed. The park is capped at one poll
// cycle: the loop re-probes and re-parks while exhausted, and a recovered
// subscription self-heals within one cycle (no stale multi-day benches
// from a vendor that resets early or a parse that read a wrong reset).
func (t *Tracker) parkIfExhausted(tgt Target, snap Snapshot) {
	if snap.Err != "" {
		return
	}
	w, ok := snap.exhaustedWindow()
	if !ok {
		return
	}
	now := t.now()
	until := now.Add(t.every)
	if w.Resets != nil && w.Resets.After(now) && w.Resets.Sub(now) < t.every {
		until = *w.Resets
	}
	if t.onExhausted != nil {
		t.onExhausted(tgt, until)
	}
}

// probeHTTP fetches one target and decodes it per dialect.
func (t *Tracker) probeHTTP(ctx context.Context, tgt Target) Snapshot {
	if tgt.Dialect == CommandCode {
		// Multi-endpoint dialect (OmniRoute fetches whoami + credits +
		// subscriptions + summary); URL is a BASE, not one endpoint.
		return t.probeCommandCode(ctx, tgt)
	}

	if tgt.Dialect == Cursor {
		// No other dialect authenticates by cookie or derives a query
		// parameter from the credential, so it cannot ride the bearer probe.
		return t.probeCursor(ctx, tgt)
	}

	if tgt.Dialect == Freebuff {
		// POST + JSON body + browser-grade UA: not a bearer GET.
		return t.probeFreebuff(ctx, tgt)
	}

	if tgt.Dialect == AgentRouter {
		// Needs a second header (New-Api-User) alongside the bearer.
		return t.probeAgentRouter(ctx, tgt)
	}
	url := tgt.URL
	if url == "" {
		url = DefaultURL(tgt.Dialect)
	}
	snap := Snapshot{Provider: tgt.Provider, Account: tgt.AcctName, Dialect: tgt.Dialect, URL: url, FetchedAt: t.now()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		snap.Err = err.Error()
		return snap
	}
	req.Header.Set("Authorization", "Bearer "+tgt.AcctKey)
	req.Header.Set("Accept", "application/json")
	if tgt.Dialect == GrokCli {
		// OmniRoute's grokQuotaFetcher fingerprint (x-grok-client-mode:
		// cli is what the endpoint keys its response shape on).
		req.Header.Set("x-grok-client-mode", "cli")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		snap.Err = err.Error()
		return snap
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		snap.Err = "read quota response: " + err.Error()
		return snap
	}
	switch tgt.Dialect {
	case OpenCodeGo:
		snap.Plan = "OpenCode Go"
		snap.Windows, snap.Err = parseOpenCodeGo(body, resp.StatusCode)
	case Zai, ZaiCN:
		snap.Windows, snap.Plan, snap.Err = parseZai(body, resp.StatusCode)
	case GrokCli:
		snap.Windows, snap.Plan, snap.Err = parseGrokCli(tgt.AcctKey, body, resp.StatusCode)
	default:
		snap.Err = "unknown subscription quota dialect " + strconv.Quote(tgt.Dialect)
	}
	return snap
}

// parseOpenCodeGo decodes 9router's verified OpenCode Zen Go shape:
// {"usage":{"rolling":{"percent":13,"resetsAt":"..."},"weekly":{...},"monthly":{...}}}
func parseOpenCodeGo(body []byte, status int) ([]Window, string) {
	if status == http.StatusUnauthorized {
		return nil, "OpenCode Go authentication failed. Check the API key."
	}
	if status == http.StatusForbidden {
		var e struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error.Type == "EntitlementError" {
			return nil, "OpenCode Go subscription required for this API key."
		}
		return nil, "OpenCode Go access forbidden for this API key."
	}
	if status != http.StatusOK {
		return nil, "OpenCode Go usage API error (" + strconv.Itoa(status) + ")."
	}
	var data struct {
		Usage struct {
			Rolling json.RawMessage `json:"rolling"`
			Weekly  json.RawMessage `json:"weekly"`
			Monthly json.RawMessage `json:"monthly"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, "OpenCode Go usage response did not contain quota data."
	}
	periods := []struct {
		raw  json.RawMessage
		name string
	}{
		{data.Usage.Rolling, "Rolling"},
		{data.Usage.Weekly, "Weekly"},
		{data.Usage.Monthly, "Monthly"},
	}
	windows := make([]Window, 0, 3)
	for _, period := range periods {
		if len(period.raw) == 0 {
			continue
		}
		var q struct {
			Percent  any `json:"percent"`
			ResetsAt any `json:"resetsAt"`
		}
		if err := json.Unmarshal(period.raw, &q); err != nil {
			continue
		}
		pct, ok := asPercent(q.Percent)
		if !ok {
			continue
		}
		windows = append(windows, Window{Name: period.name, Used: pct, Resets: asReset(q.ResetsAt)})
	}
	if len(windows) == 0 {
		return nil, "OpenCode Go usage response did not contain valid quota data."
	}
	return windows, ""
}

// parseZai decodes 9router's verified GLM Coding Plan shape:
// {"code":200,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3,"number":5,
//
//	"percentage":25,"nextResetTime":1787905548392}],"level":"lite"},"success":true}
//
// unit 3 = session window (number hours), unit 6 = weekly; TOKENS_LIMIT and
// CREDIT_LIMIT are both percent-based.
func parseZai(body []byte, status int) ([]Window, string, string) {
	if status == http.StatusUnauthorized {
		return nil, "", "GLM API key invalid or expired."
	}
	if status != http.StatusOK {
		return nil, "", "GLM quota API error (" + strconv.Itoa(status) + ")."
	}
	var data struct {
		Code int `json:"code"`
		Data struct {
			Limits []struct {
				Type          string `json:"type"`
				Unit          int    `json:"unit"`
				Number        int    `json:"number"`
				Percentage    any    `json:"percentage"`
				NextResetTime any    `json:"nextResetTime"`
			} `json:"limits"`
			Level string `json:"level"`
		} `json:"data"`
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, "", "GLM quota response is not valid JSON."
	}
	if data.Code != 200 || !data.Success {
		return nil, "", "GLM quota API error (code " + strconv.Itoa(data.Code) + ")."
	}
	windows := make([]Window, 0, len(data.Data.Limits))
	for _, limit := range data.Data.Limits {
		switch limit.Type {
		case "TOKENS_LIMIT", "CREDIT_LIMIT":
		default:
			continue
		}
		pct, ok := asPercent(limit.Percentage)
		if !ok {
			continue
		}
		key := "Limit (" + strconv.Itoa(limit.Number) + ")"
		switch {
		case limit.Unit == 3:
			n := limit.Number
			if n <= 0 {
				n = 5
			}
			key = "Session (" + strconv.Itoa(n) + "h)"
		case limit.Unit == 6:
			key = "Weekly (7d)"
		case limit.Type == "TOKENS_LIMIT":
			key = "Tokens"
		}
		windows = append(windows, Window{Name: key, Used: pct, Resets: asReset(limit.NextResetTime)})
	}
	if len(windows) == 0 {
		return nil, "", "GLM quota response did not contain valid limit data."
	}
	plan := "Unknown"
	if l := strings.TrimSpace(data.Data.Level); l != "" {
		plan = strings.ToUpper(l[:1]) + strings.ToLower(l[1:])
	}
	return windows, plan, ""
}

// probeCommandCode ports OmniRoute's usage/command-code.ts: one GET per
// /alpha surface (whoami → orgId, usage/summary → period spend,
// billing/credits → windows + credit pool, billing/subscriptions → plan +
// period), all bearer-authenticated against the API base. Credits is the
// load-bearing call (its windowLimits carry the exhausted flags); summary
// turns the credits window into a real used-percent, and everything else
// enriches but never fails the probe.
func (t *Tracker) probeCommandCode(ctx context.Context, tgt Target) Snapshot {
	base := tgt.URL
	if base == "" {
		base = DefaultURL(CommandCode)
	}
	base = strings.TrimSuffix(base, "/")
	snap := Snapshot{Provider: tgt.Provider, Account: tgt.AcctName, Dialect: CommandCode, URL: base, FetchedAt: t.now()}

	get := func(path string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tgt.AcctKey)
		req.Header.Set("Accept", "application/json")
		resp, err := t.client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return resp.StatusCode, nil, err
		}
		return resp.StatusCode, body, nil
	}

	// whoami is optional: it only scopes the billing queries to an org
	// (OmniRoute continues without orgId on any failure).
	q := ""
	if _, body, err := get("/alpha/whoami"); err == nil {
		var who struct {
			Org *struct {
				ID string `json:"id"`
			} `json:"org"`
		}
		if json.Unmarshal(body, &who) == nil && who.Org != nil && strings.TrimSpace(who.Org.ID) != "" {
			q = "?orgId=" + url.QueryEscape(strings.TrimSpace(who.Org.ID))
		}
	}

	// Period spend (soft-fail): totalCost — falling back to
	// totalMonthlyCredits — scopes the credits pool to real usage. The
	// vendor's own default period is the billing period (live-verified:
	// periodBasis "billing-period", identical with or without ?since).
	spend := 0.0
	if _, body, err := get("/alpha/usage/summary" + q); err == nil {
		var sum struct {
			TotalCost           float64 `json:"totalCost"`
			TotalMonthlyCredits float64 `json:"totalMonthlyCredits"`
		}
		if json.Unmarshal(body, &sum) == nil {
			spend = sum.TotalCost
			if spend <= 0 {
				spend = sum.TotalMonthlyCredits
			}
		}
	}

	status, body, err := get("/alpha/billing/credits" + q)
	if err != nil {
		snap.Err = err.Error()
		return snap
	}
	windows, plan, perr := parseCommandCode(body, status, spend)
	if perr != "" {
		snap.Err = perr
		return snap
	}
	snap.Windows = windows

	// Subscriptions enrich the plan label and the credits reset; a missing
	// subscription (team orgs, rotated keys) must not fail the probe.
	if _, body, err := get("/alpha/billing/subscriptions" + q); err == nil {
		var sub struct {
			Data struct {
				PlanID           string `json:"planId"`
				CurrentPeriodEnd any    `json:"currentPeriodEnd"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &sub) == nil {
			if p := commandCodePlanLabel(sub.Data.PlanID); p != "" {
				plan = p
			}
			if reset := asReset(sub.Data.CurrentPeriodEnd); reset != nil {
				// The credits pool resets with the billing period; only
				// fill it when the vendor's credits call didn't already
				// give that window its own reset.
				for i := range snap.Windows {
					if snap.Windows[i].Name == creditsWindow && snap.Windows[i].Resets == nil {
						snap.Windows[i].Resets = reset
					}
				}
			}
		}
	}
	snap.Plan = plan
	return snap
}

// creditsWindow is the monthly credit-pool window name (see parseCommandCode).
const creditsWindow = "Credits (monthly)"

// parseCommandCode decodes the live-verified /alpha/billing/credits shape:
//
//	{"credits":{"monthlyCredits":10.28,"purchasedCredits":0,"freeCredits":0},
//	 "windowLimits":{"limited":true,"exceeded":"weekly",
//	   "fiveHour":{"used":0,"cap":14,"exceeded":false,"resetAt":0},
//	   "weekly":{"used":35.0018,"cap":35,"exceeded":true,
//	             "resetAt":1789539876848}}}}
//
// Windows: five_hour/weekly roll USD used against cap; credits is the
// monthly pool — spend (the /alpha/usage/summary period total) against
// spend+remaining, OmniRoute's totalCost math. The weekly window above IS
// exhausted (used >= cap) — the shape that parks the account.
func parseCommandCode(body []byte, status int, spend float64) ([]Window, string, string) {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, "", "CommandCode API key was rejected — reconnect or rotate the key."
	}
	if status != http.StatusOK {
		return nil, "", "CommandCode credits API error (" + strconv.Itoa(status) + ")."
	}
	var data struct {
		Credits *struct {
			MonthlyCredits   float64 `json:"monthlyCredits"`
			PurchasedCredits float64 `json:"purchasedCredits"`
			FreeCredits      float64 `json:"freeCredits"`
		} `json:"credits"`
		WindowLimits struct {
			FiveHour struct {
				Used    float64 `json:"used"`
				Cap     float64 `json:"cap"`
				ResetAt float64 `json:"resetAt"`
			} `json:"fiveHour"`
			Weekly struct {
				Used    float64 `json:"used"`
				Cap     float64 `json:"cap"`
				ResetAt float64 `json:"resetAt"`
			} `json:"weekly"`
		} `json:"windowLimits"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, "", "CommandCode credits response is not valid JSON."
	}
	windows := make([]Window, 0, 3)
	for _, w := range []struct {
		name  string
		used  float64
		cap   float64
		reset float64
	}{
		{"5-hour window", data.WindowLimits.FiveHour.Used, data.WindowLimits.FiveHour.Cap, data.WindowLimits.FiveHour.ResetAt},
		{"Weekly window", data.WindowLimits.Weekly.Used, data.WindowLimits.Weekly.Cap, data.WindowLimits.Weekly.ResetAt},
	} {
		if w.cap <= 0 {
			continue // window not configured for this plan
		}
		pct := w.used / w.cap * 100
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100 // over-cap usage parks at 100%
		}
		// Floor, never round: the window is exhausted only when used >=
		// cap. Rounding made 99.5-99.99% read 100 and re-park an account
		// that still had spendable headroom every poll cycle.
		windows = append(windows, Window{Name: w.name, Used: int(pct), Resets: asReset(w.reset)})
	}
	// Credits pool (monthly + purchased + free remaining) against the
	// pool total = spend (usage/summary period total) + remaining
	// (OmniRoute's totalCost math, live-verified on a GOAT key:
	// remaining 10.29 + spend 59.87 = total 70). Drained pool parks;
	// spend 0 keeps the informational 0% (vendor grants not yet spent).
	if data.Credits != nil {
		remaining := data.Credits.MonthlyCredits + data.Credits.PurchasedCredits + data.Credits.FreeCredits
		if remaining < 0 {
			remaining = 0
		}
		if spend < 0 {
			spend = 0
		}
		credits := Window{Name: creditsWindow}
		switch {
		case spend+remaining <= 0:
			credits.Used = 100 // nothing granted and nothing spent: drained
		case remaining == 0:
			credits.Used = 100 // drained pool parks until the vendor refills
		default:
			// Floor: the fraction never reaches 1, so spend alone can
			// never fabricate the drained park — only remaining == 0 does.
			credits.Used = int(spend / (spend + remaining) * 100)
		}
		windows = append(windows, credits)
	}
	if len(windows) == 0 {
		return nil, "", "CommandCode credits response did not contain valid quota data."
	}
	return windows, "Command Code", ""
}

// commandCodePlanLabel maps OmniRoute's PLAN_LABELS; unknown ids fall back
// to a title-cased split (individual-goat → Goat) so the row never shows an
// empty plan.
func commandCodePlanLabel(planID string) string {
	switch planID {
	case "individual-goat":
		return "Command Code · GOAT"
	case "individual-go":
		return "Command Code · Go"
	case "individual-pro":
		return "Command Code · Pro"
	case "individual-max-10x":
		return "Command Code · Max 10×"
	case "individual-max-20x":
		return "Command Code · Max 20×"
	case "team-pro":
		return "Command Code · Team Pro"
	}
	id := strings.TrimPrefix(planID, "individual-")
	id = strings.TrimPrefix(id, "team-")
	if id == planID || id == "" {
		return "" // unrecognized shape: keep the default label
	}
	parts := strings.Split(id, "-")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return "Command Code · " + strings.Join(parts, " ")
}

// grokTierLabels maps the auth.x.ai access-token `tier` claim (9router's
// usage/grok-cli.js tier map). Display only: upstream stays authoritative
// for entitlement, so an unknown value degrades to the plain vendor name.
var grokTierLabels = map[string]string{
	"0": "Free",
	"1": "SuperGrok",
	"2": "X Basic",
	"3": "X Premium",
	"4": "X Premium Plus",
	"5": "SuperGrok Heavy",
	"6": "SuperGrok Lite",
}

// grokPlanLabel reads the tier claim out of an auth.x.ai JWT without any
// network call — the billing probe already holds the bearer, so the plan
// costs zero extra requests (9router's planFromAccessToken does the same).
func grokPlanLabel(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return "Grok"
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "Grok"
	}
	var claims struct {
		Tier json.Number `json:"tier"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return "Grok"
	}
	if label, ok := grokTierLabels[claims.Tier.String()]; ok {
		return "Grok · " + label
	}
	return "Grok"
}

// parseGrokCli decodes OmniRoute's grokQuotaFetcher shape from
// GET https://cli-chat-proxy.grok.com/v1/billing?format=credits:
//
//	{"config":{"creditUsagePercent":62.4,"currentPeriod":
//	  {"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-09-17T00:00:00Z"},
//	  "productUsage":[{"product":"grok-build","usagePercent":41}]}}
//
// creditUsagePercent is the ONE shared weekly pool; productUsage is a
// breakdown legend and is deliberately never split into extra windows
// (9router's own warning), so exactly one window is emitted and 100 %
// parks the account.
func parseGrokCli(token string, body []byte, status int) ([]Window, string, string) {
	plan := grokPlanLabel(token)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, plan, "Grok session token rejected — re-run: onegw-oauth login -provider xai"
	}
	if status != http.StatusOK {
		return nil, plan, "Grok billing API error (" + strconv.Itoa(status) + ")."
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var data struct {
		Config struct {
			CreditUsagePercent  any `json:"creditUsagePercent"`
			CreditUsagePercent2 any `json:"credit_usage_percent"`
			CurrentPeriod       struct {
				Type any `json:"type"`
				End  any `json:"end"`
			} `json:"currentPeriod"`
		} `json:"config"`
	}
	if err := dec.Decode(&data); err != nil {
		return nil, plan, "Grok billing response is not valid JSON."
	}
	raw := data.Config.CreditUsagePercent
	if raw == nil {
		raw = data.Config.CreditUsagePercent2
	}
	used, ok := grokPercent(raw)
	if !ok {
		return nil, plan, "Grok billing response did not contain valid quota data."
	}
	windows := []Window{{Name: grokPeriodName(grokString(data.Config.CurrentPeriod.Type)), Resets: grokReset(data.Config.CurrentPeriod.End), Used: used}}
	return windows, plan, ""
}

// grokPercent reads a vendor percent as a NUMBER or a numeric STRING and
// FLOORS it: rounding made 99.5-99.99 % read 100 and re-park an account
// the vendor still considered spendable (the commandcode lesson).
func grokPercent(v any) (int, bool) {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case json.Number:
		parsed, err := x.Float64()
		if err != nil {
			return 0, false
		}
		f = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	if f < 0 {
		f = 0
	}
	if f > 100 {
		f = 100
	}
	return int(f), true
}

// grokString renders a scalar field that may arrive as a JSON string or
// number; anything else reads as empty.
func grokString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	}
	return ""
}

// grokPeriodName titles the pool after the vendor's period type
// (USAGE_PERIOD_TYPE_WEEKLY → "Weekly pool").
func grokPeriodName(typ string) string {
	switch strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(typ, "USAGE_PERIOD_TYPE_"))) {
	case "WEEKLY":
		return "Weekly pool"
	case "MONTHLY":
		return "Monthly pool"
	case "DAILY":
		return "Daily pool"
	}
	return "Usage pool"
}

// grokReset decodes a period end that arrives either as an RFC3339 /
// epoch value or in the protobuf-JSON {seconds, nanos} shape.
func grokReset(v any) *time.Time {
	if x, ok := v.(map[string]any); ok {
		return asReset(x["seconds"])
	}
	return asReset(v)
}

// ---------------------------------------------------------------------------
// Cursor (cursor.com session-cookie dashboard API)
// ---------------------------------------------------------------------------

// probeCursor fetches one Cursor account's meter state. The endpoint is the
// browser dashboard's own API, so auth is a session cookie rather than a
// bearer: WorkosCursorSessionToken=<uid>%3A%3A<jwt>, with <uid> the session
// JWT's sub claim minus its identity-provider prefix. usage-summary needs no
// query parameter — the cookie alone identifies the account. Contract ported
// from cursor-usage-extension/extension.js (extractUserIdFromToken/
// buildCookie) and live-verified 2026-09-14 against both configured Cursor
// accounts. NOTE: only BROWSER-type sessions authenticate here — a CLI/agent
// keychain token gets 401 (measured 2026-09-14), unlike the old /api/usage.
func (t *Tracker) probeCursor(ctx context.Context, tgt Target) Snapshot {
	base := tgt.URL
	if base == "" {
		base = DefaultURL(Cursor)
	}
	snap := Snapshot{Provider: tgt.Provider, Account: tgt.AcctName, Dialect: Cursor, URL: base, FetchedAt: t.now()}
	uid, errMsg := cursorUserID(tgt.AcctKey)
	if errMsg != "" {
		snap.Err = errMsg
		return snap
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		snap.Err = err.Error()
		return snap
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", "WorkosCursorSessionToken="+uid+"%3A%3A"+tgt.AcctKey)
	resp, err := t.client.Do(req)
	if err != nil {
		snap.Err = err.Error()
		return snap
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		snap.Err = "read cursor usage response: " + err.Error()
		return snap
	}
	snap.Windows, snap.Plan, snap.Err = parseCursor(body, resp.StatusCode)
	return snap
}

// cursorUserID reads the sub claim of a Cursor session JWT and strips its
// provider prefix: "auth0|user_01K7…" and "grok|user_01M1…" both resolve to
// the bare user id (live 2026-09-14 — Cursor issues the same session shape
// under more than one connection prefix). Failures come back as probe-error
// text, matching what every other parser in this file returns.
func cursorUserID(jwt string) (string, string) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return "", "cursor: account credential is not a session JWT"
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", "cursor: session JWT payload does not decode"
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", "cursor: session JWT claims are not valid JSON"
	}
	uid := strings.TrimSpace(claims.Sub)
	if uid == "" {
		return "", "cursor: session JWT carries no sub claim"
	}
	if _, after, found := strings.Cut(uid, "|"); found {
		uid = after
	}
	return uid, ""
}

// parseCursor decodes cursor.com/api/usage-summary (live shape 2026-09-14,
// both configured accounts probed with their own browser session cookies):
//
//	{"billingCycleStart":"2026-09-03T03:51:46.559Z","billingCycleEnd":
//	 "2026-10-03T03:51:46.559Z","membershipType":"free","limitType":"user",
//	 "isUnlimited":false,
//	 "autoModelSelectedDisplayMessage":"You've used 5% of your included total usage",
//	 "namedModelSelectedDisplayMessage":"You've used 0% of your included API usage",
//	 "individualUsage":{"plan":{"enabled":true,"autoPercentUsed":9,
//	  "apiPercentUsed":0,"totalPercentUsed":4.5},"onDemand":{"enabled":false}},
//	 "teamUsage":{}}
//
// The two dashboard meters are totalPercentUsed and apiPercentUsed, and the
// display messages are exactly those numbers rounded to whole percents — so
// "included usage" and "included API usage" carry those names and the same
// rounding, making the Quota table read like cursor.com's own banner. The
// reset is the vendor's own billingCycleEnd (no month-anchor arithmetic: the
// summary gives the instant, unlike the old startOfMonth + AddDate guess that
// could read days off on signup-anchored cycles).
//
// isUnlimited is the uncapped-lane rule inherited from the old dialect: the
// windows still render (a row per meter) but read 0% forever, so an uncapped
// account is tracked, never parked. teamUsage is deliberately ignored: for
// team members the per-user meters live in individualUsage.plan (live
// 2026-09-14: the enterprise/team account's 12.35% headline appears there
// while teamUsage carries only onDemand flags).
func parseCursor(body []byte, status int) ([]Window, string, string) {
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, "", "Cursor session token invalid or expired — re-export it from a BROWSER cursor.com dashboard session (the WorkosCursorSessionToken cookie); CLI/agent keychain tokens get 401 on usage-summary."
	default:
		return nil, "", "Cursor usage API error (" + strconv.Itoa(status) + ")."
	}
	var top struct {
		MembershipType  string `json:"membershipType"`
		BillingCycleEnd string `json:"billingCycleEnd"`
		IsUnlimited     bool   `json:"isUnlimited"`
		IndividualUsage struct {
			Plan struct {
				TotalPercentUsed float64 `json:"totalPercentUsed"`
				APIPercentUsed   float64 `json:"apiPercentUsed"`
			} `json:"plan"`
		} `json:"individualUsage"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, "", "Cursor usage response is not valid JSON."
	}
	if top.MembershipType == "" && top.BillingCycleEnd == "" {
		return nil, "", "Cursor usage response carried no summary meters."
	}
	var resets *time.Time
	if s := strings.TrimSpace(top.BillingCycleEnd); s != "" {
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			resets = &ts
		}
	}
	inc, api := 0, 0
	if !top.IsUnlimited {
		inc, _ = asPercent(top.IndividualUsage.Plan.TotalPercentUsed)
		api, _ = asPercent(top.IndividualUsage.Plan.APIPercentUsed)
	}
	windows := []Window{
		{Name: "included usage", Used: inc, Resets: resets},
		{Name: "included API usage", Used: api, Resets: resets},
	}
	plan := top.MembershipType
	if top.IsUnlimited {
		plan = "uncapped"
	}
	return windows, plan, ""
}

// ---------------------------------------------------------------------------
// freebuff (Codebuff free tier) — POST session probe
// ---------------------------------------------------------------------------

// freebuffModel is the model the session endpoint is probed with. Live
// 2026-09-16: the header is ignored when only one model is admitted (the
// response echoes deepseek/deepseek-v4-flash whatever is asked for), but
// OmniRoute's validator sends it and the free tier's admission is per-model,
// so the probe sends the same one to keep reading the same pool.
const freebuffModel = "deepseek/deepseek-v4-flash"

// probeFreebuff POSTs the session endpoint OmniRoute's validateFreebuffProvider
// and its freebuff executor both hit. It is the one dialect that is a POST
// with a body and a browser-grade User-Agent (the endpoint 401s a bare
// GET-shaped request), so it cannot ride the shared bearer probe.
//
// Valid is 200 OR 409: 409 means the account already holds a live session,
// which is exactly as usable as a fresh one (OmniRoute's rule).
func (t *Tracker) probeFreebuff(ctx context.Context, tgt Target) Snapshot {
	url := tgt.URL
	if url == "" {
		url = DefaultURL(Freebuff)
	}
	snap := Snapshot{Provider: tgt.Provider, Account: tgt.AcctName, Dialect: Freebuff, URL: url, FetchedAt: t.now()}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		snap.Err = err.Error()
		return snap
	}
	req.Header.Set("Authorization", "Bearer "+tgt.AcctKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codebuff/0.1.0 (darwin-arm64)")
	req.Header.Set("x-freebuff-model", freebuffModel)
	resp, err := t.client.Do(req)
	if err != nil {
		snap.Err = err.Error()
		return snap
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		snap.Err = "read freebuff session response: " + err.Error()
		return snap
	}
	snap.Windows, snap.Plan, snap.Err = parseFreebuff(body, resp.StatusCode)
	return snap
}

// parseFreebuff decodes the live-verified free-tier session shape (2026-09-16):
//
//	{"status":"active","accessTier":"limited","instanceId":"d997df40-…",
//	 "freebucks":{"balance":10,"daily":{"limit":25,"spent":15,"remaining":10,
//	               "resetAt":"2026-09-17T07:00:00.000Z"},"wallet":{"balance":0}},
//	 "rateLimit":{"model":"deepseek/deepseek-v4-flash","limit":6,
//	               "recentCount":1,"poolLabel":"Daily",
//	               "resetAt":"2026-09-17T07:00:00.000Z"}}
//
// Two windows, both free-tier meters: the daily freebucks pool
// (spent/limit — the currency free sessions are actually charged in) and the
// probed model's own daily admission count (recentCount/limit). recentCount
// is the session that running the probe just consumed, which is why the
// dollars window is the meaningful one: it moves only when a real prompt
// spends. Access tier rides the plan label ("limited" free tier vs a paid
// "starter"/"plus"/"pro" subscription).
func parseFreebuff(body []byte, status int) ([]Window, string, string) {
	switch status {
	case http.StatusOK, http.StatusConflict:
		// 409 = the account already has a live session: a usable account.
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, "", "Freebuff auth token invalid or expired — re-sign-in with the codebuff CLI (or paste a fresh token from freebuff.com)."
	default:
		return nil, "", "Freebuff session API error (" + strconv.Itoa(status) + ")."
	}
	var data struct {
		Status     string `json:"status"`
		AccessTier string `json:"accessTier"`
		Freebucks  *struct {
			Balance float64 `json:"balance"`
			Daily   struct {
				Limit     float64 `json:"limit"`
				Spent     float64 `json:"spent"`
				Remaining float64 `json:"remaining"`
				ResetAt   any     `json:"resetAt"`
			} `json:"daily"`
		} `json:"freebucks"`
		RateLimit *struct {
			Model       string  `json:"model"`
			Limit       float64 `json:"limit"`
			RecentCount float64 `json:"recentCount"`
			PoolLabel   string  `json:"poolLabel"`
			ResetAt     any     `json:"resetAt"`
		} `json:"rateLimit"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, "", "Freebuff session response is not valid JSON."
	}
	plan := data.AccessTier
	if plan != "" {
		plan = "freebuff " + plan
	}
	windows := make([]Window, 0, 2)
	if d := data.Freebucks; d != nil && d.Daily.Limit > 0 {
		// Floor, never round (same rule as the other dialects): a pool is
		// exhausted only when spent >= limit, so 24/25 must stay 96%.
		pct := d.Daily.Spent / d.Daily.Limit * 100
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		windows = append(windows, Window{Name: "Freebucks (" + fmtFreebucks(d.Daily.Remaining) + " left)", Used: int(pct), Resets: asReset(d.Daily.ResetAt)})
	}
	if r := data.RateLimit; r != nil && r.Limit > 0 {
		pct := r.RecentCount / r.Limit * 100
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		name := "Sessions"
		if r.Model != "" {
			name = "Sessions " + r.Model
		}
		if r.PoolLabel != "" {
			name += " (" + r.PoolLabel + ")"
		}
		windows = append(windows, Window{Name: name, Used: int(pct), Resets: asReset(r.ResetAt)})
	}
	// ponytail: a free account whose daily pool is spent still answers 200
	// with limit 6 and recentCount 1, so there is no reliable "refused"
	// signal to read here — the windows carry the numbers and the gateway
	// parks on 100% like every other dialect. Upgrade path if Codebuff ever
	// exposes an explicit exhausted flag: parse it into these windows.
	if len(windows) == 0 {
		return nil, "", "Freebuff session response carried no quota windows (status " + strconv.Quote(data.Status) + ")."
	}
	return windows, plan, ""
}

// fmtFreebucks renders a freebucks amount without a trailing ".0" for whole
// units ("10 left", not "10.0 left").
func fmtFreebucks(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// ---------------------------------------------------------------------------
// agentrouter (New-API) — console wallet balance
// ---------------------------------------------------------------------------

// quotaPerUnit is New-API's units-per-dollar constant. OmniRoute hardcodes it
// (QUOTA_PER_UNIT = 500_000) rather than paying a second /api/status
// round-trip per fetch to read the same number; the ratio is a property of
// the New-API deployment, not of the account.
const quotaPerUnit = 500_000

// probeAgentRouter GETs New-API's console balance API, which needs TWO
// credentials: the console System Access Token as the bearer (NOT the
// routing sk- key) and the console user id in New-Api-User. The id rides
// Target.ConsoleUser; with no id there is nothing to probe, so the snapshot
// carries an actionable error instead of a 401 with no explanation.
func (t *Tracker) probeAgentRouter(ctx context.Context, tgt Target) Snapshot {
	url := tgt.URL
	if url == "" {
		url = DefaultURL(AgentRouter)
	}
	snap := Snapshot{Provider: tgt.Provider, Account: tgt.AcctName, Dialect: AgentRouter, URL: url, FetchedAt: t.now()}
	if tgt.ConsoleUser == "" {
		snap.Err = "AgentRouter needs the console New-API user id (providers.subscription_user) — it is the number in agentrouter.org's console URL."
		return snap
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		snap.Err = err.Error()
		return snap
	}
	req.Header.Set("Authorization", "Bearer "+tgt.AcctKey)
	req.Header.Set("New-Api-User", tgt.ConsoleUser)
	req.Header.Set("Accept", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		snap.Err = err.Error()
		return snap
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		snap.Err = "read agentrouter user response: " + err.Error()
		return snap
	}
	snap.Windows, snap.Plan, snap.Err = parseAgentRouter(body, resp.StatusCode)
	return snap
}

// parseAgentRouter decodes New-API's /api/user/self: {"data":{"quota":<int>}}
// with quota in New-API credit units (quotaPerUnit per dollar). OmniRoute's
// agentrouterQuotaFetcher reads the same field and derives a boolean-ish
// percent — 0% while any balance remains, 100% at zero — because New-API
// reports no grant total to divide by, so a real percentage is not derivable
// from this endpoint.
func parseAgentRouter(body []byte, status int) ([]Window, string, string) {
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, "", "AgentRouter console token was rejected — re-copy the System Access Token from agentrouter.org's console."
	default:
		return nil, "", "AgentRouter user API error (" + strconv.Itoa(status) + ")."
	}
	var data struct {
		Data *struct {
			Quota *float64 `json:"quota"`
		} `json:"data"`
		// New-API answers a failed auth with 200 + success:false, so the
		// envelope has to be checked as well as the status code.
		Success *bool  `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, "", "AgentRouter user response is not valid JSON."
	}
	if data.Success != nil && !*data.Success {
		msg := strings.TrimSpace(data.Message)
		if msg == "" {
			msg = "the console token or the New-API user id is wrong"
		}
		return nil, "", "AgentRouter console auth failed: " + msg
	}
	if data.Data == nil || data.Data.Quota == nil {
		return nil, "", "AgentRouter user response carried no quota field."
	}
	usd := *data.Data.Quota / quotaPerUnit
	used := 0
	if *data.Data.Quota <= 0 {
		used = 100 // a drained wallet is the one state this endpoint reports
	}
	return []Window{{
			Name: "Wallet ($" + strconv.FormatFloat(usd, 'f', 2, 64) + " left)",
			Used: used,
		}},
		"AgentRouter", ""
}

// asPercent clamps a vendor percentage (number or numeric string) to 0-100.
func asPercent(v any) (int, bool) {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case json.Number:
		parsed, err := x.Float64()
		if err != nil {
			return 0, false
		}
		f = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	if f < 0 {
		f = 0
	}
	if f > 100 {
		f = 100
	}
	return int(f + 0.5), true
}

// asReset decodes a vendor reset instant: epoch seconds or milliseconds
// (numbers/numeric strings), or an RFC3339 string (9router's parseResetTime
// semantics).
func asReset(v any) *time.Time {
	switch x := v.(type) {
	case float64:
		return resetFromUnits(x)
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return nil
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return resetFromUnits(n)
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil
		}
		return &t
	default:
		return nil
	}
}

func resetFromUnits(n float64) *time.Time {
	if n <= 0 {
		return nil
	}
	if n < 1e12 { // seconds, not milliseconds
		n *= 1000
	}
	t := time.UnixMilli(int64(n))
	return &t
}
