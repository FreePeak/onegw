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

	"onegw/internal/config"
	"onegw/internal/provider"
	"onegw/internal/server/dashboard"
	"onegw/internal/store"
	"onegw/internal/types"
	"onegw/internal/update"
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
		`<div class="metric"><div class="k">in-flight</div><div class="v hi">%d</div></div>`+
			`<div class="metric"><div class="k">uptime</div><div class="v">%s</div></div>`+
			`<div class="metric"><div class="k">heap alloc / sys</div><div class="v">%d / %d MiB</div></div>`+
			`<div class="metric"><div class="k">GC cycles</div><div class="v">%d</div></div>`+
			`<div class="metric"><div class="k">budget held</div><div class="v">%s</div></div>`+
			`<div class="metric"><div class="k">budget 503s</div><div class="v">%d</div></div>`+
			`<div class="metric"><div class="k">admin sessions</div><div class="v">%d</div></div>`+
			`<div class="metric"><div class="k">stream</div><div class="v text-ok">live · 1s</div></div>`,
		s.inflight.Load(), time.Since(s.start).Round(time.Second).String(),
		m.HeapAlloc>>20, m.HeapSys>>20, m.NumGC,
		humanBytes(held), rejected, s.sessions.count())
	return "event: health\ndata: " + strings.ReplaceAll(frag, "\n", " ") + "\n\n"
}

// ---------------------------------------------------------------------------
// #19 request log ring
// ---------------------------------------------------------------------------

// logEntry is one completed request in the #19 ring. Kind marks the
// no-route failure paths; code is the client-visible status. Account is
// the provider account (key name) the attempt ran on — empty when the
// request never got as far as an account pick (no_route, saturated).
type logEntry struct {
	Seq       int64  `json:"seq"`
	TS        int64  `json:"ts"` // unix seconds
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Account   string `json:"account,omitempty"`
	Code      int    `json:"code"`
	Kind      string `json:"kind,omitempty"` // "" ok | upstream_error | budget_saturated | no_route
	In        int64  `json:"in,omitempty"`
	Out       int64  `json:"out,omitempty"`
	CacheRead int64  `json:"cache_read,omitempty"`
	Saved     int64  `json:"saved,omitempty"`
	Err       string `json:"err,omitempty"`
	// Decode phase of the serving attempt: duration in ms and output
	// tokens/sec (0 when unknown — failures, synthetic replies).
	Ms  int64   `json:"ms,omitempty"`
	Tps float64 `json:"tps,omitempty"`
	// End-to-end client view of the winning attempt: whole-request wall in
	// ms and delivered tok/s (0 when the request had no delivery context).
	E2EMs int64   `json:"e2e_ms,omitempty"`
	DTps  float64 `json:"dtps,omitempty"`
}

type requestLog struct {
	mu   sync.Mutex
	ring []logEntry
	head int
	seq  int64
	next *sseHub // wired after hub construction
}

const logRingCap = 512

// logMaxAge is how long a request-log entry stays visible: entries older
// than 7 days are dropped at read time (API + initial page load). The
// ring already bounds memory at 512 entries; the age window keeps a
// long-idle gateway from serving week-old rows as if they were current.
const logMaxAge = 7 * 24 * time.Hour

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

// latest returns up to n most recent entries, oldest first. Entries older
// than logMaxAge are treated as cleared: the scan walks newest→oldest, so
// the first stale entry ends it.
func (l *requestLog) latest(n int) []logEntry {
	if n > logRingCap {
		n = logRingCap
	}
	cutoff := time.Now().Add(-logMaxAge).Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]logEntry, 0, n)
	for i := range n {
		idx := (l.head - 1 - i + len(l.ring)) % len(l.ring)
		e := l.ring[idx]
		if e.Seq == 0 {
			break
		}
		if e.TS < cutoff {
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
// ms/tps carry the decode phase's duration and tokens/sec (0 = unknown).
func (s *Server) observeLog(provider, model, acct string, code int, kind string, u types.Usage, saved int64, errMsg string, ms int64, tps float64, e2eMs int64, dtps float64) {
	if s.reqlog == nil {
		return
	}
	s.reqlog.record(logEntry{
		TS: time.Now().Unix(), Model: model, Provider: provider, Account: acct, Code: code, Kind: kind,
		In: u.InputTokens, Out: u.OutputTokens, CacheRead: u.CacheReadTokens, Saved: saved, Err: errMsg,
		Ms: ms, Tps: tps, E2EMs: e2eMs, DTps: dtps,
	})
}

// navItems — the sidebar IA (#41). Icon is the glyph's inner SVG markup:
// 24x24 viewBox, stroke=currentColor paths drawn by base.html inside a
// <svg class="nicon"> wrapper. Geometry follows the Lucide icon language
// (MIT): gauge (overview), bar chart (usage), list (logs), server rack
// (providers), route (combos failover), pie (quota), banknote (saver),
// terminal (tools), sliders (settings). Collapsed to the icon rail these
// glyphs are the only navigation, so each must read at 17px.
var navItems = []dashboard.NavItem{
	{ID: "overview", Href: "/admin", Label: "Overview", Group: "Monitor",
		Icon: `<path d="m19 15-4-4"/><path d="M21.64 15a9 9 0 1 0-19.28 0"/>`},
	{ID: "usage", Href: "/admin/ui/usage", Label: "Usage", Group: "Monitor",
		Icon: `<path d="M3 3v18h18"/><path d="M18 17V9"/><path d="M13 17V5"/><path d="M8 17v-3"/>`},
	{ID: "logs", Href: "/admin/ui/logs", Label: "Console Log", Group: "Monitor",
		Icon: `<path d="M8 6h13"/><path d="M8 12h13"/><path d="M8 18h13"/><path d="M3.5 6h.01"/><path d="M3.5 12h.01"/><path d="M3.5 18h.01"/>`},
	{ID: "providers", Href: "/admin/ui/providers", Label: "Providers", Group: "Routing",
		Icon: `<rect width="20" height="8" x="2" y="2" rx="2"/><rect width="20" height="8" x="2" y="14" rx="2"/><path d="M6 6h.01"/><path d="M6 18h.01"/>`},
	{ID: "combos", Href: "/admin/ui/combos", Label: "Combos", Group: "Routing",
		Icon: `<circle cx="6" cy="19" r="3"/><path d="M9 19h8.5a3.5 3.5 0 0 0 0-7h-11a3.5 3.5 0 0 1 0-7H15"/><circle cx="18" cy="5" r="3"/>`},
	{ID: "quota", Href: "/admin/ui/quota", Label: "Quota", Group: "Routing",
		Icon: `<path d="M21.21 15.89A10 10 0 1 1 8 2.83"/><path d="M22 12A10 10 0 0 0 12 2v10z"/>`},
	{ID: "saver", Href: "/admin/ui/saver", Label: "Token Saver", Group: "Routing",
		Icon: `<rect width="20" height="12" x="2" y="6" rx="2"/><circle cx="12" cy="12" r="2"/><path d="M6 12h.01"/><path d="M18 12h.01"/>`},
	{ID: "tools", Href: "/admin/ui/tools", Label: "CLI Tools", Group: "Gateway",
		Icon: `<path d="m4 17 6-6-6-6"/><path d="M12 19h8"/>`},
	{ID: "settings", Href: "/admin/ui/settings", Label: "Settings", Group: "Gateway",
		Icon: `<line x1="4" x2="4" y1="21" y2="14"/><line x1="4" x2="4" y1="10" y2="3"/><line x1="12" x2="12" y1="21" y2="12"/><line x1="12" x2="12" y1="8" y2="3"/><line x1="20" x2="20" y1="21" y2="16"/><line x1="20" x2="20" y1="12" y2="3"/><line x1="2" x2="6" y1="14" y2="14"/><line x1="10" x2="14" y1="8" y2="8"/><line x1="18" x2="22" y1="16" y2="16"/>`},
}

// authedPage renders a dashboard page after the gate; unauthenticated
// browsers get the login page instead of a bare 401.
func (s *Server) authedPage(w http.ResponseWriter, r *http.Request, id, title string, live bool, v any) {
	if !s.adminOK(r) {
		s.renderLogin(w, "")
		return
	}
	out, err := dashboard.Render(id, dashboard.Shell{
		Title: title, Active: id, Live: live, Nav: navItems, Ranks: s.rankRows(), V: v,
	})
	if err != nil {
		http.Error(w, "render error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(out))
}

// rankRows aggregates provider request counts over the trailing 7 local
// days for the shell's right-rail ranking. Read-only; empty when the store
// is unavailable.
func (s *Server) rankRows() []dashboard.RankRow {
	if s.st == nil {
		return nil
	}
	to := time.Now().Format("2006-01-02")
	from := time.Now().AddDate(0, 0, -6).Format("2006-01-02")
	raw, err := s.rowsInLocalWindow(from, to)
	if err != nil {
		return nil
	}
	type agg struct{ req, tok int64 }
	byProv := map[string]*agg{}
	for _, r := range raw {
		name := r.Provider
		if name == "" {
			name = "unresolved"
		}
		a := byProv[name]
		if a == nil {
			a = &agg{}
			byProv[name] = a
		}
		a.req += r.Requests
		a.tok += r.InputTok + r.OutputTok
	}
	rows := make([]dashboard.RankRow, 0, len(byProv))
	for name, a := range byProv {
		rows = append(rows, dashboard.RankRow{Name: name, Req: a.req, Tok: a.tok})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Req > rows[j].Req })
	const maxRows = 4
	if len(rows) > maxRows {
		rows = rows[:maxRows]
	}
	for i := range rows {
		rows[i].Rank = i + 1
		rows[i].Featured = i == 0
	}
	return rows
}

// ---------------------------------------------------------------------------
// Local-time dashboard windows
//
// The dashboard presents every usage window in the gateway host's LOCAL
// calendar, while the rollup store keys rows by UTC day+hour. A local day
// straddles two UTC days (UTC+7 midnight = 17:00 of the previous UTC day),
// so each local window maps onto a UTC key superset and rows are
// re-filtered by the local day their rollup instant lands on.
// ---------------------------------------------------------------------------

// rollupInstant parses a stored (day, hour) rollup key as the UTC instant
// it represents. ok=false for odd/legacy keys; callers fall back to the
// raw key as the display label.
func rollupInstant(day, hour string) (time.Time, bool) {
	if hour == "" {
		t, err := time.ParseInLocation("2006-01-02", day, time.UTC)
		return t, err == nil
	}
	t, err := time.ParseInLocation("2006-01-02 15", day+" "+hour, time.UTC)
	return t, err == nil
}

// utcKeyWindow maps an inclusive local-day window onto the range of UTC
// day keys that can contain its rollups: the bounds are the UTC dates of
// the window's local-midnight instants.
func utcKeyWindow(from, to string) (string, string) {
	start, err1 := time.ParseInLocation("2006-01-02", from, time.Local)
	end, err2 := time.ParseInLocation("2006-01-02", to, time.Local)
	if err2 != nil {
		return from, to
	}
	kTo := end.AddDate(0, 0, 1).Add(-time.Second).UTC().Format("2006-01-02")
	if err1 != nil {
		return "", kTo
	}
	return start.UTC().Format("2006-01-02"), kTo
}

// rowsInLocalWindow queries the rollup store for the UTC key superset
// covering the inclusive LOCAL-day window [from, to] and keeps only rows
// whose rollup instant falls on a local day inside it. from == "" queries
// the whole history (the upper bound still applies).
func (s *Server) rowsInLocalWindow(from, to string) ([]store.UsageRow, error) {
	if s.st == nil {
		return nil, nil
	}
	kFrom, kTo := utcKeyWindow(from, to)
	raw, err := s.st.QueryRange(kFrom, kTo)
	if err != nil {
		return nil, err
	}
	out := raw[:0]
	for _, r := range raw {
		ld := r.Day
		if inst, ok := rollupInstant(r.Day, r.Hour); ok {
			ld = inst.In(time.Local).Format("2006-01-02")
		}
		if from != "" && ld < from {
			continue
		}
		if ld <= to {
			out = append(out, r)
		}
	}
	return out, nil
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
	ChartJSON                               template.JS // today's hourly token chart
	// TPS is the per-provider decode-speed ranking (tokens/sec EWMA,
	// fastest first); TPSTotal is the whole-gateway average.
	TPS      []provider.SpeedRow
	TPSTotal float64
}

// overviewView assembles the Overview page data.
func (s *Server) overviewView() *overviewData {
	v := &overviewData{}
	st := s.cur()
	today := time.Now().Format("2006-01-02") // local calendar day, like every dashboard window
	if rows, err := s.rowsInLocalWindow(today, today); err == nil {
		for _, r := range rows {
			v.TodayReq += r.Requests
			v.TodayIn += r.InputTok
			v.TodayOut += r.OutputTok
			v.TodaySaved += r.SavedTok
		}
	}
	if st != nil {
		v.Providers = len(st.cfg.Providers)
		v.Combos = len(st.cfg.Combos)
		for _, p := range st.cfg.Providers {
			v.Models += len(p.Models)
		}
		// today's hourly token series — the Overview chart card (M.O.N.K.Y
		// puts a chart on the main page; the usage page covers wider ranges).
		v.ChartJSON = template.JS(s.chartJSON(today, today))
		if q := st.quota; q != nil {
			for _, qs := range q.All(time.Now()) {
				if qs.Exhausted {
					v.QuotaExhausted++
				}
			}
		}
		// Decode-speed ranking: providers with samples, fastest first —
		// the Overview throughput card.
		for _, name := range st.pool.Names() {
			if d, ok := st.pool.Get(name); ok && !d.Disabled {
				if tps := d.ProviderTPS(); tps > 0 {
					v.TPS = append(v.TPS, provider.SpeedRow{Account: name, TPS: tps, Samples: d.SpeedSamples()})
				}
			}
		}
		sort.Slice(v.TPS, func(i, j int) bool { return v.TPS[i].TPS > v.TPS[j].TPS })
		var wsum, w float64
		for _, r := range v.TPS {
			wsum += r.TPS
			w++
		}
		if w > 0 {
			v.TPSTotal = wsum / w
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
	ChartUnit string
	ChartJSON template.JS
	From, To  string // local-day labels (display)
	// Export window: the raw rollup store is UTC-day-keyed, so the CSV
	// export link carries the UTC key superset of the local window.
	ExportFrom, ExportTo string
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

// rangeWindow resolves ?range= to a from/to LOCAL-day pair ("all" has no
// lower bound: from = "" queries everything stored).
func rangeWindow(sel string) (from, to string) {
	to = time.Now().Format("2006-01-02")
	switch sel {
	case "today":
		return to, to
	case "7d":
		return time.Now().AddDate(0, 0, -7).Format("2006-01-02"), to
	case "1m":
		return time.Now().AddDate(0, 0, -31).Format("2006-01-02"), to
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
	// Raw export API is UTC-day-keyed: hand it the UTC key superset.
	v.ExportFrom, v.ExportTo = utcKeyWindow(from, to)
	for _, rg := range usageRanges {
		v.Ranges = append(v.Ranges, usageRange{ID: rg.ID, Label: rg.Label, On: rg.ID == sel})
	}
	switch sel {
	case "today":
		v.Window = "local day " + to + " (" + time.Now().Format("MST") + ")"
	case "7d", "1m":
		v.Window = from + " → " + to + " (local)"
	default:
		v.Window = "all stored rollups"
	}
	rows, err := s.usageRowsLocal(from, to)
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
	// "today" charts hourly; bounded windows chart per day. "all time"
	// would need the whole history on one x-axis — the table covers that.
	v.HasCharts = sel != "all"
	if v.HasCharts {
		v.ChartJSON = template.JS(s.chartJSON(from, to))
	}
	v.ChartUnit = "per day"
	if from == to {
		v.ChartUnit = "per hour (local)"
	}
	s.authedPage(w, r, "usage", "Usage", false, v)
}

// usageRows aggregates the LOCAL-window rollups to one row per
// provider+model.
func (s *Server) usageRowsLocal(from, to string) ([]usageRowView, error) {
	if s.st == nil {
		return nil, nil
	}
	raw, err := s.rowsInLocalWindow(from, to)
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

// chartJSON builds the uPlot dataset on LOCAL-calendar axes: day mode
// buckets per local day, hour mode (single-day window) buckets per local
// hour of that local day. Rollup keys stay UTC day+hour — rows map to
// their local slot via the instant they represent.
func (s *Server) chartJSON(from, to string) string {
	// slot: local label for a rollup row. ok=false (odd legacy keys) falls
	// back to the raw key as the display label.
	slot := func(r store.UsageRow) (string, bool) {
		if from == to { // hour-of-the-day label
			if inst, ok := rollupInstant(r.Day, r.Hour); ok {
				return inst.In(time.Local).Format("15"), true
			}
			return r.Hour, false
		}
		if inst, ok := rollupInstant(r.Day, r.Hour); ok {
			return inst.In(time.Local).Format("2006-01-02"), true
		}
		return r.Day, false
	}
	type axis struct {
		Labels []string
		Keys   map[string][4]int64 // label → [req, in, out, cache_read]
	}
	var ax axis
	ax.Keys = map[string][4]int64{}
	if rows, err := s.rowsInLocalWindow(from, to); err == nil {
		for _, r := range rows {
			label, _ := slot(r)
			d := ax.Keys[label]
			d[0] += r.Requests
			d[1] += r.InputTok
			d[2] += r.OutputTok
			d[3] += r.CacheRead
			ax.Keys[label] = d
		}
	}
	if from == to { // dense 00..23 local-hour axis
		for h := range 24 {
			ax.Labels = append(ax.Labels, fmt.Sprintf("%02d", h))
		}
	} else { // dense local-day axis; cap the span
		for d := from; d <= to; {
			ax.Labels = append(ax.Labels, d)
			t, err := time.ParseInLocation("2006-01-02", d, time.Local)
			if err != nil {
				break
			}
			d = t.AddDate(0, 0, 1).Format("2006-01-02")
		}
		if len(ax.Labels) > 62 {
			ax.Labels = ax.Labels[len(ax.Labels)-62:]
		}
	}
	type chartData struct {
		Days      []string `json:"days"` // display labels
		Xs        []int64  `json:"xs"`   // epoch seconds — uPlot needs numeric x
		Requests  []int64  `json:"requests"`
		Input     []int64  `json:"input"`
		Output    []int64  `json:"output"`
		CacheRead []int64  `json:"cache_read"`
	}
	xs := make([]int64, len(ax.Labels))
	for i, d := range ax.Labels {
		var t time.Time
		if from == to { // hour label on the current local day
			h, _ := strconv.Atoi(d)
			t, _ = time.ParseInLocation("2006-01-02", from, time.Local)
			t = t.Add(time.Duration(h) * time.Hour)
		} else {
			t, _ = time.ParseInLocation("2006-01-02", d, time.Local)
		}
		xs[i] = t.Unix()
	}
	cd := chartData{
		Days:     ax.Labels,
		Xs:       xs,
		Requests: make([]int64, len(ax.Labels)), Input: make([]int64, len(ax.Labels)),
		Output: make([]int64, len(ax.Labels)), CacheRead: make([]int64, len(ax.Labels)),
	}
	for i, d := range ax.Labels {
		v := ax.Keys[d]
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
	Disabled    bool     `json:"disabled"`
	Concurrency string   `json:"concurrency"`
	Sticky      string   `json:"sticky,omitempty"`
	Quota       string   `json:"quota,omitempty"`
	QuotaLimit  string   `json:"quota_limit,omitempty"`
	Models      []string `json:"models,omitempty"`
	// Decode-speed EWMA (tokens/sec) and per-account breakdown; empty
	// until the provider has served streaming replies.
	TPS    float64             `json:"tps,omitempty"`
	Speeds []provider.SpeedRow `json:"speeds,omitempty"`
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
			Accounts: n, Models: p.Models, Sticky: p.Sticky, Disabled: p.Disabled,
		}
		if def, ok := st.pool.Get(p.Name); ok {
			v.TPS = def.ProviderTPS()
			v.Speeds = def.SpeedRows()
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

// providerEditView is the editor-prefill shape for the popup modal. It
// carries NO secret material: accounts expose has_key only, so an edit
// round-trip can never echo a key back into the file.
type providerEditView struct {
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`
	BaseURL     string         `json:"base_url,omitempty"`
	Models      []string       `json:"models,omitempty"`
	MaxConc     int            `json:"max_concurrency,omitempty"`
	Sticky      string         `json:"sticky,omitempty"`
	QuotaWindow string         `json:"quota_window,omitempty"`
	QuotaTokens int64          `json:"quota_limit_tokens,omitempty"`
	QuotaReqs   int64          `json:"quota_limit_requests,omitempty"`
	Accounts    []acctEditView `json:"accounts,omitempty"`
}

type acctEditView struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url,omitempty"`
	Weight  int    `json:"weight,omitempty"`
	RPM     int    `json:"rpm,omitempty"`
	HasKey  bool   `json:"has_key,omitempty"`
}

func providerEditViews(st *state) []providerEditView {
	out := make([]providerEditView, 0, len(st.cfg.Providers))
	for _, p := range st.cfg.Providers {
		v := providerEditView{
			Name: p.Name, Kind: p.Kind, BaseURL: p.BaseURL, Models: p.Models,
			MaxConc: p.MaxConc, Sticky: p.Sticky, QuotaWindow: p.QuotaWindow,
			QuotaTokens: p.QuotaLimitTokens, QuotaReqs: p.QuotaLimitRequests,
		}
		src := p.Accounts
		if len(src) == 0 {
			for i, k := range p.Keys {
				if k == "" {
					continue // env-provided; nothing to preserve or report
				}
				src = append(src, config.Acct{Name: fmt.Sprintf("key-%d", i+1), APIKey: k})
			}
			if len(src) == 0 && p.APIKey != "" {
				src = []config.Acct{{Name: "default", APIKey: p.APIKey}}
			}
		}
		for _, a := range src {
			v.Accounts = append(v.Accounts, acctEditView{
				Name: a.Name, BaseURL: a.BaseURL, Weight: a.Weight, RPM: a.RPM, HasKey: a.APIKey != "",
			})
		}
		out = append(out, v)
	}
	return out
}

type providersPageView struct {
	Views []providerView
	Edit  template.JS
}

func (s *Server) providersPage(w http.ResponseWriter, r *http.Request) {
	v := providersPageView{}
	if st := s.cur(); st != nil {
		v.Views = providerViews(st)
		edits := providerEditViews(st)
		if b, err := json.Marshal(edits); err == nil {
			v.Edit = template.JS(b)
		}
	}
	s.authedPage(w, r, "providers", "Providers", false, v)
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

type combosPageView struct {
	Rows []struct{ Name, Targets string }
	Edit template.JS
}

func (s *Server) combosPage(w http.ResponseWriter, r *http.Request) {
	var out []struct{ Name, Targets string }
	if st := s.cur(); st != nil {
		for _, c := range st.cfg.Combos {
			out = append(out, struct{ Name, Targets string }{c.Name, strings.Join(c.Targets, "  →  ")})
		}
	}
	v := combosPageView{Rows: out}
	if st := s.cur(); st != nil {
		type ce struct {
			Name    string   `json:"name"`
			Targets []string `json:"targets"`
		}
		edits := []ce{}
		for _, c := range st.cfg.Combos {
			edits = append(edits, ce{Name: c.Name, Targets: c.Targets})
		}
		if b, err := json.Marshal(edits); err == nil {
			v.Edit = template.JS(b)
		}
	}
	s.authedPage(w, r, "combos", "Combos", false, v)
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
		if rows, err := s.rowsInLocalWindow("", time.Now().Format("2006-01-02")); err == nil {
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

// toolsPage renders per-agent-CLI preset cards (9router's CLI Tools page).
// Snippets mirror the real config schemas of each tool; the bearer key is
// always the $ONEGW_KEY placeholder — real keys never render here.
func (s *Server) toolsPage(w http.ResponseWriter, r *http.Request) {
	listen := ":8080"
	if st := s.cur(); st != nil {
		listen = st.cfg.Server.Listen
	}
	base := "http://" + listenHost(listen)
	v := &toolsView{BaseURL: base}
	add := func(id, name, code string) { v.Presets = append(v.Presets, toolsPreset{id, name, code}) }

	add("env", "Shared env — every CLI below needs this",
		"export ONEGW_KEY=<one of auth.keys from onegw.toml>\nexport ONEGW_BASE="+base+"\nmodel: use \"provider/model\" or a combo name (e.g. dev)")

	add("claude-code", "Claude Code (Anthropic surface)",
		"export ANTHROPIC_BASE_URL="+base+"\n"+
			"export ANTHROPIC_AUTH_KEY=$ONEGW_KEY\n"+
			"# then: claude --model <provider/model-or-combo>")

	add("opencode", "opencode (~/.config/opencode/opencode.json)",
		"{\n  \"provider\": {\n    \"onegw\": {\n      \"npm\": \"@ai-sdk/openai-compatible\",\n"+
			"      \"name\": \"onegw\",\n      \"options\": {\n        \"baseURL\": \""+base+"/v1\",\n"+
			"        \"apiKey\": \"{env:ONEGW_KEY}\"\n      },\n      \"models\": {\"dev\": {\"name\": \"Dev (combo)\"}}\n    }\n  }\n}")

	add("grok", "grok (OpenAI-compatible surface)",
		"export GROK_API_KEY=$ONEGW_KEY\n"+
			"export GROK_BASE_URL="+base+"/v1\n"+
			"# grok speaks Chat Completions; use \"provider/model\" or a combo name")

	add("codex", "Codex CLI (~/.codex/config.toml)",
		"model_provider = \"onegw\"\nmodel = \"dev\"\n\n"+
			"[model_providers.onegw]\nname = \"onegw\"\nbase_url = \""+base+"/v1\"\n"+
			"env_key = \"ONEGW_KEY\"\nwire_api = \"chat\"")

	add("omp", "omp (~/.omp/agent/models.yml)",
		"providers:\n  onegw:\n    baseUrl: "+base+"/v1\n    apiKey: $ONEGW_KEY\n"+
			"    api: openai-completions\n    models:\n      - id: dev\n        name: Dev\n"+
			"        contextWindow: 1000000\n      - id: free\n        name: Free")

	add("pi", "pi (~/.pi/agent/models.json)",
		"{\n  \"providers\": {\n    \"onegw\": {\n      \"baseUrl\": \""+base+"/v1\",\n"+
			"      \"api\": \"openai-completions\",\n      \"apiKey\": \"${ONEGW_KEY}\",\n"+
			"      \"models\": [\n        {\"id\": \"dev\", \"name\": \"Dev (combo)\", \"reasoning\": true,\n"+
			"         \"contextWindow\": 1000000, \"maxTokens\": 131072}\n      ]\n    }\n  }\n}")

	add("hermes", "hermes (~/.hermes/config.yaml)",
		"providers:\n  onegw:\n    base_url: "+base+"/v1\n    key_env: ONEGW_KEY\n"+
			"    default_model: dev\n\n# plus ONEGW_KEY in ~/.hermes/.env as OPENAI_API_KEY\n"+
			"# if the hermes provider inherits the parent key")

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
	UpdCurrent  string
	UpdLatest   string
	UpdChecked  string
	UpdErr      string
	UpdApply    string
	UpdInDocker bool
	UpdOutdated bool
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	v := settingsView{PID: os.Getpid(), StartedAt: s.start.Format(time.RFC3339),
		UpdCurrent: update.Version()}
	if svc := s.updater(); svc != nil {
		st := svc.Snapshot()
		v.UpdLatest = st.Latest
		v.UpdInDocker = st.InContainer
		v.UpdOutdated = st.Outdated
		v.UpdApply = st.LastApply
		v.UpdErr = st.LastError
		if st.LastCheck != nil {
			v.UpdChecked = st.LastCheck.Format("2006-01-02 15:04:05 MST")
		}
	}
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
		// The owner record stamps UTC; the dashboard shows local.
		if mt, err := time.Parse(time.RFC3339Nano, o.ConfigMtime); err == nil {
			v.ConfigMtime = mt.Local().Format(time.RFC3339)
		} else {
			v.ConfigMtime = o.ConfigMtime
		}
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
