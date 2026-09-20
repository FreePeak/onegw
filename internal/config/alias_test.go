package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "onegw.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const aliasBase = `
[server]
data_dir = "memory"
[[providers]]
name = "p1"
kind = "openai"
api_key = "k"
models = ["m1"]
[[providers]]
name = "p2"
kind = "openai"
api_key = "k"
models = ["m2"]
[[combo]]
name = "pair"
targets = ["p1/m1", "p2/m2"]
`

func TestAliasConfigValid(t *testing.T) {
	for _, aliases := range []string{
		`fast = "p1/m1"`,
		`best = "pair"`,
		"a = \"b\"\nb = \"p1/m1\"",
	} {
		p := writeCfg(t, aliasBase+"\n[aliases]\n"+aliases+"\n")
		if _, err := Load(p); err != nil {
			t.Fatalf("aliases [%s] should load: %v", aliases, err)
		}
	}
}

func TestAliasValidationRejects(t *testing.T) {
	cases := map[string]string{
		"unknown provider":    `fast = "nope/m1"`,
		"unknown combo":       `fast = "nope"`,
		"bare word":           `fast = "justtext"`,
		"shadow provider":     `p1 = "p2/m2"`,
		"shadow combo":        `pair = "p1/m1"`,
		"alias with slash":    `"a/b" = "p1/m1"`,
		"cycle":               "a = \"b\"\nb = \"a\"",
		"case-variant dup":    "fast = \"p1/m1\"\nFAST = \"p2/m2\"",
		"case-variant shadow": `P1 = "p2/m2"`,
		"long chain":          "a1 = \"a2\"\na2 = \"a3\"\na3 = \"a4\"\na4 = \"a5\"\na5 = \"a6\"\na6 = \"a7\"\na7 = \"a8\"\na8 = \"a9\"\na9 = \"p1/m1\"",
	}
	for name, aliases := range cases {
		p := writeCfg(t, aliasBase+"\n[aliases]\n"+aliases+"\n")
		if _, err := Load(p); err == nil {
			t.Fatalf("%s: expected validation failure", name)
		}
	}
}

func TestAliasEmptyNameRejected(t *testing.T) {
	// TOML cannot express an empty key inline; parse a table with an empty
	// key via the long form is also impossible — so the guard is only
	// reachable programmatically. Exercise Validate directly.
	cfg := &Config{}
	cfg.Defaults()
	cfg.Providers = []ProviderCfg{{Name: "p1", Kind: "openai", APIKey: "k"}}
	cfg.Aliases = map[string]string{"": "p1/m1"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("empty alias name should be rejected")
	}
}
