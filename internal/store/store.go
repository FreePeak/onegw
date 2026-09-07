// Package store persists usage rollups to SQLite (pure Go driver, WAL mode,
// single writer). Schema is intentionally tiny: one rollup table + an
// in-memory-friendly index. Config lives in TOML, not SQLite.
package store

import (
	"database/sql"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"onegw/internal/usage"
)

// Store wraps the SQLite handle.
type Store struct {
	db   *sql.DB
	mu   sync.Mutex // serialize writers (modernc sqlite prefers 1 writer)
	path string
	// nodeID attributes rows written by FlushBuckets to this instance
	// (usage transfer: export/import across instances). Set once at
	// startup via SetNodeID, before any flush can run.
	nodeID string
}

// Open creates/opens the database at path and applies pragmas.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=max_page_count(2147483646)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2) // 1 reader beyond the writer
	db.SetMaxIdleConns(2)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS usage_rollup (
	day         TEXT NOT NULL,
	hour        TEXT NOT NULL,
	provider    TEXT NOT NULL,
	model       TEXT NOT NULL,
	api_key     TEXT NOT NULL,
	node_id     TEXT NOT NULL DEFAULT '',
	requests    INTEGER NOT NULL DEFAULT 0,
	input_tok   INTEGER NOT NULL DEFAULT 0,
	output_tok  INTEGER NOT NULL DEFAULT 0,
	cache_read  INTEGER NOT NULL DEFAULT 0,
	cache_write INTEGER NOT NULL DEFAULT 0,
	reasoning   INTEGER NOT NULL DEFAULT 0,
	saved_tok   INTEGER NOT NULL DEFAULT 0,
	estimated   INTEGER NOT NULL DEFAULT 0,
	first_seen  TEXT,
	last_seen   TEXT,
	PRIMARY KEY (day, hour, provider, model, api_key, node_id)
);
CREATE INDEX IF NOT EXISTS idx_rollup_day ON usage_rollup(day);
`
	if _, err := s.db.Exec(ddl); err != nil {
		return err
	}
	// Older schemas keyed on (day, hour, provider, model, api_key) without
	// node_id: rebuild onto the node-sharded key, keeping every counter.
	col, err := s.db.Query(`PRAGMA table_info(usage_rollup)`)
	if err != nil {
		return err
	}
	var hasNode bool
	for col.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt any
		if err := col.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			col.Close()
			return err
		}
		if name == "node_id" {
			hasNode = true
		}
	}
	if err := col.Err(); err != nil {
		return err
	}
	col.Close()
	if hasNode {
		return s.migrateQuota() // already sharded; still ensure quota state
	}
	const rebuild = `
BEGIN IMMEDIATE;
ALTER TABLE usage_rollup RENAME TO usage_rollup_old;
CREATE TABLE usage_rollup (
	day         TEXT NOT NULL,
	hour        TEXT NOT NULL,
	provider    TEXT NOT NULL,
	model       TEXT NOT NULL,
	api_key     TEXT NOT NULL,
	node_id     TEXT NOT NULL DEFAULT '',
	requests    INTEGER NOT NULL DEFAULT 0,
	input_tok   INTEGER NOT NULL DEFAULT 0,
	output_tok  INTEGER NOT NULL DEFAULT 0,
	cache_read  INTEGER NOT NULL DEFAULT 0,
	cache_write INTEGER NOT NULL DEFAULT 0,
	reasoning   INTEGER NOT NULL DEFAULT 0,
	saved_tok   INTEGER NOT NULL DEFAULT 0,
	estimated   INTEGER NOT NULL DEFAULT 0,
	first_seen  TEXT,
	last_seen   TEXT,
	PRIMARY KEY (day, hour, provider, model, api_key, node_id)
);
INSERT INTO usage_rollup
	(day, hour, provider, model, api_key, node_id, requests, input_tok, output_tok, cache_read, cache_write, reasoning, saved_tok, estimated, first_seen, last_seen)
SELECT day, hour, provider, model, api_key, '', requests, input_tok, output_tok, cache_read, cache_write, reasoning, saved_tok, estimated, first_seen, last_seen
FROM usage_rollup_old;
DROP TABLE usage_rollup_old;
COMMIT;`
	_, err = s.db.Exec(rebuild)
	if err != nil {
		return err
	}
	// The rebuild dropped the old table together with its index; recreate
	// it on the sharded table.
	if _, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_rollup_day ON usage_rollup(day)`); err != nil {
		return err
	}
	return s.migrateQuota() // issue #7 quota window state
}

// FlushBuckets upserts rollups. Implements usage.Sink.
func (s *Store) FlushBuckets(buckets []usage.Bucket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO usage_rollup
		(day, hour, provider, model, api_key, node_id, requests, input_tok, output_tok, cache_read, cache_write, reasoning, saved_tok, estimated, first_seen, last_seen)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(day, hour, provider, model, api_key, node_id) DO UPDATE SET
			requests = requests + excluded.requests,
			input_tok = input_tok + excluded.input_tok,
			output_tok = output_tok + excluded.output_tok,
			cache_read = cache_read + excluded.cache_read,
			cache_write = cache_write + excluded.cache_write,
			reasoning = reasoning + excluded.reasoning,
			saved_tok = saved_tok + excluded.saved_tok,
			estimated = estimated + excluded.estimated,
			first_seen = MIN(COALESCE(first_seen, excluded.first_seen), excluded.first_seen),
			last_seen = MAX(COALESCE(last_seen, excluded.last_seen), excluded.last_seen)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for i := range buckets {
		b := &buckets[i]
		est := 0
		if b.Estimated {
			est = 1
		}
		if _, err := stmt.Exec(b.Day, b.Hour, b.Provider, b.Model, b.APIKey, s.nodeID,
			b.Requests, b.InputTokens, b.OutputTokens, b.CacheRead, b.CacheWrite,
			b.Reasoning, b.SavedTokens, est,
			fmtTime(b.FirstSeen), fmtTime(b.LastSeen)); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// QueryRange aggregates rollups over [fromDay, toDay].
func (s *Store) QueryRange(fromDay, toDay string) ([]UsageRow, error) {
	rows, err := s.db.Query(`SELECT day, hour, provider, model, api_key,
			SUM(requests), SUM(input_tok), SUM(output_tok), SUM(cache_read), SUM(cache_write), SUM(reasoning), SUM(saved_tok)
		FROM usage_rollup WHERE day >= ? AND day <= ?
		GROUP BY day, hour, provider, model, api_key ORDER BY day, hour`, fromDay, toDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Day, &r.Hour, &r.Provider, &r.Model, &r.APIKey,
			&r.Requests, &r.InputTok, &r.OutputTok, &r.CacheRead, &r.CacheWrite, &r.Reasoning, &r.SavedTok); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageRow is one aggregated query result.
type UsageRow struct {
	Day        string `json:"day"`
	Hour       string `json:"hour"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	APIKey     string `json:"api_key"`
	Requests   int64  `json:"requests"`
	InputTok   int64  `json:"input"`
	OutputTok  int64  `json:"output"`
	CacheRead  int64  `json:"cacheRead"`
	CacheWrite int64  `json:"cacheWrite"`
	Reasoning  int64  `json:"reasoning"`
	SavedTok   int64  `json:"saved"`
}

func (s *Store) Prune(retentionDays int) (int64, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format("2006-01-02")
	res, err := s.db.Exec(`DELETE FROM usage_rollup WHERE day < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TotalsSince aggregates over a window of days.
func (s *Store) TotalsSince(days int) (requests, inTok, outTok int64, err error) {
	from := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	row := s.db.QueryRow(`SELECT COALESCE(SUM(requests),0), COALESCE(SUM(input_tok),0), COALESCE(SUM(output_tok),0)
		FROM usage_rollup WHERE day >= ?`, from)
	return requests, inTok, outTok, row.Scan(&requests, &inTok, &outTok)
}

// Close closes the DB.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
