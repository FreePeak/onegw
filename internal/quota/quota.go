// Package quota tracks per-provider usage against reset windows — calendar
// kinds (UTC daily, ISO weekly, monthly) and rolling duration grids (the
// named "5h" or any Go duration like "48h", "90m") — with optional
// token/request limits, giving 9router-style countdowns per provider.
// Tokens counted are input+output+reasoning; a zero limit means track-only
// (no enforcement).
//
// State is in-memory, seeded at startup from the persisted quota table
// (usage.db `quota_state`) with usage rollups as a fallback for history that
// predates the quota table, and flushed back to the store periodically.
package quota

import (
	"sort"
	"sync"
	"time"

	"onegw/internal/store"
)

// Window kinds used in ProviderCfg.QuotaWindow. Calendar kinds phase
// resets to UTC boundaries; any other accepted value is a Go duration
// string ("5h", "48h", "90m") driving a rolling grid (see rollingPeriod).
const (
	Off     = "" // tracking disabled
	W5h     = "5h"
	Daily   = "daily"
	Weekly  = "weekly"
	Monthly = "monthly"
)

const (
	periodD = 24 * time.Hour
	periodW = 7 * 24 * time.Hour
)

// rollingPeriod resolves a rolling-window spec to its grid length: the
// named "5h" or any positive time.ParseDuration value ("48h", "90m").
// Calendar kinds and "" are not rolling.
func rollingPeriod(w string) (time.Duration, bool) {
	switch w {
	case Off, Daily, Weekly, Monthly:
		return 0, false
	}
	d, err := time.ParseDuration(w)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// Limits configures one provider's quota window.
type Limits struct {
	// Window kind: "" (off), a calendar kind ("daily", "weekly",
	// "monthly") or a rolling duration ("5h", "48h", "90m").
	Window string
	// Anchor optionally pins the reset grid to an absolute instant: its
	// time-of-day phases daily resets, its instant phases weekly and
	// rolling grids, its day-of-month phases monthly (clamped to shorter
	// months). Zero = default (daily: UTC midnight; weekly: ISO Monday
	// 00:00 UTC; monthly: 1st of month; rolling: first-seen).
	Anchor time.Time
	// LimitTokens caps input+output+reasoning tokens per window; 0 = track
	// only. Same for LimitRequests with request count.
	LimitTokens   int64
	LimitRequests int64
}

// Status is one provider's quota state for the window containing now.
type Status struct {
	Provider      string    `json:"provider"`
	Window        string    `json:"window"`
	WindowStart   time.Time `json:"window_start"`
	WindowEnd     time.Time `json:"window_end"`
	UsedTokens    int64     `json:"used_tokens"`
	LimitTokens   int64     `json:"limit_tokens"`
	UsedRequests  int64     `json:"used_requests"`
	LimitRequests int64     `json:"limit_requests"`
	Exhausted     bool      `json:"exhausted"`
}

// windowState is one provider's live counters.
type windowState struct {
	start        time.Time // current window start (zero = never observed)
	usedTokens   int64
	usedRequests int64
}

// Tracker holds per-provider window state.
type Tracker struct {
	mu     sync.Mutex
	limits map[string]Limits
	state  map[string]*windowState
	store  *store.Store // nil = memory only

	stop     chan struct{}
	stopOnce sync.Once
	done     sync.WaitGroup
}

// New builds a tracker for the given per-provider limits and starts its
// flush loop (flushEvery <= 0 resets to 30 s). st may be nil (memory only).
func New(limits map[string]Limits, st *store.Store, flushEvery time.Duration) *Tracker {
	if flushEvery <= 0 {
		flushEvery = 30 * time.Second
	}
	t := &Tracker{
		limits: limits,
		state:  make(map[string]*windowState, len(limits)),
		store:  st,
		stop:   make(chan struct{}),
	}
	t.seed(time.Now())
	t.done.Add(1)
	go t.loop(flushEvery)
	return t
}

// ---------------------------------------------------------------------------
// Window math (pure; all instants UTC)

// currentWindow returns the [start, end) window containing now. anchor is
// the rolling grid origin (rolling windows: the provider's window start /
// first-seen; ignored by the calendar kinds, which use Limits.Anchor or
// the calendar).
func currentWindow(l Limits, anchor, now time.Time) (start, end time.Time) {
	switch l.Window {
	case Daily:
		if !l.Anchor.IsZero() {
			start = gridStart(l.Anchor, now, periodD)
		} else {
			y, m, d := now.UTC().Date()
			start = time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		}
		return start, start.Add(periodD)
	case Weekly:
		if !l.Anchor.IsZero() {
			start = gridStart(l.Anchor, now, periodW)
		} else {
			start = isoWeekStart(now.UTC())
		}
		return start, start.Add(periodW)
	case Monthly:
		start, end = monthGrid(l.Anchor, now.UTC())
		return start, end
	default:
		// Rolling windows: "5h" and any custom duration share one path —
		// a grid of that length floored onto the anchor.
		period, ok := rollingPeriod(l.Window)
		if !ok {
			return time.Time{}, time.Time{}
		}
		if anchor.IsZero() {
			if !l.Anchor.IsZero() {
				anchor = l.Anchor
			} else {
				return now, now.Add(period) // first-seen anchors here
			}
		}
		start = gridStart(anchor, now, period)
		return start, start.Add(period)
	}
}

// monthGrid returns the [start, end) calendar-month window containing u
// (UTC). A zero anchor is the plain calendar month (1st, 00:00 UTC); a
// non-zero anchor phases the grid to its day-of-month and time-of-day,
// clamped to each month's length (a 31st anchor resets on Feb 28/29).
func monthGrid(anchor, u time.Time) (start, end time.Time) {
	if anchor.IsZero() {
		y, m, _ := u.Date()
		start = time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0)
	}
	a := anchor.UTC()
	phase := func(y int, m time.Month) time.Time {
		day := a.Day()
		if last := daysIn(y, m); day > last {
			day = last
		}
		hh, mm, ss := a.Clock()
		return time.Date(y, m, day, hh, mm, ss, 0, time.UTC)
	}
	y, m, _ := u.Date()
	start = phase(y, m)
	if start.After(u) {
		start = phase(monthShift(y, m, -1))
	}
	end = phase(monthShift(start.Year(), start.Month(), 1))
	return start, end
}

// daysIn is the number of days in month m of year y (UTC leap rules).
func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// monthShift moves (y, m) by k whole months (m is 1-based).
func monthShift(y int, m time.Month, k int) (int, time.Month) {
	i := int(m) - 1 + k
	ny, nm := y+i/12, i%12
	if nm < 0 {
		nm += 12
		ny--
	}
	return ny, time.Month(nm + 1)
}

// gridStart floors now onto the grid anchored at a with spacing d.
func gridStart(a, now time.Time, d time.Duration) time.Time {
	k := floorDiv(now.Sub(a), d)
	return a.Add(time.Duration(k) * d)
}

func floorDiv(a, b time.Duration) int64 {
	q := int64(a / b)
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// isoWeekStart returns Monday 00:00 UTC of u's ISO week.
func isoWeekStart(u time.Time) time.Time {
	wd := int(u.Weekday())
	if wd == 0 {
		wd = 7
	}
	y, m, d := u.Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return midnight.AddDate(0, 0, -(wd - 1))
}

// ---------------------------------------------------------------------------
// Recording and status

// Observe records one completed request against the provider's current
// window. Unknown providers (no configured window) are ignored.
func (t *Tracker) Observe(provider string, tokens, requests int64, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	l, ok := t.limits[provider]
	if !ok || l.Window == Off {
		return
	}
	ws := t.state[provider]
	if ws == nil {
		ws = &windowState{}
		t.state[provider] = ws
	}
	roll(l, ws, now)
	ws.usedTokens += tokens
	ws.usedRequests += requests
}

// roll advances ws to the window containing now, resetting counters on
// rollover. The grid is re-derived from the configured anchor whenever one
// exists (so a changed anchor takes effect immediately); without an anchor
// the current window start carries the rolling grid forward. Caller holds mu.
func roll(l Limits, ws *windowState, now time.Time) {
	anchor := ws.start
	if !l.Anchor.IsZero() {
		anchor = l.Anchor
	}
	start, _ := currentWindow(l, anchor, now)
	if !ws.start.Equal(start) {
		ws.start = start
		ws.usedTokens, ws.usedRequests = 0, 0
	}
}

// Status reports the provider's window state read-only (a never-observed
// provider shows zeroed counters and the prospective window). ok=false when
// the provider has no quota window configured.
func (t *Tracker) Status(provider string, now time.Time) (Status, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	l, ok := t.limits[provider]
	if !ok || l.Window == Off {
		return Status{}, false
	}
	ws := t.state[provider]
	if ws == nil {
		ws = &windowState{}
	}
	anchor := ws.start
	if !l.Anchor.IsZero() {
		anchor = l.Anchor
	}
	start, end := currentWindow(l, anchor, now)
	var usedT, usedR int64
	if ws.start.Equal(start) {
		usedT, usedR = ws.usedTokens, ws.usedRequests
	}
	return Status{
		Provider:      provider,
		Window:        l.Window,
		WindowStart:   start,
		WindowEnd:     end,
		UsedTokens:    usedT,
		LimitTokens:   l.LimitTokens,
		UsedRequests:  usedR,
		LimitRequests: l.LimitRequests,
		Exhausted:     exhausted(l, usedT, usedR),
	}, true
}

// All returns statuses for every tracked provider, ordered by name.
func (t *Tracker) All(now time.Time) []Status {
	t.mu.Lock()
	names := make([]string, 0, len(t.limits))
	for n := range t.limits {
		names = append(names, n)
	}
	t.mu.Unlock()
	sort.Strings(names)
	out := make([]Status, 0, len(names))
	for _, n := range names {
		if st, ok := t.Status(n, now); ok {
			out = append(out, st)
		}
	}
	return out
}

func exhausted(l Limits, usedT, usedR int64) bool {
	if l.LimitTokens > 0 && usedT >= l.LimitTokens {
		return true
	}
	if l.LimitRequests > 0 && usedR >= l.LimitRequests {
		return true
	}
	return false
}

// Inherit carries in-memory window state from a previous tracker (hot
// reload); it is fresher than the last flush. Only providers present in the
// new limits are carried over; stale windows reset on the next Observe.
func (t *Tracker) Inherit(o *Tracker) {
	if o == nil {
		return
	}
	o.mu.Lock()
	snap := make(map[string]windowState, len(o.state))
	for n, ws := range o.state {
		snap[n] = *ws
	}
	o.mu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	for n, ws := range snap {
		if _, ok := t.limits[n]; ok {
			cp := ws
			t.state[n] = &cp
		}
	}
}

// ---------------------------------------------------------------------------
// Persistence: seed at startup, flush on a ticker

// seed rebuilds window state from the store: the persisted quota table is
// authoritative when its window is still current; usage rollups fill in
// history that predates it (conservative — a window starting mid-hour counts
// that whole hour).
func (t *Tracker) seed(now time.Time) {
	if t.store == nil {
		return
	}
	if rows, err := t.store.LoadQuotaState(); err == nil {
		for _, r := range rows {
			l, ok := t.limits[r.Provider]
			if !ok || l.Window == Off {
				continue
			}
			start, err := time.Parse(time.RFC3339, r.WindowStart)
			if err != nil {
				continue
			}
			t.state[r.Provider] = &windowState{
				start: start, usedTokens: r.UsedTokens, usedRequests: r.UsedRequests,
			}
		}
	}
	for name, l := range t.limits {
		if l.Window == Off {
			continue
		}
		if ws := t.state[name]; ws != nil && !ws.start.IsZero() {
			chkAnchor := ws.start
			if !l.Anchor.IsZero() {
				chkAnchor = l.Anchor
			}
			if start, _ := currentWindow(l, chkAnchor, now); start.Equal(ws.start) {
				continue // persisted counters cover the live window
			}
			delete(t.state, name) // stale window; rebuild below
		}
		start, _ := currentWindow(l, t.rebuildAnchor(name, l, now), now)
		if start.IsZero() {
			continue
		}
		usedT, usedR, err := t.rollupSum(name, start)
		if err != nil || (usedT == 0 && usedR == 0) {
			continue
		}
		t.state[name] = &windowState{start: start, usedTokens: usedT, usedRequests: usedR}
	}
}

// rebuildAnchor derives a window anchor for a provider with no usable
// persisted state: the configured anchor, else the earliest rollup first-seen
// (the best available first-seen for any rolling grid), else now.
func (t *Tracker) rebuildAnchor(name string, l Limits, now time.Time) time.Time {
	if !l.Anchor.IsZero() {
		return l.Anchor
	}
	if _, ok := rollingPeriod(l.Window); !ok {
		return time.Time{} // calendar kinds derive from the calendar
	}
	if fs, err := t.store.QuotaFirstSeen(name); err == nil && !fs.IsZero() && fs.Before(now) {
		return fs
	}
	return now
}

// rollupSum aggregates usage_rollup rows for provider whose (day, hour)
// falls at or after the window start. Day/hour are zero-padded UTC strings,
// so lexicographic order is chronological.
func (t *Tracker) rollupSum(provider string, start time.Time) (tokens, requests int64, err error) {
	rows, rerr := t.store.QueryRange(start.UTC().Format("2006-01-02"), time.Now().UTC().Format("2006-01-02"))
	if rerr != nil {
		return 0, 0, rerr
	}
	sd, sh := start.UTC().Format("2006-01-02"), start.UTC().Format("15")
	for _, r := range rows {
		if r.Provider != provider || r.Day < sd || (r.Day == sd && r.Hour < sh) {
			continue
		}
		tokens += r.InputTok + r.OutputTok + r.Reasoning
		requests += r.Requests
	}
	return tokens, requests, nil
}

// flush persists window state; best-effort (a lost flush is recovered from
// rollups or costs at most one window of slight under-count after a crash).
func (t *Tracker) flush() {
	if t.store == nil {
		return
	}
	t.mu.Lock()
	rows := make([]store.QuotaState, 0, len(t.state))
	for name, ws := range t.state {
		l := t.limits[name]
		if l.Window == Off || ws.start.IsZero() {
			continue
		}
		rows = append(rows, store.QuotaState{
			Provider:     name,
			Window:       l.Window,
			WindowStart:  ws.start.UTC().Format(time.RFC3339),
			UsedTokens:   ws.usedTokens,
			UsedRequests: ws.usedRequests,
		})
	}
	t.mu.Unlock()
	if len(rows) == 0 {
		return
	}
	_ = t.store.SaveQuotaState(rows)
}

func (t *Tracker) loop(every time.Duration) {
	defer t.done.Done()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			t.flush()
			return
		case <-tick.C:
			t.flush()
		}
	}
}

// Stop flushes remaining state and ends the loop. Idempotent.
func (t *Tracker) Stop() {
	t.stopOnce.Do(func() { close(t.stop) })
	t.done.Wait()
}
