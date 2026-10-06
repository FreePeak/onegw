package store

// Responses affinity and history (POST /v1/responses). A Responses object
// lives in the upstream account that created it: a previous_response_id
// continuation, or a GET/DELETE on the id, only works against that same
// account. Each served response records where it was served (affinity) and,
// when [responses] history is on, the turn's input and output items — the
// material needed to rebuild the conversation on another account when the
// pinned one cannot serve any more.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ResponseRow is one served response. Input and Output are JSON arrays of
// Responses items; both are "" when history is off.
type ResponseRow struct {
	ID       string
	Provider string
	Account  string
	Model    string
	APIKey   string // client key label that created it
	PrevID   string // previous_response_id the turn continued, "" for a first turn
	Created  time.Time
	Input    string
	Output   string
}

func (s *Store) migrateResponses() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS responses (
	id       TEXT PRIMARY KEY,
	provider TEXT NOT NULL,
	account  TEXT NOT NULL,
	model    TEXT NOT NULL,
	api_key  TEXT NOT NULL,
	prev_id  TEXT NOT NULL DEFAULT '',
	created  INTEGER NOT NULL,
	input    TEXT NOT NULL DEFAULT '',
	output   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_responses_created ON responses(created);`)
	return err
}

// PutResponse inserts or replaces one response row.
func (s *Store) PutResponse(r ResponseRow) error {
	if r.ID == "" || r.Provider == "" {
		return fmt.Errorf("response row needs an id and a provider")
	}
	if r.Created.IsZero() {
		r.Created = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO responses (id, provider, account, model, api_key, prev_id, created, input, output)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET provider = excluded.provider, account = excluded.account,
			model = excluded.model, api_key = excluded.api_key, prev_id = excluded.prev_id,
			created = excluded.created, input = excluded.input, output = excluded.output`,
		r.ID, r.Provider, r.Account, r.Model, r.APIKey, r.PrevID, r.Created.Unix(), r.Input, r.Output)
	return err
}

// GetResponse returns the row for id; ok=false when it is unknown.
func (s *Store) GetResponse(id string) (ResponseRow, bool, error) {
	var r ResponseRow
	var created int64
	err := s.db.QueryRow(`SELECT id, provider, account, model, api_key, prev_id, created, input, output
		FROM responses WHERE id = ?`, id).
		Scan(&r.ID, &r.Provider, &r.Account, &r.Model, &r.APIKey, &r.PrevID, &created, &r.Input, &r.Output)
	if errors.Is(err, sql.ErrNoRows) {
		return ResponseRow{}, false, nil
	}
	if err != nil {
		return ResponseRow{}, false, err
	}
	r.Created = time.Unix(created, 0)
	return r, true, nil
}

// DeleteResponse drops one row (no-op if absent).
func (s *Store) DeleteResponse(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM responses WHERE id = ?`, id)
	return err
}

// PruneResponses deletes rows created before cutoff and returns how many.
func (s *Store) PruneResponses(cutoff time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM responses WHERE created < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
