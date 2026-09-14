package store

// Provider presets: a small catalog table (9router's providerConnections
// shape — name + JSON doc), seeded once by the server with the built-in
// provider recipes (glm/z.ai, xAI/Grok, opencode-go) so the dashboard's
// provider editor pre-fills without typing. The catalog is NOT live config:
// [[providers]] still lives in TOML, and applying a preset is a UI action
// that fills the editor, followed by the normal validated save.

import "fmt"

// PresetRow is one catalog entry: a display name and its JSON document
// (kind, base_url, models, …). The doc's schema is owned by the server
// package (presetDoc there); the store keeps it opaque.
type PresetRow struct {
	Name string
	Doc  string
}

func (s *Store) migratePresets() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS provider_presets (
	name TEXT PRIMARY KEY,
	doc  TEXT NOT NULL
);`)
	return err
}

// PresetCount reports how many catalog rows exist (seeding guard).
func (s *Store) PresetCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM provider_presets`).Scan(&n)
	return n, err
}

// ListPresets returns the catalog sorted by name (stable UI order).
func (s *Store) ListPresets() ([]PresetRow, error) {
	rows, err := s.db.Query(`SELECT name, doc FROM provider_presets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PresetRow
	for rows.Next() {
		var p PresetRow
		if err := rows.Scan(&p.Name, &p.Doc); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertPreset inserts or replaces one catalog entry.
func (s *Store) UpsertPreset(name, doc string) error {
	if name ***REMOVED*** "" || doc ***REMOVED*** "" {
		return fmt.Errorf("preset needs a name and a document")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO provider_presets (name, doc) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET doc = excluded.doc`, name, doc)
	return err
}

// DeletePreset drops one catalog entry (no-op if absent).
func (s *Store) DeletePreset(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM provider_presets WHERE name = ?`, name)
	return err
}
