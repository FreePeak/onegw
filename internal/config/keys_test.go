package config

import (
	"os"
	"path/filepath"
	"testing"
)

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
	}
}
