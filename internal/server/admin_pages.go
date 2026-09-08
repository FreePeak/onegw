package server

// Admin UI pages + grouped API + SSE live events (#41/#45/#19).
//
// Pages are server-rendered html/template views (internal/server/dashboard
// embeds the templates and vendored htmx/uPlot). Live updates ride one SSE
// endpoint with a bounded fan-out: a slow dashboard can never grow RSS —
// events are dropped and the client is told to resync. Config stays
// file-based; every page here is read-mostly.

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"onegw/internal/server/dashboard"
	"onegw/internal/store"
	"onegw/internal/types"
)

// ---------------------------------------------------------------------------
// SSE hub — bounded fan-out over stdlib http.Flusher
// ---------------------------------------------------------------------------

// sseSub is one EventSource subscriber. ch is a small buffer of pending
// SSE frames; if the client can't keep up and the buffer fills, we drop
// the subscriber with a `resync` event instead of buffering unbounded.
type sseSub struct {
	ch     chan string
	topics map[string]bool
	dead   chan struct{}
	once   sync.Once
}

func (sub *sseSub) kill() { sub.once.Do(func() { close(sub.dead) }) }

type sseHub struct {
	mu     sync.Mutex
	subs   map[*sseSub]bool
	closed chan struct{}
}

const (
	maxSSESubs    = 32
	sseRingFrames = 32 // frames of ≤256 B each → ~8 KB cap per subscriber
)

func newSSEHub() *sseHub {
	return &sseHub{subs: map[*sseSub]bool{}, closed: make(chan struct{})}
}

func (h *sseHub) subscribe(topics []string) *sseSub {
	sub := &sseSub{
		ch:     make(chan string, sseRingFrames),
		topics: map[string]bool{},
		dead:   make(chan struct{}),
	}
	for _, t := range topics {
		sub.topics[t] = true
	}
	h.mu.Lock()
	// Bounded: at capacity, drop the oldest subscriber (EventSource
	// reconnects automatically and resyncs) rather than growing RSS.
	if len(h.subs) >= maxSSESubs {
		// Map order is arbitrary — evict any one subscriber; EventSource
		// reconnects it automatically and the client resyncs.
		for oldest := range h.subs {
			delete(h.subs, oldest)
			oldest.kill()
			break
		}
	}
	h.subs[sub] = true
	h.mu.Unlock()
	return sub
}

func (h *sseHub) unsubscribe(sub *sseSub) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
	sub.kill()
}

// publish queues a frame to subscribers watching the topic. Never blocks:
// a full ring closes the subscriber (client resyncs on reconnect).
func (h *sseHub) publish(topic, data string) {
	if h == nil {
		return
	}
	frame := "event: " + topic + "\ndata: " + strings.ReplaceAll(data, "\n", " ") + "\n\n"
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if !sub.topics[topic] {
			continue
		}
		select {
		case sub.ch <- frame:
		default: // ring full → drop subscriber
			delete(h.subs, sub)
			sub.kill()
		}
	}
}

func (h *sseHub) shutdown() {
	if h == nil {
		return
	}
	select {
	case <-h.closed:
	default:
		close(h.closed)
	}
}

// handleEvents serves GET /admin/events?topics=a,b as an SSE stream.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	var topics []string
	for _, t := range strings.Split(r.URL.Query().Get("topics"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			topics = append(topics, t)
		}
	}
	if len(topics) == 0 {
		topics = []string{"health"}
	}
	sub := s.events.subscribe(topics)
	defer s.events.unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	if sub.topics["health"] {
		fmt.Fprint(w, s.healthFrame())
	}
	flusher.Flush()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.dead:
			// Ring overflow: tell the client everything it missed is gone.
			fmt.Fprint(w, "event: resync\ndata: {}\n\n")
			flusher.Flush()
			return
		case frame := <-sub.ch:
			if _, err := fmt.Fprint(w, frame); err != nil {
				return
			}
			flusher.Flush()
		case <-tick.C:
			if sub.topics["health"] {
				if _, err := fmt.Fprint(w, s.healthFrame()); err != nil {
					return
				}
				flusher.Flush()
			}
		case <-s.events.closed:
			fmt.Fprint(w, "event: bye\ndata: {}\n\n")
			flusher.Flush()
			return
		}
	}
}

// healthFrame builds the 1s health SSE event (Overview live strip). The
// payload is an HTML fragment — htmx swaps it straight into the DOM, so
// the strip renders styled values instead of raw JSON.
func (s *Server) healthFrame() string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	held, rejected := int64(0), int64(0)
	if st := s.cur(); st != nil && st.budget != nil {
		held, rejected = st.budget.Stats()
	}
	frag := fmt.Sprintf(
		`<div class="kv" style="grid-template-columns:repeat(4,1fr)">`+
			`<div><div class="k">in-flight</div><div class="big" style="font-size:18px">%d</div></div>`+
			`<div><div class="k">uptime</div><div class="mono">%s</div></div>`+
			`<div><div class="k">heap alloc / sys</div><div class="mono">%d / %d MiB</div></div>`+
			`<div><div class="k">GC cycles</div><div class="mono">%d</div></div>`+
			`<div><div class="k">budget held</div><div class="mono">%s</div></div>`+
			`<div><div class="k">budget 503s</div><div class="mono">%d</div></div>`+
			`<div><div class="k">admin sessions</div><div class="mono">%d</div></div>`+
			`<div><div class="k">stream</div><div class="mono" style="color:var(--ok)">live · 1s</div></div>`+
			`</div>`,
		s.inflight.Load(), time.Since(s.start).Round(time.Second).String(),
		m.HeapAlloc>>20, m.HeapSys>>20, m.NumGC,
		humanBytes(held), rejected, s.sessions.count())
	return "event: health\ndata: " + strings.ReplaceAll(frag, "\n", " ") + "\n\n"
}

// ---------------------------------------------------------------------------
// #19 request log ring
// ---------------------------------------------------------------------------

// logEntry is one completed request in the #19 ring. Kind marks the
// no-route failure paths; code is the client-visible status.
type logEntry struct {
	Seq       int64  `json:"seq"`
	TS        int64  `json:"ts"` // unix seconds
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Code      int    `json:"code"`
	Kind      string `json:"kind,omitempty"` // "" ok | upstream_error | budget_saturated | no_route
	In        int64  `json:"in,omitempty"`
	Out       int64  `json:"out,omitempty"`
	CacheRead int64  `json:"cache_read,omitempty"`
	Saved     int64  `json:"saved,omitempty"`
	Err       string `json:"err,omitempty"`
}

type requestLog struct {
	mu   sync.Mutex
	ring []logEntry
	head int
	seq  int64
	next *sseHub // wired after hub construction
}

const logRingCap = 512

func newRequestLog() *requestLog {
	return &requestLog{ring: make([]logEntry, logRingCap)}
}

// record appends one entry and pushes it to live `logs` subscribers.
func (l *requestLog) record(e logEntry) {
	l.mu.Lock()
	l.seq++
	e.Seq = l.seq
	l.ring[l.head] = e
	l.head = (l.head + 1) % len(l.ring)
	l.mu.Unlock()
	if b, err := json.Marshal(e); err == nil && l.next != nil {
		l.next.publish("logs", string(b))
	}
}

// latest returns up to n most recent entries, oldest first.
func (l *requestLog) latest(n int) []logEntry {
	if n > logRingCap {
		n = logRingCap
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]logEntry, 0, n)
	for i := range n {
		idx := (l.head - 1 - i + len(l.ring)) % len(l.ring)
		e := l.ring[idx]
		if e.Seq == 0 {
			break
		}
		out = append(out, e)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// handleAPILogs answers GET /admin/api/v1/logs?limit=N (newest last).
func (s *Server) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	n := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 && x <= logRingCap {
			n = x
		}
	}
	writeJSON(w, map[string]any{"entries": s.reqlog.latest(n)})
}

// observeLog is the single hook the proxy paths call on completion.
func (s *Server) observeLog(provider, model string, code int, kind string, u types.Usage, saved int64, errMsg string) {
	if s.reqlog == nil {
		return
	}
	s.reqlog.record(logEntry{
		TS: time.Now().Unix(), Model: model, Provider: provider, Code: code, Kind: kind,
		In: u.InputTokens, Out: u.OutputTokens, CacheRead: u.CacheReadTokens, Saved: saved, Err: errMsg,
	})
}

// ---------------------------------------------------------------------------
// Page handlers
// ---------------------------------------------------------------------------

var navItems = []dashboard.NavItem{
	{ID: "overview", Href: "/admin", Label: "Overview"},
	{ID: "usage", Href: "/admin/ui/usage", Label: "Usage"},
	{ID: "providers", Href: "/admin/ui/providers", Label: "Providers"},
	{ID: "combos", Href: "/admin/ui/combos", Label: "Combos"},
	{ID: "quota", Href: "/admin/ui/quota", Label: "Quota"},
	{ID: "saver", Href: "/admin/ui/saver", Label: "Token Saver"},
	{ID: "logs", Href: "/admin/ui/logs", Label: "Console Log"},
	{ID: "tools", Href: "/admin/ui/tools", Label: "CLI Tools"},
	{ID: "settings", Href: "/admin/ui/settings", Label: "Settings"},
}

// authedPage renders a dashboard page after the gate; unauthenticated
// browsers get the login page instead of a bare 401.
func (s *Server) authedPage(w http.ResponseWriter, r *http.Request, id, title string, live bool, v any) {
	if !s.adminOK(r) {
		s.renderLogin(w, "")
		return
	}
	out, err := dashboard.Render(id, dashboard.Shell{
		Title: title, Active: id, Live: live, Nav: navItems, V: v,
	})
	if err != nil {
		http.Error(w, "render error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(out))
}

// handleAdminPage serves GET /admin (Overview).
func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		s.renderLogin(w, "")
		return
	}
	s.authedPage(w, r, "overview", "Overview", true, s.overviewView())
}

type overviewData struct {
	TodayReq, TodayIn, TodayOut, TodaySaved int64
	Providers, Combos, Models               int
	QuotaExhausted                          int
	Budget, BudgetCap                       string
	BudgetRejected, BudgetWaiting           int64
	BudgetPct                               int
	HeapAlloc, HeapSys                      uint64
	NumGC                                   uint32
	LiveJSON                                string
}

// overviewView assembles the Overview page data.
func (s *Server) overviewView() *overviewData {
	v := &overviewData{}
	st := s.cur()
	today := time.Now().UTC().Format("2006-01-02")
	if s.st != nil {
		if rows, err := s.st.QueryRange(today, today); err == nil {
			for _, r := range rows {
				v.TodayReq += r.Requests
				v.TodayIn += r.InputTok
				v.TodayOut += r.OutputTok
				v.TodaySaved += r.SavedTok
			}
		}
	}
	if st != nil {
		v.Providers = len(st.cfg.Providers)
		v.Combos = len(st.cfg.Combos)
		for _, p := range st.cfg.Providers {
			v.Models += len(p.Models)
		}
		if q := st.quota; q != nil {
			for _, qs := range q.All(time.Now()) {
				if qs.Exhausted {
					v.QuotaExhausted++
				}
			}
		}
		if st.budget != nil {
			held, rejected := st.budget.Stats()
			capB := st.budget.Capacity()
			v.Budget = humanBytes(held)
			v.BudgetCap = humanBytes(capB)
			v.BudgetRejected = rejected
			v.BudgetWaiting = st.budget.Waiting()
			if capB > 0 {
				v.BudgetPct = int(held * 100 / capB)
			}
		}
	}
	// No-JS fallback text; the SSE swap replaces it with the live strip.
	v.LiveJSON = "live metrics need JavaScript (SSE /admin/events)"
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	v.HeapAlloc = m.HeapAlloc >> 20
	v.HeapSys = m.HeapSys >> 20
	v.NumGC = m.NumGC
	return v
}

// handleAdminUI dispatches GET /admin/ui/{page}.
func (s *Server) handleAdminUI(w http.ResponseWriter, r *http.Request) {
	page := strings.TrimPrefix(r.URL.Path, "/admin/ui/")
	switch page {
	case "usage":
		s.usagePage(w, r)
	case "providers":
		s.providersPage(w, r)
	case "combos":
		s.combosPage(w, r)
	case "quota":
		s.quotaPage(w, r)
	case "saver":
		s.saverPage(w, r)
	case "logs":
		s.logsPage(w, r)
	case "tools":
		s.toolsPage(w, r)
	case "settings":
		s.settingsPage(w, r)
	default:
		http.NotFound(w, r)
	}
}

// ---------------------------------------------------------------------------
// Usage page + daily-series API (cursor pagination)
// ---------------------------------------------------------------------------

type usageRange struct {
	ID, Label string
	On        bool
}

type usageRowView struct {
	Provider, Model              string
	Req, In, Out                 int64
	CacheRead, CacheWrite, Saved int64
}

type usageView struct {
	Totals    struct{ Req, In, Out, CacheRead, Saved int64 }
	Ranges    []usageRange
	Window    string
	Rows      []usageRowView
	HasCharts bool
	ChartJSON template.JS
	From, To  string
}

var usageRanges = []struct {
	ID    string
	Label string
	Days  int
}{
	{"today", "Today", 0},
	{"7d", "7 days", 7},
	{"1m", "1 month", 31},
	{"all", "All time", 3650},
}

// rangeWindow resolves ?range= to a from/to day pair ("all" has no lower
// bound: from = "" queries everything stored).
func rangeWindow(sel string) (from, to string) {
	to = time.Now().UTC().Format("2006-01-02")
	switch sel {
	case "today":
		return to, to
	case "7d":
		return time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02"), to
	case "1m":
		return time.Now().UTC().AddDate(0, 0, -31).Format("2006-01-02"), to
	default:
		return "", to
	}
}

func (s *Server) usagePage(w http.ResponseWriter, r *http.Request) {
	sel := r.URL.Query().Get("range")
	if sel == "" {
		sel = "all"
	}
	from, to := rangeWindow(sel)
	v := &usageView{From: from, To: to}
	for _, rg := range usageRanges {
		v.Ranges = append(v.Ranges, usageRange{ID: rg.ID, Label: rg.Label, On: rg.ID == sel})
	}
	switch sel {
	case "today":
		v.Window = "UTC day " + to
	case "7d", "1m":
		v.Window = from + " → " + to
	default:
		v.Window = "all stored rollups"
	}
	rows, err := s.usageRows(from, to)
	if err == nil {
		v.Rows = rows
		for _, r2 := range rows {
			v.Totals.Req += r2.Req
			v.Totals.In += r2.In
			v.Totals.Out += r2.Out
			v.Totals.CacheRead += r2.CacheRead
			v.Totals.Saved += r2.Saved
		}
	}
	// Charts render for bounded windows; "all time" would need the whole
	// history on one x-axis — the table covers that view.
	v.HasCharts = sel == "7d" || sel == "1m"
	if v.HasCharts {
		v.ChartJSON = template.JS(s.chartJSON(from, to))
	}
	s.authedPage(w, r, "usage", "Usage", false, v)
}

// usageRows aggregates store rollups to one row per provider+model.
func (s *Server) usageRows(from, to string) ([]usageRowView, error) {
	if s.st == nil {
		return nil, nil
	}
	raw, err := s.st.QueryRange(from, to)
	if err != nil {
		return nil, err
	}
	type key struct{ p, m string }
	agg := map[key]*usageRowView{}
	for _, r := range raw {
		k := key{r.Provider, r.Model}
		v := agg[k]
		if v == nil {
			v = &usageRowView{Provider: r.Provider, Model: r.Model}
			agg[k] = v
		}
		v.Req += r.Requests
		v.In += r.InputTok
		v.Out += r.OutputTok
		v.CacheRead += r.CacheRead
		v.CacheWrite += r.CacheWrite
		v.Saved += r.SavedTok
	}
	out := make([]usageRowView, 0, len(agg))
	for _, v := range agg {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Req != out[j].Req {
			return out[i].Req > out[j].Req
		}
		return out[i].Provider+out[i].Model < out[j].Provider+out[j].Model
	})
	return out, nil
}

// chartJSON builds the uPlot dataset: dense day axis + per-day sums.
func (s *Server) chartJSON(from, to string) string {
	days := map[string][4]int64{} // day → [req, in, out, cache_read]
	if s.st != nil {
		if raw, err := s.st.QueryRange(from, to); err == nil {
			for _, r := range raw {
				d := days[r.Day]
				d[0] += r.Requests
				d[1] += r.InputTok
				d[2] += r.OutputTok
				d[3] += r.CacheRead
				days[r.Day] = d
			}
		}
	}
	var ds []string
	for d := from; d <= to; {
		ds = append(ds, d)
		t, err := time.Parse("2006-01-02", d)
		if err != nil {
			break
		}
		d = t.AddDate(0, 0, 1).Format("2006-01-02")
	}
	if len(ds) > 62 {
		ds = ds[len(ds)-62:]
	}
	type chartData struct {
		Days      []string `json:"days"`
		Requests  []int64  `json:"requests"`
		Input     []int64  `json:"input"`
		Output    []int64  `json:"output"`
		CacheRead []int64  `json:"cache_read"`
	}
	cd := chartData{
		Days:     ds,
		Requests: make([]int64, len(ds)), Input: make([]int64, len(ds)),
		Output: make([]int64, len(ds)), CacheRead: make([]int64, len(ds)),
	}
	for i, d := range ds {
		v := days[d]
		cd.Requests[i], cd.Input[i], cd.Output[i], cd.CacheRead[i] = v[0], v[1], v[2], v[3]
	}
	b, _ := json.Marshal(cd)
	return string(b)
}

// handleAPIUsageDaily answers GET /admin/api/v1/usage/daily?from&to&cursor
// — cursor = last day returned (rollups are day-keyed; offset pagination
// breaks as data shifts). One page = 31 distinct days.
func (s *Server) handleAPIUsageDaily(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	q := r.URL.Query()
	to := q.Get("to")
	if to == "" {
		to = time.Now().UTC().Format("2006-01-02")
	}
	from := q.Get("from")
	if from == "" {
		from = time.Now().UTC().AddDate(0, 0, -3650).Format("2006-01-02")
	}
	if cursor := q.Get("cursor"); cursor > from && cursor <= to {
		if t, err := time.Parse("2006-01-02", cursor); err == nil {
			from = t.AddDate(0, 0, 1).Format("2006-01-02")
		}
	}
	if s.st == nil {
		writeJSON(w, map[string]any{"days": []string{}, "rows": []store.UsageRow{}, "next_cursor": nil})
		return
	}
	raw, err := s.st.QueryRange(from, to)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store query failed")
		return
	}
	if raw == nil {
		raw = []store.UsageRow{}
	}
	seen := map[string]bool{}
	var ds []string
	for _, r2 := range raw {
		if !seen[r2.Day] {
			seen[r2.Day] = true
			ds = append(ds, r2.Day)
		}
	}
	sort.Strings(ds)
	const pageSize = 31
	var next any
	if len(ds) > pageSize {
		ds = ds[:pageSize]
		next = ds[len(ds)-1]
		paged := map[string]bool{}
		for _, d := range ds {
			paged[d] = true
		}
		kept := raw[:0]
		for _, r2 := range raw {
			if paged[r2.Day] {
				kept = append(kept, r2)
			}
		}
		raw = kept
	}
	writeJSON(w, map[string]any{"days": ds, "rows": raw, "next_cursor": next})
}

// ---------------------------------------------------------------------------
// Read-only config views + their JSON twins
// ---------------------------------------------------------------------------

type providerView struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	BaseURL     string   `json:"base_url"`
	Accounts    int      `json:"accounts"`
	Concurrency string   `json:"concurrency"`
	Sticky      string   `json:"sticky,omitempty"`
	Quota       string   `json:"quota,omitempty"`
	QuotaLimit  string   `json:"quota_limit,omitempty"`
	Models      []string `json:"models,omitempty"`
}

func providerViews(st *state) []providerView {
	out := make([]providerView, 0, len(st.cfg.Providers))
	for _, p := range st.cfg.Providers {
		n := len(p.Accounts)
		if n == 0 {
			n = len(p.Keys)
			if n == 0 && p.APIKey != "" {
				n = 1
			}
		}
		v := providerView{
			Name: p.Name, Kind: p.Kind, BaseURL: p.BaseURL,
			Accounts: n, Models: p.Models, Sticky: p.Sticky,
		}
		if p.MaxConc > 0 {
			v.Concurrency = fmt.Sprintf("max %d concurrent", p.MaxConc)
		} else {
			v.Concurrency = "unbounded"
		}
		if p.QuotaWindow != "" {
			v.Quota = p.QuotaWindow
			if p.QuotaLimitTokens > 0 {
				v.QuotaLimit = dashboard.Compact(p.QuotaLimitTokens) + " tok"
			}
			if p.QuotaLimitRequests > 0 {
				v.QuotaLimit += " · " + dashboard.Compact(p.QuotaLimitRequests) + " req"
			}
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) providersPage(w http.ResponseWriter, r *http.Request) {
	var views []providerView
	if st := s.cur(); st != nil {
		views = providerViews(st)
	}
	s.authedPage(w, r, "providers", "Providers", false, views)
}

func (s *Server) handleAPIProviders(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var views []providerView
	if st := s.cur(); st != nil {
		views = providerViews(st)
	}
	writeJSON(w, views)
}

func (s *Server) combosPage(w http.ResponseWriter, r *http.Request) {
	type combo struct{ Name, Targets string }
	var out []combo
	if st := s.cur(); st != nil {
		for _, c := range st.cfg.Combos {
			out = append(out, combo{Name: c.Name, Targets: strings.Join(c.Targets, "  →  ")})
		}
	}
	s.authedPage(w, r, "combos", "Combos", false, out)
}

func (s *Server) handleAPICombos(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	type combo struct {
		Name    string   `json:"name"`
		Targets []string `json:"targets"`
	}
	out := []combo{}
	if st := s.cur(); st != nil {
		for _, c := range st.cfg.Combos {
			out = append(out, combo{Name: c.Name, Targets: c.Targets})
		}
	}
	writeJSON(w, out)
}

type quotaRowView struct {
	Provider, Window, ResetIn   string
	UsedTokens, LimitTokens     int64
	UsedRequests, LimitRequests int64
	WindowStart, WindowEnd      string
	Pct                         int
	BarClass                    string
	Exhausted                   bool
}

func quotaViews(st *state) []quotaRowView {
	out := []quotaRowView{}
	if st == nil || st.quota == nil {
		return out
	}
	now := time.Now()
	for _, q := range st.quota.All(now) {
		v := quotaRowView{
			Provider: q.Provider, Window: q.Window,
			UsedTokens: q.UsedTokens, LimitTokens: q.LimitTokens,
			UsedRequests: q.UsedRequests, LimitRequests: q.LimitRequests,
			Exhausted: q.Exhausted,
		}
		if !q.WindowEnd.IsZero() {
			d := q.WindowEnd.Sub(now).Round(time.Second)
			switch {
			case d > 48*time.Hour:
				v.ResetIn = fmt.Sprintf("%.1f d", d.Hours()/24)
			case d > 2*time.Hour:
				v.ResetIn = fmt.Sprintf("%.1f h", d.Hours())
			default:
				v.ResetIn = d.String()
			}
			v.WindowStart = q.WindowStart.Format(time.RFC3339)
			v.WindowEnd = q.WindowEnd.Format(time.RFC3339)
		}
		if q.LimitTokens > 0 {
			v.Pct = int(q.UsedTokens * 100 / q.LimitTokens)
			switch {
			case v.Pct >= 100:
				v.BarClass = "err"
			case v.Pct >= 80:
				v.BarClass = "warn"
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

func (s *Server) quotaPage(w http.ResponseWriter, r *http.Request) {
	s.authedPage(w, r, "quota", "Quota", false, quotaViews(s.cur()))
}

func (s *Server) handleAPIQuota(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, quotaViews(s.cur()))
}

type saverInjectView struct {
	Mode, Models, Text string
}

type saverExternalView struct {
	Enabled  bool
	URL      string
	MinBytes int
	FailOpen string
}

type saverView struct {
	Enabled    bool
	SavedTotal int64
	External   saverExternalView
	Inject     []saverInjectView
}

func (s *Server) saverPage(w http.ResponseWriter, r *http.Request) {
	v := &saverView{}
	if st := s.cur(); st != nil {
		v.Enabled = st.cfg.Saver.Enabled
		v.External.Enabled = st.cfg.Saver.External.Enabled
		v.External.URL = st.cfg.Saver.External.URL
		v.External.MinBytes = st.cfg.Saver.External.MinBytes
		if st.cfg.Saver.External.FailOpen == nil || *st.cfg.Saver.External.FailOpen {
			v.External.FailOpen = "fail-open"
		} else {
			v.External.FailOpen = "fail-closed"
		}
		for _, in := range st.cfg.Saver.Inject {
			v.Inject = append(v.Inject, saverInjectView{
				Mode: in.Mode, Models: strings.Join(in.Models, ", "), Text: in.Text,
			})
		}
	}
	v.SavedTotal = s.savedAllTime()
	s.authedPage(w, r, "saver", "Token Saver", false, v)
}

func (s *Server) handleAPISaver(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	out := map[string]any{"saved_tokens_all_time": s.savedAllTime()}
	if st := s.cur(); st != nil {
		out["enabled"] = st.cfg.Saver.Enabled
		out["inject_rules"] = len(st.cfg.Saver.Inject)
		out["external"] = st.cfg.Saver.External
	}
	writeJSON(w, out)
}

func (s *Server) savedAllTime() int64 {
	var saved int64
	if s.st != nil {
		if rows, err := s.st.QueryRange("0001-01-01", time.Now().UTC().Format("2006-01-02")); err == nil {
			for _, r := range rows {
				saved += r.SavedTok
			}
		}
	}
	return saved
}

// ---------------------------------------------------------------------------
// Logs page + CLI Tools + Settings
// ---------------------------------------------------------------------------

func (s *Server) logsPage(w http.ResponseWriter, r *http.Request) {
	s.authedPage(w, r, "logs", "Console Log", false, map[string]any{"Cap": logRingCap})
}

type toolsPreset struct{ ID, Name, Code string }

type toolsView struct {
	BaseURL string
	Presets []toolsPreset
}

func (s *Server) toolsPage(w http.ResponseWriter, r *http.Request) {
	listen := ":8080"
	if st := s.cur(); st != nil {
		listen = st.cfg.Server.Listen
	}
	v := &toolsView{BaseURL: "http://" + listenHost(listen)}
	add := func(id, name, code string) { v.Presets = append(v.Presets, toolsPreset{id, name, code}) }
	base := v.BaseURL
	add("openai", "OpenAI-compatible CLIs (Codex, Aider, …)",
		"export OPENAI_BASE_URL="+base+"/v1\nexport OPENAI_API_KEY=$ONEGW_KEY\nmodel: use \"provider/model\" or a combo name")
	add("anthropic", "Anthropic-compatible CLIs (Claude Code, …)",
		"export ANTHROPIC_BASE_URL="+base+"\nexport ANTHROPIC_AUTH_KEY=$ONEGW_KEY\nmodel: use \"provider/model\" or a combo name")
	add("gemini", "Gemini-compatible tools",
		"export GEMINI_API_BASE="+base+"\nmodel: provider/model")
	add("env", "Shared env",
		"export ONEGW_KEY=<one of auth.keys from onegw.toml>")
	s.authedPage(w, r, "tools", "CLI Tools", false, v)
}

type settingsView struct {
	PID         int
	Build       string
	StartedAt   string
	Uptime      string
	Listen      string
	ConfigPath  string
	ConfigMtime string
	DataDir     string
	AdminSet    bool
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	v := settingsView{PID: os.Getpid(), StartedAt: s.start.Format(time.RFC3339)}
	v.Uptime = time.Since(s.start).Round(time.Second).String()
	if st := s.cur(); st != nil {
		v.AdminSet = st.cfg.Server.AdminPassword != ""
		v.Listen = st.cfg.Server.Listen
	}
	v.DataDir = s.dataDir
	if o := s.owner.Load(); o != nil {
		v.Build = o.Build.Revision
		if v.Build == "" {
			v.Build = o.Build.ModuleVersion
		}
		if o.Build.GoVersion != "" {
			v.Build += " · " + o.Build.GoVersion
		}
		if v.Listen == "" {
			v.Listen = o.Listen
		}
		v.ConfigPath = o.ConfigPath
		v.ConfigMtime = o.ConfigMtime
	}
	s.authedPage(w, r, "settings", "Settings", false, v)
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func listenHost(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		h := listen[:i]
		if h == "" || h == "0.0.0.0" || h == "::" {
			return "127.0.0.1"
		}
		return h
	}
	return "127.0.0.1"
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": http.StatusText(status), "message": msg}})
}
