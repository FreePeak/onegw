package quota

import (
	"testing"
	"time"
)

// windowMaths covers the pure window boundary logic via currentWindow.
func TestWindow5hRolling(t *testing.T) {
	l := Limits{Window: W5h}
	anchor := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	// Anchor instant starts a window.
	start, end := currentWindow(l, anchor, anchor)
	if !start.Equal(anchor) || !end.Equal(anchor.Add(5*time.Hour)) {
		t.Fatalf("anchor window: got %v..%v", start, end)
	}
	// 4h59m in → same window; 5h01m in → next window.
	in, _ := currentWindow(l, anchor, anchor.Add(4*time.Hour+59*time.Minute))
	if !in.Equal(anchor) {
		t.Fatalf("4h59m should stay in first window, got %v", in)
	}
	next, _ := currentWindow(l, anchor, anchor.Add(5*time.Hour+time.Minute))
	if !next.Equal(anchor.Add(5 * time.Hour)) {
		t.Fatalf("5h01m should roll over, got %v", next)
	}
	// Deeply inside a later slot: floor lands on the grid.
	mid, _ := currentWindow(l, anchor, anchor.Add(13*time.Hour+2*time.Hour))
	if !mid.Equal(anchor.Add(15 * time.Hour)) {
		t.Fatalf("grid floor wrong: got %v", mid)
	}
}

func TestWindow5hAnchoredGrid(t *testing.T) {
	// Anchor phases the 5h grid: 06:30 is between slots 05:00 and 10:00
	// for anchor 2026-09-07T01:30.
	a := time.Date(2026, 9, 7, 1, 30, 0, 0, time.UTC)
	l := Limits{Window: W5h}
	start, end := currentWindow(l, a, time.Date(2026, 9, 7, 6, 30, 0, 0, time.UTC))
	if !start.Equal(a.Add(5*time.Hour)) || !end.Equal(a.Add(10*time.Hour)) {
		t.Fatalf("anchored 5h grid wrong: %v..%v", start, end)
	}
}

func TestWindowDailyUTCMidnight(t *testing.T) {
	l := Limits{Window: Daily}
	now := time.Date(2026, 9, 7, 13, 45, 12, 0, time.UTC)
	start, end := currentWindow(l, time.Time{}, now)
	want := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	if !start.Equal(want) || !end.Equal(want.Add(24*time.Hour)) {
		t.Fatalf("daily window wrong: %v..%v", start, end)
	}
	// Just before midnight is still yesterday's window.
	late := now.Add(-14 * time.Hour).Add(-time.Minute) // 2026-09-06 23:59
	prev, _ := currentWindow(l, time.Time{}, late)
	if !prev.Equal(want.Add(-24 * time.Hour)) {
		t.Fatalf("pre-midnight should be previous day, got %v", prev)
	}
}

func TestWindowDailyAnchorOffset(t *testing.T) {
	// Daily resets at 09:30 UTC (anchor's time-of-day), so 08:00 belongs
	// to the window that started the previous day at 09:30.
	a := time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC)
	l := Limits{Window: Daily, Anchor: a}
	start, end := currentWindow(l, a, time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC))
	wantStart := time.Date(2026, 9, 6, 9, 30, 0, 0, time.UTC)
	if !start.Equal(wantStart) || !end.Equal(wantStart.Add(24*time.Hour)) {
		t.Fatalf("anchored daily window wrong: %v..%v", start, end)
	}
}

func TestWindowWeeklyISOMonday(t *testing.T) {
	l := Limits{Window: Weekly}
	// 2026-09-07 is a Monday; Saturday 09-12 falls in the same ISO week.
	sat := time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC)
	start, end := currentWindow(l, time.Time{}, sat)
	want := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	if !start.Equal(want) || !end.Equal(want.Add(7*24*time.Hour)) {
		t.Fatalf("weekly window wrong: %v..%v", start, end)
	}
	// Sunday 09-06 belongs to the previous week (ISO weeks start Monday).
	sun := time.Date(2026, 9, 6, 23, 0, 0, 0, time.UTC)
	prev, _ := currentWindow(l, time.Time{}, sun)
	if !prev.Equal(want.Add(-7 * 24 * time.Hour)) {
		t.Fatalf("Sunday should be previous ISO week, got %v", prev)
	}
}

func TestWindowWeeklyAnchor(t *testing.T) {
	// Weekly grid phased to the anchor instant (Wednesday 12:00); the
	// following Wednesday 11:00 is still inside the 7-day window.
	a := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) // Wednesday
	l := Limits{Window: Weekly, Anchor: a}
	start, end := currentWindow(l, a, time.Date(2026, 9, 9, 11, 0, 0, 0, time.UTC))
	if !start.Equal(a) || !end.Equal(a.Add(7*24*time.Hour)) {
		t.Fatalf("anchored weekly window wrong: %v..%v", start, end)
	}
	// 13:00 that Wednesday has rolled into the next window.
	next, _ := currentWindow(l, a, time.Date(2026, 9, 9, 13, 0, 0, 0, time.UTC))
	if !next.Equal(a.Add(7 * 24 * time.Hour)) {
		t.Fatalf("anchored weekly should roll at anchor time-of-day, got %v", next)
	}
}

// statusAndObserve covers tracker behaviour: accumulation, rollover reset,
// exhaustion, and track-only mode.

func TestObserveAccumulatesAndRolls(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	tr := New(map[string]Limits{"p": {Window: W5h, LimitRequests: 3}}, nil, time.Hour)
	defer tr.Stop()
	for i := 0; i < 3; i++ {
		tr.Observe("p", 100, 1, now)
	}
	st, ok := tr.Status("p", now)
	if !ok || !st.Exhausted || st.UsedTokens != 300 || st.UsedRequests != 3 {
		t.Fatalf("status after 3 reqs: %+v ok=%v", st, ok)
	}
	// After window end the counters reset and the grid advances by one
	// 5h slot from the first-seen anchor.
	later := now.Add(5 * time.Hour).Add(time.Minute)
	st, _ = tr.Status("p", later)
	wantStart := now.Add(5 * time.Hour)
	if st.Exhausted || st.UsedRequests != 0 || !st.WindowStart.Equal(wantStart) {
		t.Fatalf("status after rollover: %+v (want window start %v)", st, wantStart)
	}
	// A fresh observation inside the new window keeps counters there.
	tr.Observe("p", 10, 1, later)
	st, _ = tr.Status("p", later)
	if st.Exhausted || st.UsedRequests != 1 {
		t.Fatalf("status after rollover observe: %+v", st)
	}
}

func TestTrackOnlyNeverExhausts(t *testing.T) {
	now := time.Now().UTC()
	tr := New(map[string]Limits{"p": {Window: Daily}}, nil, time.Hour)
	defer tr.Stop()
	tr.Observe("p", 1_000_000, 1000, now)
	if st, _ := tr.Status("p", now); st.Exhausted {
		t.Fatalf("track-only mode must never exhaust: %+v", st)
	}
}

func TestRequestLimitExhaustsIndependently(t *testing.T) {
	now := time.Now().UTC()
	tr := New(map[string]Limits{"p": {Window: Daily, LimitRequests: 2}}, nil, time.Hour)
	defer tr.Stop()
	tr.Observe("p", 0, 1, now)
	tr.Observe("p", 0, 1, now)
	if st, _ := tr.Status("p", now); !st.Exhausted {
		t.Fatal("request limit should exhaust")
	}
}

func TestUntrackedProviderIgnored(t *testing.T) {
	tr := New(map[string]Limits{}, nil, time.Hour)
	defer tr.Stop()
	tr.Observe("p", 100, 1, time.Now())
	if _, ok := tr.Status("p", time.Now()); ok {
		t.Fatal("untracked provider must report ok=false")
	}
	if got := tr.All(time.Now()); len(got) != 0 {
		t.Fatalf("All() should be empty, got %v", got)
	}
}

func TestWindowOffIgnored(t *testing.T) {
	tr := New(map[string]Limits{"p": {Window: Off, LimitTokens: 10}}, nil, time.Hour)
	defer tr.Stop()
	tr.Observe("p", 1000, 10, time.Now())
	if _, ok := tr.Status("p", time.Now()); ok {
		t.Fatal("window off must report ok=false")
	}
}

func TestInheritCarriesLiveState(t *testing.T) {
	now := time.Now().UTC()
	old := New(map[string]Limits{"a": {Window: Daily, LimitTokens: 100}, "b": {Window: Daily}}, nil, time.Hour)
	old.Observe("a", 50, 2, now)
	old.Observe("b", 10, 1, now)
	nw := New(map[string]Limits{"a": {Window: Daily, LimitTokens: 100}}, nil, time.Hour)
	defer nw.Stop()
	defer old.Stop()
	nw.Inherit(old)
	st, ok := nw.Status("a", now)
	if !ok || st.UsedTokens != 50 || st.UsedRequests != 2 {
		t.Fatalf("inherited state wrong: %+v ok=%v", st, ok)
	}
}

// TestAnchorChangeRephasesWindow proves a changed quota_reset_anchor takes
// effect even when window state already exists (the persisted/observed
// window start must not shadow the configured grid).
func TestAnchorChangeRephasesWindow(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	// First-seen anchors the window at 10:00 (no configured anchor).
	tr := New(map[string]Limits{"p": {Window: W5h, LimitTokens: 100}}, nil, time.Hour)
	defer tr.Stop()
	tr.Observe("p", 10, 1, now)
	if st, _ := tr.Status("p", now); !st.WindowStart.Equal(now) {
		t.Fatalf("first-seen window start wrong: %+v", st)
	}
	// Reload with an anchor at 02:30: the grid re-phases to 10:00-5h*k =
	// 05:00..10:00? No: floor(10:00 - 02:30 over 5h) = 1 slot -> 07:30.
	newAnchor := time.Date(2026, 9, 7, 2, 30, 0, 0, time.UTC)
	tr2 := New(map[string]Limits{"p": {Window: W5h, LimitTokens: 100, Anchor: newAnchor}}, nil, time.Hour)
	defer tr2.Stop()
	tr2.Inherit(tr)
	st, _ := tr2.Status("p", now)
	want := time.Date(2026, 9, 7, 7, 30, 0, 0, time.UTC)
	if !st.WindowStart.Equal(want) || !st.WindowEnd.Equal(want.Add(5*time.Hour)) {
		t.Fatalf("anchor change must re-phase the grid: got %v..%v want %v..", st.WindowStart, st.WindowEnd, want)
	}
	// Counters from the shadowed window are stale and reset on observe.
	tr2.Observe("p", 5, 1, now)
	if st, _ := tr2.Status("p", now); st.UsedTokens != 5 || st.UsedRequests != 1 {
		t.Fatalf("stale counters must reset after re-phase: %+v", st)
	}
}

func TestDailyResetsAcrossMidnight(t *testing.T) {
	tr := New(map[string]Limits{"p": {Window: Daily, LimitTokens: 100}}, nil, time.Hour)
	defer tr.Stop()
	now := time.Date(2026, 9, 7, 23, 59, 0, 0, time.UTC)
	tr.Observe("p", 100, 1, now)
	if st, _ := tr.Status("p", now); !st.Exhausted {
		t.Fatal("should exhaust at limit")
	}
	tomorrow := now.Add(2 * time.Minute) // 00:01 next day
	if st, _ := tr.Status("p", tomorrow); st.Exhausted || st.UsedTokens != 0 {
		t.Fatalf("midnight must reset counters: %+v", st)
	}
}

// windowStartEdge checks the 5h first-seen semantics: an observe exactly at
// tracker start opens the window at that instant, not the calendar hour.
func Test5hFirstSeenAnchorsWindow(t *testing.T) {
	now := time.Date(2026, 9, 7, 13, 37, 12, 0, time.UTC)
	tr := New(map[string]Limits{"p": {Window: W5h, LimitTokens: 10}}, nil, time.Hour)
	defer tr.Stop()
	tr.Observe("p", 10, 1, now)
	st, _ := tr.Status("p", now)
	if !st.WindowStart.Equal(now) {
		t.Fatalf("5h window should start at first-seen %v, got %v", now, st.WindowStart)
	}
	later := now.Add(4*time.Hour + 59*time.Minute)
	tr.Observe("p", 0, 1, later) // inside the same window
	if st, _ := tr.Status("p", later); !st.WindowStart.Equal(now) || st.WindowEnd.Equal(st.WindowStart) {
		t.Fatalf("window should still be the first one: %+v", st)
	}
}
