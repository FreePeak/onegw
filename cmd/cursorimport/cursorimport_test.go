package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestExtractCursorToken(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE itemTable (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`INSERT INTO itemTable (key, value) VALUES ('cursorAuth/accessToken', 'eyJ0eXAiOiJKV1QiLCJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhdXRoMHx1c2VyXzAxSzdCV1NZNkJLUEszQVJYRlBEQ1FHSFM1IiwidHlwZSI6InNlc3Npb24iLCJleHAiOjE3ODg3OTEwOTB9.sig')`); err != nil {
		t.Fatal(err)
	}

	creds, err := extractCursorToken(dbPath)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if creds.AccessToken == "" {
		t.Fatal("access token empty")
	}
	if creds.MachineID != "" {
		t.Fatalf("machine id should be empty, got %q", creds.MachineID)
	}
}

func TestExtractCursorTokenMissing(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE itemTable (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}

	_, err = extractCursorToken(dbPath)
	if err == nil {
		t.Fatal("expected error for missing cursorAuth/accessToken")
	}
	if !strings.Contains(err.Error(), "cursorAuth/accessToken") {
		t.Fatalf("expected cursorAuth/accessToken error, got: %v", err)
	}
}

func TestExtractCursorTokenWithMachineID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE itemTable (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO itemTable (key, value) VALUES ('cursorAuth/accessToken', 'tok-cursor-123')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO itemTable (key, value) VALUES ('storage.serviceMachineId', 'b9854c5c-64ac-418c-bd14-68085958ef73')`); err != nil {
		t.Fatal(err)
	}

	creds, err := extractCursorToken(dbPath)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if creds.MachineID != "b9854c5c-64ac-418c-bd14-68085958ef73" {
		t.Fatalf("machine id = %q, want %q", creds.MachineID, "b9854c5c-64ac-418c-bd14-68085958ef73")
	}
}
