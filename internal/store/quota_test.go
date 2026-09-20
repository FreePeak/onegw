package store

import (
	"testing"
	"time"

	"onegw/internal/usage"
)

func TestQuotaStateRoundTrip(t *testing.T) {
	path := t.TempDir() + "/usage.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	start := time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC).Format(time.RFC3339)
	err = s.SaveQuotaState([]QuotaState{
		{Provider: "glm", Window: "5h", WindowStart: start, UsedTokens: 1234, UsedRequests: 7},
		{Provider: "b-ai", Window: "daily", WindowStart: start, UsedTokens: 99, UsedRequests: 3},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	rows, err := s.LoadQuotaState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	byProv := map[string]QuotaState{}
	for _, r := range rows {
		byProv[r.Provider] = r
	}
	got := byProv["glm"]
	if got.Window != "5h" || got.WindowStart != start || got.UsedTokens != 1234 || got.UsedRequests != 7 {
		t.Fatalf("glm row wrong: %+v", got)
	}
	// Upsert replaces counters per provider.
	if err := s.SaveQuotaState([]QuotaState{{Provider: "glm", Window: "5h", WindowStart: start, UsedTokens: 50, UsedRequests: 1}}); err != nil {
		t.Fatalf("resave: %v", err)
	}
	rows, _ = s.LoadQuotaState()
	if len(rows) != 2 {
		t.Fatalf("upsert must not duplicate rows, got %d", len(rows))
	}
	for _, r := range rows {
		if r.Provider == "glm" && (r.UsedTokens != 50 || r.UsedRequests != 1) {
			t.Fatalf("upsert did not replace: %+v", r)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestQuotaRebuildAcrossRestart simulates a process restart: state saved by
// one store handle is readable through a fresh handle (no in-memory carry).
func TestQuotaRebuildAcrossRestart(t *testing.T) {
	path := t.TempDir() + "/usage.db"
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	start := time.Now().UTC().Format(time.RFC3339)
	if err := s1.SaveQuotaState([]QuotaState{{Provider: "p", Window: "weekly", WindowStart: start, UsedTokens: 4096, UsedRequests: 12}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close1: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer s2.Close()
	rows, err := s2.LoadQuotaState()
	if err != nil {
		t.Fatalf("load2: %v", err)
	}
	if len(rows) != 1 || rows[0].UsedTokens != 4096 || rows[0].UsedRequests != 12 {
		t.Fatalf("rebuild after restart wrong: %+v", rows)
	}
}

func TestQuotaFirstSeen(t *testing.T) {
	path := t.TempDir() + "/usage.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	fs, err := s.QuotaFirstSeen("nope")
	if err != nil {
		t.Fatalf("first seen on empty store: %v", err)
	}
	if !fs.IsZero() {
		t.Fatalf("empty store should give zero time, got %v", fs)
	}
	first := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	second := first.Add(2 * time.Hour)
	b := []usage.Bucket{
		{Key: usage.Key{Day: "2026-09-01", Hour: "08", Provider: "p", Model: "m", APIKey: "k"}, FirstSeen: first, LastSeen: first},
		{Key: usage.Key{Day: "2026-09-01", Hour: "10", Provider: "p", Model: "m", APIKey: "k"}, FirstSeen: second, LastSeen: second},
	}
	if err := s.FlushBuckets(b); err != nil {
		t.Fatalf("flush: %v", err)
	}
	fs, err = s.QuotaFirstSeen("p")
	if err != nil {
		t.Fatalf("first seen: %v", err)
	}
	if !fs.Equal(first) {
		t.Fatalf("want earliest first_seen %v, got %v", first, fs)
	}
}
