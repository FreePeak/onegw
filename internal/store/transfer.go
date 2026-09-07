// Per-node usage transfer: export rollups as node-attributed rows and merge
// remote rows into the local store. Rollups are sharded by source node
// (node_id is part of the primary key), which makes snapshot imports
// idempotent (REPLACE per key+node) while delta imports accumulate (SUM).
package store

import (
	"database/sql"
	"time"

	"onegw/internal/usage"
)

// SetNodeID attributes rows written by FlushBuckets to this instance.
// Called once at startup, before any flush can run.
func (s *Store) SetNodeID(id string) { s.nodeID = id }

// RollupRow is one stored rollup shard: a usage.Bucket attributed to the
// node that produced it. JSON is flat — Bucket fields plus "node".
type RollupRow struct {
	Node string `json:"node"`
	usage.Bucket
}

// EachRollup streams every rollup shard in [fromDay, toDay] to fn in
// (day, hour, provider, model, api_key, node) order — a deterministic
// total order for export. Memory is O(1 row); the walk aborts on the
// first fn error. Used by the export handler so a full-history export
// never materializes in memory.
func (s *Store) EachRollup(fromDay, toDay string, fn func(RollupRow) error) error {
	rows, err := s.db.Query(`SELECT node_id, day, hour, provider, model, api_key,
			requests, input_tok, output_tok, cache_read, cache_write, reasoning, saved_tok,
			estimated, first_seen, last_seen
		FROM usage_rollup WHERE day >= ? AND day <= ?
		ORDER BY day, hour, provider, model, api_key, node_id`, fromDay, toDay)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRollupRow(rows)
		if err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Rollups materializes EveryRollup's stream — for tests and small admin
// views. The export handler uses EachRollup directly.
func (s *Store) Rollups(fromDay, toDay string) ([]RollupRow, error) {
	var out []RollupRow
	err := s.EachRollup(fromDay, toDay, func(r RollupRow) error {
		out = append(out, r)
		return nil
	})
	return out, err
}

func scanRollupRow(sc interface{ Scan(...any) error }) (RollupRow, error) {
	var r RollupRow
	var day, hour string
	var first, last sql.NullString
	var est int64
	if err := sc.Scan(&r.Node, &day, &hour, &r.Bucket.Provider, &r.Bucket.Model, &r.Bucket.APIKey,
		&r.Bucket.Requests, &r.Bucket.InputTokens, &r.Bucket.OutputTokens,
		&r.Bucket.CacheRead, &r.Bucket.CacheWrite, &r.Bucket.Reasoning, &r.Bucket.SavedTokens,
		&est, &first, &last); err != nil {
		return r, err
	}
	r.Bucket.Day, r.Bucket.Hour = day, hour
	r.Bucket.Estimated = est > 0
	r.Bucket.FirstSeen = parseTime(first.String)
	r.Bucket.LastSeen = parseTime(last.String)
	return r, nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// mergeStmt builds the upsert for one MergeRows mode. Snapshot mode replaces
// every counter with the incoming value — re-importing the same snapshot can
// never double-count. Delta mode sums — consecutive push windows accumulate.
// Both keep min(first_seen)/max(last_seen) and never let an empty incoming
// timestamp clobber a stored one (scalar MIN/MAX in SQLite return NULL when
// any argument is NULL, and ” sorts before every timestamp).
func mergeStmt(tx *sql.Tx, delta bool) (*sql.Stmt, error) {
	const cols = `day, hour, provider, model, api_key, node_id,
		requests, input_tok, output_tok, cache_read, cache_write, reasoning, saved_tok,
		estimated, first_seen, last_seen`
	vals := `?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?`
	setCounters := `requests = excluded.requests, input_tok = excluded.input_tok,
			output_tok = excluded.output_tok, cache_read = excluded.cache_read,
			cache_write = excluded.cache_write, reasoning = excluded.reasoning,
			saved_tok = excluded.saved_tok, estimated = excluded.estimated`
	sumCounters := `requests = requests + excluded.requests,
			input_tok = input_tok + excluded.input_tok,
			output_tok = output_tok + excluded.output_tok,
			cache_read = cache_read + excluded.cache_read,
			cache_write = cache_write + excluded.cache_write,
			reasoning = reasoning + excluded.reasoning,
			saved_tok = saved_tok + excluded.saved_tok,
			estimated = estimated + excluded.estimated`
	mode := setCounters
	if delta {
		mode = sumCounters
	}
	q := `INSERT INTO usage_rollup (` + cols + `) VALUES (` + vals + `)
		ON CONFLICT(day, hour, provider, model, api_key, node_id) DO UPDATE SET
		` + mode + `,
		first_seen = CASE
			WHEN excluded.first_seen IS NULL OR excluded.first_seen = '' THEN first_seen
			WHEN first_seen IS NULL OR first_seen = '' THEN excluded.first_seen
			ELSE MIN(first_seen, excluded.first_seen) END,
		last_seen = CASE
			WHEN excluded.last_seen IS NULL OR excluded.last_seen = '' THEN last_seen
			WHEN last_seen IS NULL OR last_seen = '' THEN excluded.last_seen
			ELSE MAX(last_seen, excluded.last_seen) END`
	return tx.Prepare(q)
}

// MergeRows merges exported rows into the local store. delta selects the
// merge mode: false = snapshot (replace counters per key+node; idempotent
// re-import), true = delta (sum counters; push windows).
func (s *Store) MergeRows(rows []RollupRow, delta bool) error {
	if len(rows) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := mergeStmt(tx, delta)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for i := range rows {
		r := &rows[i]
		est := 0
		if r.Estimated {
			est = 1
		}
		if _, err := stmt.Exec(r.Day, r.Hour, r.Provider, r.Model, r.APIKey, r.Node,
			r.Requests, r.InputTokens, r.OutputTokens, r.CacheRead, r.CacheWrite,
			r.Reasoning, r.SavedTokens, est,
			fmtTime(r.FirstSeen), fmtTime(r.LastSeen)); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
