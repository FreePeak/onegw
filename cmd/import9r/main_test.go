package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// seedDB creates a 9router-shaped SQLite DB with one commandcode apikey
// connection (no base_url — the builtin map must supply it) and one cursor
// oauth connection (must stay skipped).
func seedDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "data.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := `CREATE TABLE providerConnections (provider TEXT, name TEXT, priority INTEGER, data TEXT, authType TEXT, isActive INTEGER, testStatus TEXT);
CREATE TABLE apiKeys (key TEXT, isActive INTEGER);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	cc, _ := json.Marshal(map[string]any{
		"apiKey": "user_test_cckey",
	})
	cursor, _ := json.Marshal(map[string]any{
		"accessToken": "tok_cursor",
	})
	rows := []struct {
		prov, name         string
		prio               int
		data, auth, status string
	}{
		{"commandcode", "cc-main", 1, string(cc), "apikey", "active"},
		{"cursor", "cur-main", 1, string(cursor), "oauth", "active"},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO providerConnections VALUES (?,?,?,?,?,1,?)`,
			r.prov, r.name, r.prio, r.data, r.auth, r.status); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO apiKeys VALUES ('sk-gw-test', 1)`); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

func TestImportCommandCodeConnection(t *testing.T) {
	dbPath := seedDB(t)
	out := filepath.Join(t.TempDir(), "imported.toml")
	if code := runImport(dbPath, out, true); code != 0 {
		t.Fatalf("import exited %d", code)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `kind = "commandcode"`) {
		t.Fatalf("commandcode kind missing: %s", s)
	}
	if !strings.Contains(s, `base_url = "https://api.commandcode.ai/alpha/generate"`) {
		t.Fatalf("commandcode base url missing: %s", s)
	}
	if !strings.Contains(s, "user_test_cckey") {
		t.Fatalf("api key missing: %s", s)
	}
	if strings.Contains(s, "tok_cursor") || strings.Contains(s, `kind = "cursor"`) {
		t.Fatalf("cursor must stay skipped: %s", s)
	}
	if !strings.Contains(s, "sk-gw-test") {
		t.Fatalf("gateway keys missing: %s", s)
	}
	// No model discovery for commandcode (no /models endpoint): the provider
	// must appear without a models list.
	sec := s[strings.Index(s, `kind = "commandcode"`):]
	if strings.Contains(sec[:min(len(sec), 400)], "models") {
		t.Fatalf("unexpected models discovery for commandcode: %s", s)
	}
}
