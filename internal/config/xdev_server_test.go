package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

// docker/xdev-server.toml is the shipped gateway config. A typo there is a
// failed first boot, so Load must accept it without env (keys come from
// compose .env at runtime).
func TestXdevServerTomlLoads(t *testing.T) {
	_, this, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	path := filepath.Join(filepath.Dir(this), "..", "..", "docker", "xdev-server.toml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load xdev-server.toml: %v", err)
	}
	if got := cfg.Server.Listen; got != "0.0.0.0:8080" {
		t.Errorf("listen = %q", got)
	}
	names := map[string]bool{}
	for _, p := range cfg.Providers {
		names[p.Name] = true
	}
	for _, want := range []string{"opencode", "opencode-free"} {
		if !names[want] {
			t.Errorf("missing provider %s", want)
		}
	}
	combos := map[string][]string{}
	for _, c := range cfg.Combos {
		combos[c.Name] = c.Targets
	}
	if got := combos["free"]; len(got) < 2 || got[0] != "opencode/deepseek-v4.1-flash" {
		t.Errorf("combo free = %v, want Go flash first", got)
	}
	if got := combos["xdev"]; len(got) == 0 || got[0] != "opencode/deepseek-v4.1-flash" {
		t.Errorf("combo xdev = %v", got)
	}
}
