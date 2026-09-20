// Package cursorimport extracts Cursor credentials from the Cursor IDE's
// local SQLite database (state.vscdb) — the cursorAuth/accessToken key —
// and formats them for onegw configuration. This mirrors 9router's
// open-sse/executors/cursor.js token source and src/app/api/oauth/cursor/auto-import/route.js.
//
// Usage:
//
//	onegw-cursor-import [flags]
//
// Flags:
//
//	-db path   path to state.vscdb (default: Cursor's globalStorage/state.vscdb)
//	-out path  write config fragment to path (default: stdout)
//	-json      output as JSON instead of TOML
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	_ "modernc.org/sqlite"
)

// cursorCreds holds what we extract from Cursor's local DB.
type cursorCreds struct {
	AccessToken string `json:"access_token"`
	MachineID   string `json:"machine_id,omitempty"`
}

// cursorDBPaths is the list of candidate state.vscdb paths on each
// platform (mirrors 9router's auto-import route.js).
var cursorDBPaths = map[string][]string{
	"darwin": {
		filepath.Join(homeDir(), "Library/Application Support/Cursor/User/globalStorage/state.vscdb"),
		filepath.Join(homeDir(), "Library/Application Support/Cursor - Insiders/User/globalStorage/state.vscdb"),
	},
	"linux": {
		filepath.Join(homeDir(), ".config/Cursor/User/globalStorage/state.vscdb"),
		filepath.Join(homeDir(), ".config/cursor/User/globalStorage/state.vscdb"),
	},
	"windows": {
		filepath.Join(os.Getenv("APPDATA"), "Cursor/User/globalStorage/state.vscdb"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs/Cursor/User/globalStorage/state.vscdb"),
	},
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}

func main() {
	dbPath := flag.String("db", "", "path to Cursor state.vscdb (auto-detected if empty)")
	outPath := flag.String("out", "", "write config fragment to file (default: stdout)")
	jsonOut := flag.Bool("json", false, "output as JSON instead of TOML")
	flag.Parse()

	creds, err := extractCursorToken(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cursorimport: %v\n", err)
		os.Exit(1)
	}

	var out []byte
	if *jsonOut {
		out, err = json.MarshalIndent(creds, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "cursorimport: %v\n", err)
			os.Exit(1)
		}
	} else {
		out = []byte(fmt.Sprintf(`# Cursor credentials extracted from state.vscdb
# Upstream API token (api_key) — Bearer to api2.cursor.sh / agent.api5.cursor.sh
# Quota probe token (dashboard_token) — WorkosCursorSessionToken cookie for cursor.com/api/usage-summary
# Copy dashboard_token from your browser session: log into cursor.com/dashboard,
# inspect the WorkosCursorSessionToken cookie, keep the full cookie value.
cursor_api_key = "%s"
`, creds.AccessToken))
	}

	if *outPath != "" {
		if err := os.WriteFile(*outPath, out, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "cursorimport: %v\n", err)
			os.Exit(1)
		}
	} else {
		os.Stdout.Write(out)
	}
}

// extractCursorToken reads cursorAuth/accessToken from Cursor's
// state.vscdb. If dbPath is empty, it auto-detects the file on the
// current platform.
func extractCursorToken(dbPath string) (*cursorCreds, error) {
	if dbPath == "" {
		var found string
		for _, p := range cursorDBPaths[goOS()] {
			if _, err := os.Stat(p); err == nil {
				found = p
				break
			}
		}
		if found == "" {
			return nil, fmt.Errorf("no Cursor state.vscdb found — set -db explicitly")
		}
		dbPath = found
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open cursor db: %v", err)
	}
	defer db.Close()

	var accessToken string
	err = db.QueryRow("SELECT value FROM itemTable WHERE key='cursorAuth/accessToken'").Scan(&accessToken)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("cursorAuth/accessToken not found in %s", dbPath)
		}
		return nil, fmt.Errorf("query cursor token: %v", err)
	}
	if accessToken == "" {
		return nil, fmt.Errorf("cursorAuth/accessToken is empty in %s", dbPath)
	}

	creds := &cursorCreds{AccessToken: accessToken}

	// Also try to grab the machine ID (used for x-cursor-checksum)
	var machineID string
	_ = db.QueryRow("SELECT value FROM itemTable WHERE key='storage.serviceMachineId'").Scan(&machineID)
	if machineID != "" {
		creds.MachineID = machineID
	}

	return creds, nil
}

func goOS() string {
	return runtime.GOOS
}
