package store

// Quota window state persistence (issue #7): one small table keyed by
// provider, upserted on the quota tracker's flush tick. Window counters for
// history that predates this table are rebuilt from usage_rollup instead.

import (
	"database/sql"
	"time"
)

// QuotaState is the persisted quota window state for one provider.
type QuotaState struct {
	Provider     string `json:"provider"`
	Window       string `json:"window"`
	WindowStart  string `json:"window_start"` // RFC3339
	UsedTokens   int64  `json:"used_tokens"`
	UsedRequests int64  `json:"used_requests"`
}

const quotaDDL = `
CREATE TABLE IF NOT EXISTS quota_state (
	provider      TEXT PRIMARY KEY,
	window        TEXT NOT NULL,
	window_start  TEXT NOT NULL,
	used_tokens   INTEGER NOT NULL DEFAULT 0,
	used_requests INTEGER NOT NULL DEFAULT 0,
	updated_at    TEXT
);
`

// migrateQuota creates the quota_state table (called from migrate).
func (s *Store) migrateQuota() error {
	_, err := s.db.Exec(quotaDDL)
	return err
}

// LoadQuotaState returns all persisted quota rows.
func (s *Store) LoadQuotaState() ([]QuotaState, error) {
	rows, err := s.db.Query(`SELECT provider, window, window_start, used_tokens, used_requests FROM quota_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaState
	for rows.Next() {
		var r QuotaState
		if err := rows.Scan(&r.Provider, &r.Window, &r.WindowStart, &r.UsedTokens, &r.UsedRequests); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveQuotaState upserts the given rows (replaces counters per provider).
func (s *Store) SaveQuotaState(rows []QuotaState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO quota_state
		(provider, window, window_start, used_tokens, used_requests, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(provider) DO UPDATE SET
			window = excluded.window,
			window_start = excluded.window_start,
			used_tokens = excluded.used_tokens,
			used_requests = excluded.used_requests,
			updated_at = excluded.updated_at`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	now := fmtTime(time.Now())
	for _, r := range rows {
		if _, err := stmt.Exec(r.Provider, r.Window, r.WindowStart, r.UsedTokens, r.UsedRequests, now); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// QuotaFirstSeen returns the earliest first_seen recorded for a provider's
// rollups — the best available anchor for a 5h rolling window rebuilt from
// usage history alone.
func (s *Store) QuotaFirstSeen(provider string) (time.Time, error) {
	var v sql.NullString
	if err := s.db.QueryRow(`SELECT MIN(first_seen) FROM usage_rollup WHERE provider = ?`, provider).Scan(&v); err != nil {
		return time.Time{}, err
	}
	if !v.Valid || v.String == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, v.String)
}
