package config

import (
	"os"
	"path/filepath"
	"testing"
)

<<<<<<< HEAD
func TestKeysExpandToAccounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "onegw.toml")
	if err := os.WriteFile(path, []byte(`
[[providers]]
name = "opencode"
kind = "opencode"
keys = ["k1", "  ", "k2"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := c.Providers[0]
	if len(p.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2 (blank keys skipped)", len(p.Accounts))
	}
	if p.Accounts[0].Name != "key-1" || p.Accounts[0].APIKey != "k1" {
		t.Fatalf("account[0] = %+v", p.Accounts[0])
	}
	if p.Accounts[1].Name != "key-3" || p.Accounts[1].APIKey != "k2" {
		t.Fatalf("account[1] = %+v (index must follow the config order)", p.Accounts[1])
	}
}

func TestOpencodeKindAccepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "onegw.toml")
	if err := os.WriteFile(path, []byte(`
[[providers]]
name = "opencode"
kind = "opencode"
keys = ["k1", "k2"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load+validate: %v", err)
	}
	if len(c.Providers[0].Accounts) != 2 {
		t.Fatal("keys should expand to two accounts")
	}
}

func TestEnvNumberedKeysBecomeAccounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "onegw.toml")
	if err := os.WriteFile(path, []byte(`
[[providers]]
name = "opencode"
kind = "opencode"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ONEGW_PROVIDER_OPENCODE_KEY", "k1")
	t.Setenv("ONEGW_PROVIDER_OPENCODE_KEY2", "k2")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := c.Providers[0]
	if len(p.Accounts) != 2 || p.Accounts[0].APIKey != "k1" || p.Accounts[1].Name != "key-2" {
		t.Fatalf("accounts = %+v, want env k1+k2 rotation", p.Accounts)
=======
func writeTOML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "onegw.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFlatKeysBackwardCompat(t *testing.T) {
	path := writeTOML(t, `
[[providers]]
name = "p1"
kind = "openai"
api_key = "up-key"
models = ["m1"]
[auth]
keys = ["sk-test-legacy-1", "sk-test-legacy-2"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Auth.KeyList) != 2 {
		t.Fatalf("KeyList = %d entries, want 2", len(cfg.Auth.KeyList))
	}
	for i, want := range []string{"sk-test-legacy-1", "sk-test-legacy-2"} {
		if cfg.Auth.KeyList[i].Key != want {
			t.Errorf("entry %d = %q, want %q", i, cfg.Auth.KeyList[i].Key, want)
		}
		if cfg.Auth.KeyList[i].RPM != 0 || cfg.Auth.KeyList[i].TPM != 0 || len(cfg.Auth.KeyList[i].Models) != 0 {
			t.Errorf("flat entry %d must be unlimited", i)
		}
	}
}

func TestLoadPolicyTables(t *testing.T) {
	path := writeTOML(t, `
[[providers]]
name = "p1"
kind = "openai"
api_key = "up-key"
models = ["m1", "m2"]
[[combos]]
name = "both"
targets = ["p1/m1", "p1/m2"]
[[auth.keys]]
key = "sk-test-policy"
name = "teamA"
rpm = 30
tpm = 10000
models = ["p1/m1", "both", "m2"]
[[auth.keys]]
key = "sk-test-limited"
name = "teamB"
rpm = 5
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Auth.KeyList) != 2 {
		t.Fatalf("KeyList = %d entries, want 2", len(cfg.Auth.KeyList))
	}
	a := cfg.Auth.KeyList[0]
	if a.Key != "sk-test-policy" || a.Name != "teamA" || a.RPM != 30 || a.TPM != 10000 {
		t.Errorf("entry 0 = %+v", a)
	}
	if len(a.Models) != 3 || a.Models[0] != "p1/m1" || a.Models[1] != "both" || a.Models[2] != "m2" {
		t.Errorf("models = %v", a.Models)
	}
	if cfg.Auth.KeyList[1].TPM != 0 {
		t.Errorf("entry 1 tpm = %d, want 0 (unlimited)", cfg.Auth.KeyList[1].TPM)
	}
}

func TestLoadMixedShapesRejectsNothingButFlatWinsOnlyOneShape(t *testing.T) {
	// The two shapes cannot coexist in one TOML value at the same path;
	// attempting both is a parse-time error which Load surfaces.
	path := writeTOML(t, `
[[providers]]
name = "p1"
kind = "openai"
api_key = "up-key"
models = ["m1"]
[auth]
keys = ["sk-test-flat"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Auth.KeyList) != 1 {
		t.Fatalf("KeyList = %d, want 1", len(cfg.Auth.KeyList))
	}
}

func TestLoadMalformedKeyShape(t *testing.T) {
	path := writeTOML(t, `
[[providers]]
name = "p1"
kind = "openai"
api_key = "up-key"
models = ["m1"]
[auth]
keys = 42
`)
	if _, err := Load(path); err == nil {
		t.Fatal("load accepted auth.keys = 42")
	}
}

func TestLoadDuplicateKeyRejected(t *testing.T) {
	path := writeTOML(t, `
[[providers]]
name = "p1"
kind = "openai"
api_key = "up-key"
models = ["m1"]
[[auth.keys]]
key = "sk-test-dup"
[[auth.keys]]
key = "sk-test-dup"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("load accepted duplicate keys")
	}
}

func TestLoadEmptyKeyRejected(t *testing.T) {
	path := writeTOML(t, `
[[providers]]
name = "p1"
kind = "openai"
api_key = "up-key"
models = ["m1"]
[[auth.keys]]
key = ""
name = "blank"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("load accepted empty key")
	}
}

func TestLoadNegativeLimitsRejected(t *testing.T) {
	path := writeTOML(t, `
[[providers]]
name = "p1"
kind = "openai"
api_key = "up-key"
models = ["m1"]
[[auth.keys]]
key = "sk-test-neg"
rpm = -1
`)
	if _, err := Load(path); err == nil {
		t.Fatal("load accepted negative rpm")
	}
}

func TestLoadEmptyModelsEntryRejected(t *testing.T) {
	path := writeTOML(t, `
[[providers]]
name = "p1"
kind = "openai"
api_key = "up-key"
models = ["m1"]
[[auth.keys]]
key = "sk-test-empty-model"
models = [""]
`)
	if _, err := Load(path); err == nil {
		t.Fatal("load accepted empty models entry")
	}
}

func TestValidateNoKeysOpenGateway(t *testing.T) {
	cfg := &Config{}
	cfg.Providers = append(cfg.Providers, ProviderCfg{Name: "p1", Kind: "openai", APIKey: "up-key", Models: []string{"m1"}})
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("open gateway invalid: %v", err)
	}
	if len(cfg.Auth.KeyList) != 0 {
		t.Fatalf("KeyList = %d, want 0", len(cfg.Auth.KeyList))
	}
}

func TestEnvKeysOverrideFileKeys(t *testing.T) {
	t.Setenv("ONEGW_KEYS", "user_test_env1, user_test_env2")
	path := writeTOML(t, `
[[providers]]
name = "p1"
kind = "openai"
api_key = "up-key"
models = ["m1"]
[[auth.keys]]
key = "sk-test-file-key"
rpm = 10
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Auth.KeyList) != 2 {
		t.Fatalf("KeyList = %d entries, want 2 from env", len(cfg.Auth.KeyList))
	}
	if cfg.Auth.KeyList[0].Key != "user_test_env1" || cfg.Auth.KeyList[1].Key != "user_test_env2" {
		t.Errorf("env keys not used: %+v", cfg.Auth.KeyList)
	}
	if cfg.Auth.KeyList[0].RPM != 0 {
		t.Errorf("env keys must be unlimited, rpm = %d", cfg.Auth.KeyList[0].RPM)
	}
}

func TestMaskKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sk-test-abcd1234ef56", "sk-t***56"},
		{"shortkey", "***"},
		{"12345678", "***"},
		{"123456789", "1234***89"},
	}
	for _, c := range cases {
		if got := maskKey(c.in); got != c.want {
			t.Errorf("maskKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestKeyLabelPrefersName(t *testing.T) {
	ak := &AuthKey{Key: "sk-test-abcd1234ef56", Name: "teamA"}
	if ak.Label() != "teamA" {
		t.Errorf("Label() = %q, want teamA", ak.Label())
	}
	if got := (&AuthKey{Key: "sk-test-abcd1234ef56"}).Label(); got != "sk-t***56" {
		t.Errorf("Label() = %q, want sk-t***56", got)
>>>>>>> 7aae1c8 (feat(auth): per-key rpm/tpm limits and model allowlists (#3))
	}
}
