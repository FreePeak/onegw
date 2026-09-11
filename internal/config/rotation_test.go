package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// #84: the rotation policy table must parse, validate, and keep an absent
// table equal to the shipped defaults.
func TestRotationConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "onegw.toml")
	os.WriteFile(path, []byte(`
[rotation]
cooldown_base = "3s"
cooldown_cap = "9s"
flap_threshold = 2
flap_open = "30s"
model_bench_ttl = "1m"

[[providers]]
name = "p"
kind = "openai"
api_key = "k"
[providers.rotation]
cooldown_base = "500ms"
`), 0o600)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Rotation.CooldownBase != "3s" || cfg.Rotation.FlapThreshold != 2 {
		t.Fatalf("global rotation not parsed: %+v", cfg.Rotation)
	}
	if cfg.Providers[0].Rotation.CooldownBase != "500ms" {
		t.Fatalf("provider rotation not parsed: %+v", cfg.Providers[0].Rotation)
	}
}

func TestRotationConfigDefaultsOnAbsence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "onegw.toml")
	os.WriteFile(path, []byte(`
[[providers]]
name = "p"
kind = "openai"
api_key = "k"
`), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Absent tables decode to the zero RotationCfg everywhere; the provider
	// package resolves those to the shipped constants.
	if cfg.Rotation != (RotationCfg{}) || cfg.Providers[0].Rotation != (RotationCfg{}) {
		t.Fatalf("absent tables must be zero: %+v %+v", cfg.Rotation, cfg.Providers[0].Rotation)
	}
}

func TestRotationConfigValidation(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"bad duration": `[rotation]
cooldown_base = "soon"`,
		"non-positive": `[rotation]
cooldown_cap = "0s"`,
		"cap below base": `[rotation]
cooldown_base = "30s"
cooldown_cap = "5s"`,
		"negative threshold": `[rotation]
flap_threshold = -1`,
	} {
		path := filepath.Join(dir, name+" toml")
		os.WriteFile(path, []byte(body), 0o600)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: Load must fail", name)
		}
	}
	// flap_threshold 0 means "unset" and must load fine.
	path := filepath.Join(dir, "zero-threshold.toml")
	os.WriteFile(path, []byte("[rotation]\nflap_threshold = 0\n"), 0o600)
	if _, err := Load(path); err != nil {
		t.Fatalf("flap_threshold 0 must mean unset: %v", err)
	}
	_ = time.Second
}
