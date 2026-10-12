package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestComboMayReferenceMissingProvider: commenting out or deleting a
// provider must not brick reload when combos still list it as a fallback
// leg. Router.Execute already skips unknown providers at runtime; Validate
// used to refuse the load with "combo X references unknown provider Y",
// which is exactly the failure operators hit when parking xiaomi by hand.
func TestComboMayReferenceMissingProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onegw.toml")
	raw := `
[server]
listen = "127.0.0.1:0"
[auth]
keys = ["k"]
[[providers]]
name = "opencode"
kind = "opencode"
api_key = "sk-live"
[[combo]]
name = "dev"
targets = ["opencode/m", "xiaomi/mimo-v2.6-flash"]
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load with dangling combo leg: %v", err)
	}
	if len(c.Combos) != 1 || len(c.Combos[0].Targets) != 2 {
		t.Fatalf("combos = %+v, want the dangling leg kept", c.Combos)
	}
	if len(c.Providers) != 1 || c.Providers[0].Name != "opencode" {
		t.Fatalf("providers = %+v, want only opencode", c.Providers)
	}
}

// TestAliasStillRequiresKnownProvider: aliases are a single hop the client
// types directly — a typo there must still fail the load. Only combo
// fallback legs are allowed to dangle.
func TestAliasStillRequiresKnownProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onegw.toml")
	raw := `
[server]
listen = "127.0.0.1:0"
[auth]
keys = ["k"]
[[providers]]
name = "opencode"
kind = "opencode"
api_key = "sk-live"
[aliases]
fast = "xiaomi/m"
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("alias to missing provider must fail, got %v", err)
	}
}
