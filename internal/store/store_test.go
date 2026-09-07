package store

import (
	"path/filepath"
	"testing"
	"time"

	"onegw/internal/usage"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFlushAndQuery(t *testing.T) {
	s := openTest(t)
	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	hour := now.Format("15")

	b1 := usage.Bucket{Key: usage.Key{Day: day, Hour: hour, Provider: "p1", Model: "m1", APIKey: "k"},
		Requests: 3, InputTokens: 100, OutputTokens: 40, SavedTokens: 25}
	b2 := usage.Bucket{Key: usage.Key{Day: day, Hour: hour, Provider: "p1", Model: "m1", APIKey: "k"},
		Requests: 2, InputTokens: 50, OutputTokens: 10}
	b3 := usage.Bucket{Key: usage.Key{Day: day, Hour: hour, Provider: "p2", Model: "m2", APIKey: "k"},
		Requests: 1, InputTokens: 7, OutputTokens: 3}

	if err := s.FlushBuckets([]usage.Bucket{b1, b2, b3}); err != nil {
		t.Fatal(err)
	}
	// Second flush of b1 must accumulate, not duplicate.
	if err := s.FlushBuckets([]usage.Bucket{b1}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.QueryRange("2000-01-01", "2999-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
	}
	var m1 *UsageRow
	for i := range rows {
		if rows[i].Model == "m1" {
			m1 = &rows[i]
		}
	}
	if m1 == nil || m1.Requests != 8 || m1.InputTok != 250 || m1.OutputTok != 90 || m1.SavedTok != 50 {
		t.Fatalf("m1 rollup wrong: %+v", m1)
	}
}

func TestTotalsAndPrune(t *testing.T) {
	s := openTest(t)
	day := time.Now().UTC().Format("2006-01-02")
	err := s.FlushBuckets([]usage.Bucket{{
		Key: usage.Key{Day: day, Hour: "10", Provider: "p", Model: "m", APIKey: "k"},
		Requests: 1, InputTokens: 1000, OutputTokens: 500,
	}})
	if err != nil {
		t.Fatal(err)
	}
	reqs, in, out, err := s.TotalsSince(1)
	if err != nil || reqs != 1 || in != 1000 || out != 500 {
		t.Fatalf("totals wrong: %d %d %d %v", reqs, in, out, err)
	}
	// Prune with big retention keeps, with 0-day retention deletes today? No:
	// cutoff = today - 0 => today; day < cutoff false; nothing pruned.
	n, err := s.Prune(0)
	if err != nil || n != 0 {
		t.Fatalf("prune today should keep: %d %v", n, err)
	}
	// Old day prunes.
	err = s.FlushBuckets([]usage.Bucket{{
		Key: usage.Key{Day: "2020-01-01", Hour: "00", Provider: "p", Model: "m", APIKey: "k"},
		Requests: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	n, err = s.Prune(30)
	if err != nil || n != 1 {
		t.Fatalf("old row should prune: %d %v", n, err)
	}
}
