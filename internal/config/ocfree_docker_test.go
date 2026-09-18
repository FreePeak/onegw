package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"onegw/internal/config"
)

// Verifies the baked docker/onegw.default.toml ships a working
// preconfigured opencode-free provider + free combo, so a fresh
// `docker compose up` serves the dashboard on day one.
func TestDockerDefaultPreconfigured(t *testing.T) {
	toml := dockerTomlPath(t)
	cfg, err := config.Load(toml)
	if err != nil {
		t.Fatalf("load %s: %v", toml, err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "opencode-free" {
		t.Fatalf("expected exactly provider opencode-free, got %v",
			providerNames(cfg))
	}
	if cfg.Providers[0].Kind != "opencode-free" {
		t.Fatalf("kind: want opencode-free, got %q", cfg.Providers[0].Kind)
	}
	if len(cfg.Providers[0].Models) == 0 {
		t.Fatal("opencode-free provider has no models")
	}
	if len(cfg.Combos) != 1 || cfg.Combos[0].Name != "free" {
		t.Fatalf("expected exactly combo free, got %v", comboNames(cfg))
	}
	targets := cfg.Combos[0].Targets
	if len(targets) != 1 || targets[0] != "opencode-free/big-pickle" {
		t.Fatalf("free combo targets: want [opencode-free/big-pickle], got %v", targets)
	}
	// opencode-free is keyless by design — must have no api_key/keys/accounts.
	p := cfg.Providers[0]
	if p.APIKey != "" || len(p.Keys) != 0 {
		t.Fatal("opencode-free must be keyless (no api_key, no keys)")
	}
	if len(p.Accounts) != 0 {
		t.Fatalf("opencode-free must have no accounts, got %d", len(p.Accounts))
	}
}

// dockerTomlPath walks up from the test file's cwd (go test runs
// from a cache dir, not the source tree) until it finds
// docker/onegw.default.toml, then returns its absolute path.
func dockerTomlPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(wd, "docker", "onegw.default.toml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		if wd == filepath.Dir(wd) {
			break
		}
		wd = filepath.Dir(wd)
	}
	t.Fatalf("could not locate docker/onegw.default.toml from %s", wd)
	return ""
}

func providerNames(cfg *config.Config) []string {
	out := []string{}
	for _, p := range cfg.Providers {
		out = append(out, p.Name)
	}
	return out
}

func comboNames(cfg *config.Config) []string {
	out := []string{}
	for _, c := range cfg.Combos {
		out = append(out, c.Name)
	}
	return out
}
