package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A relative data_dir must anchor to the config file's directory, not the
// launcher's cwd: the gateway has been silently reopened on an empty
// usage.db after an install.sh / `onegw update` restart from another
// directory (2026-09-09 data-loss incident).
func TestLoadAnchorsRelativeDataDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "onegw.toml")
	if err := os.WriteFile(path, []byte("[server]\ndata_dir = \"data\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "data"); cfg.Server.DataDir != want {
		t.Fatalf("data_dir = %q, want %q (anchored to the config file's directory)", cfg.Server.DataDir, want)
	}
}

func TestLoadDataDirSpecialValuesUntouched(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "elsewhere")

	// Absolute paths pass through unchanged.
	path := filepath.Join(dir, "abs.toml")
	if err := os.WriteFile(path, []byte("[server]\ndata_dir = \""+abs+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.DataDir != abs {
		t.Fatalf("absolute data_dir = %q, want unchanged %q", cfg.Server.DataDir, abs)
	}

	// "memory" is the in-store test sentinel, never a path.
	path = filepath.Join(dir, "mem.toml")
	if err := os.WriteFile(path, []byte("[server]\ndata_dir = \"memory\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.DataDir != "memory" {
		t.Fatalf("sentinel data_dir = %q, want \"memory\"", cfg.Server.DataDir)
	}
}

// No data_dir in the config falls back to the absolute default (~/.onegw or
// ONEGW_DATA_DIR) — never cwd-relative.
func TestLoadDefaultDataDirAbsolute(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "onegw.toml")
	if err := os.WriteFile(path, []byte("[server]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.Server.DataDir) {
		t.Fatalf("default data_dir = %q, want absolute", cfg.Server.DataDir)
	}
}
