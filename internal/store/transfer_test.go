package store

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"onegw/internal/usage"
)

func rollupDay() (string, string) {
	now := time.Now().UTC()
	return now.Format("2006-01-02"), now.Format("15")
}

func TestRollupsDeterministicOrder(t *testing.T) {
	s := openTest(t)
	s.SetNodeID("node-b")
	day, hour := rollupDay()
	flush := func(node, prov, model string, req int64) {
		s.SetNodeID(node)
		if err := s.FlushBuckets([]usage.Bucket{{
			Key:      usage.Key{Day: day, Hour: hour, Provider: prov, Model: model, APIKey: "k"},
			Requests: req,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	flush("node-b", "p2", "m1", 1)
	flush("node-a", "p1", "m2", 1)
	flush("node-a", "p1", "m1", 1)
	s.SetNodeID("node-b")

	rows, err := s.Rollups("2000-01-01", "2999-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	for i, want := range [][3]string{{"p1", "m1", "node-a"}, {"p1", "m2", "node-a"}, {"p2", "m1", "node-b"}} {
		if rows[i].Provider != want[0] || rows[i].Model != want[1] || rows[i].Node != want[2] {
			t.Fatalf("row %d = %s/%s/%s, want %s/%s/%s", i,
				rows[i].Provider, rows[i].Model, rows[i].Node, want[0], want[1], want[2])
		}
	}
	// Repeat: identical output (deterministic total order).
	again, err := s.Rollups("2000-01-01", "2999-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(again) != fmt.Sprint(rows) {
		t.Fatal("Rollups not deterministic across calls")
	}
}

// Snapshot merge replaces counters per (key, node): importing the same file
// twice must not double-count — that is the idempotence property.
func TestMergeRowsSnapshotIdempotent(t *testing.T) {
	s := openTest(t)
	day, hour := rollupDay()
	rows := []RollupRow{{
		Node: "node-a",
		Bucket: usage.Bucket{
			Key:      usage.Key{Day: day, Hour: hour, Provider: "p", Model: "m", APIKey: "k"},
			Requests: 5, InputTokens: 100, OutputTokens: 50,
			FirstSeen: time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC),
			LastSeen:  time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC),
		},
	}}
	for i := 0; i < 3; i++ {
		if err := s.MergeRows(rows, false); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Rollups("2000-01-01", "2999-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Requests != 5 || got[0].InputTokens != 100 {
		t.Fatalf("snapshot re-import must not double-count: %+v", got)
	}
}

// Delta merge sums counters per (key, node): consecutive pushed windows
// accumulate on the aggregator.
func TestMergeRowsDeltaSums(t *testing.T) {
	s := openTest(t)
	day, hour := rollupDay()
	row := func(req int64) RollupRow {
		return RollupRow{
			Node: "node-a",
			Bucket: usage.Bucket{
				Key:      usage.Key{Day: day, Hour: hour, Provider: "p", Model: "m", APIKey: "k"},
				Requests: req,
			},
		}
	}
	if err := s.MergeRows([]RollupRow{row(3)}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.MergeRows([]RollupRow{row(4)}, true); err != nil {
		t.Fatal(err)
	}
	got, err := s.Rollups("2000-01-01", "2999-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Requests != 7 {
		t.Fatalf("delta windows must sum: %+v", got)
	}
}

// The two nodes' rows for the same key must stay separate shards and roll
// up together in QueryRange (aggregate view is node-agnostic).
func TestMergeRowsNodeShardsAggregate(t *testing.T) {
	s := openTest(t)
	day, hour := rollupDay()
	mk := func(node string, req int64) RollupRow {
		return RollupRow{
			Node: node,
			Bucket: usage.Bucket{
				Key:      usage.Key{Day: day, Hour: hour, Provider: "p", Model: "m", APIKey: "k"},
				Requests: req, InputTokens: 10 * req,
			},
		}
	}
	if err := s.MergeRows([]RollupRow{mk("node-a", 2)}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.MergeRows([]RollupRow{mk("node-b", 3)}, false); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Rollups("2000-01-01", "2999-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("per-node rows must remain distinct shards: %+v", rows)
	}
	reqs, in, _, err := s.TotalsSince(1)
	if err != nil || reqs != 5 || in != 50 {
		t.Fatalf("aggregate view wrong: req=%d in=%d err=%v", reqs, in, err)
	}
}

// Timestamps keep min(first)/max(last) across merges, and an empty incoming
// timestamp must never clobber a stored one.
func TestMergeRowsTimestampBounds(t *testing.T) {
	s := openTest(t)
	day, hour := rollupDay()
	key := usage.Key{Day: day, Hour: hour, Provider: "p", Model: "m", APIKey: "k"}
	first := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	if err := s.MergeRows([]RollupRow{{Node: "n", Bucket: usage.Bucket{
		Key: key, Requests: 1, FirstSeen: first, LastSeen: mid,
	}}}, false); err != nil {
		t.Fatal(err)
	}
	// Narrower window plus an empty-timestamp row: bounds must not shrink.
	if err := s.MergeRows([]RollupRow{{Node: "n", Bucket: usage.Bucket{
		Key: key, Requests: 1, FirstSeen: mid, LastSeen: mid,
	}}, {Node: "n", Bucket: usage.Bucket{
		Key: key, Requests: 1,
	}}}, true); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Rollups("2000-01-01", "2999-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("same key+node must be one shard: %+v", rows)
	}
	if !rows[0].FirstSeen.Equal(first) || !rows[0].LastSeen.Equal(mid) {
		t.Fatalf("bounds must stay min(first)/max(last): %v..%v", rows[0].FirstSeen, rows[0].LastSeen)
	}
	if rows[0].Requests != 3 {
		t.Fatalf("delta rows above must sum: %+v", rows[0])
	}
}

// Old schemas (PK without node_id) must rebuild, preserving every row.
func TestMigrateLegacySchemaKeepsRows(t *testing.T) {
	path := t.TempDir() + "/legacy.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE usage_rollup (
	day TEXT NOT NULL, hour TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL, api_key TEXT NOT NULL,
	requests INTEGER NOT NULL DEFAULT 0, input_tok INTEGER NOT NULL DEFAULT 0, output_tok INTEGER NOT NULL DEFAULT 0,
	cache_read INTEGER NOT NULL DEFAULT 0, cache_write INTEGER NOT NULL DEFAULT 0, reasoning INTEGER NOT NULL DEFAULT 0,
	saved_tok INTEGER NOT NULL DEFAULT 0, estimated INTEGER NOT NULL DEFAULT 0, first_seen TEXT, last_seen TEXT,
	PRIMARY KEY (day, hour, provider, model, api_key));
INSERT INTO usage_rollup (day, hour, provider, model, api_key, requests, input_tok)
	VALUES ('2026-09-01', '08', 'p', 'm', 'k', 4, 400)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.Rollups("2000-01-01", "2999-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Node != "" || rows[0].Requests != 4 || rows[0].InputTokens != 400 {
		t.Fatalf("legacy rows must survive migration: %+v", rows)
	}
	// The rebuild must recreate the day index the rebuild dropped.
	var idx int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type='index' AND name='idx_rollup_day' AND tbl_name='usage_rollup'`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatal("idx_rollup_day missing after legacy migration")
	}
	// New writes on the migrated schema keep their node shard.
	s.SetNodeID("node-x")
	if err := s.FlushBuckets([]usage.Bucket{{
		Key:      usage.Key{Day: "2026-09-01", Hour: "08", Provider: "p", Model: "m", APIKey: "k"},
		Requests: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Rollups("2000-01-01", "2999-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("migrated store must shard by node: %+v", rows)
	}
}
