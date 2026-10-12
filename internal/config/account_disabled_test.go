package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAcctTOML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "onegw.toml")
	raw := "[server]\nlisten = \"127.0.0.1:0\"\n[auth]\nkeys = [\"k\"]\n" + body
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDisabledAccountKeptButFlagged: disabled = true on a
// [[providers.accounts]] row must decode onto Acct.Disabled. The live
// pool skips it at apply time (see server); Load keeps the row so the
// file and admin editor still see the parked key. Before this fix the
// field was silently ignored (no Acct.Disabled), so an exhausted key
// the operator had "disabled" kept rotating and burning 429s.
func TestDisabledAccountKeptButFlagged(t *testing.T) {
	path := writeAcctTOML(t, `
[[providers]]
name = "opencode"
kind = "opencode"
[[providers.accounts]]
name = "exhausted"
api_key = "sk-dead"
disabled = true
[[providers.accounts]]
name = "live"
api_key = "sk-live"
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := c.Providers[0]
	if len(p.Accounts) != 2 {
		t.Fatalf("accounts = %d (%+v), want both rows (disabled kept in Config)", len(p.Accounts), p.Accounts)
	}
	byName := map[string]Acct{}
	for _, a := range p.Accounts {
		byName[a.Name] = a
	}
	if !byName["exhausted"].Disabled || byName["exhausted"].APIKey != "sk-dead" {
		t.Fatalf("exhausted = %+v, want Disabled=true with key retained", byName["exhausted"])
	}
	if byName["live"].Disabled || byName["live"].APIKey != "sk-live" {
		t.Fatalf("live = %+v, want active", byName["live"])
	}
}

// TestHalfCommentedAccountHeaderDropped: commenting only the name/key of
// an account while leaving `[[providers.accounts]]` live leaves BurntSushi
// with an empty {Name:"", APIKey:""} shell. That shell used to enter the
// pool and get picked (empty bearer / inherited provider key). Drop it.
func TestHalfCommentedAccountHeaderDropped(t *testing.T) {
	path := writeAcctTOML(t, `
[[providers]]
name = "opencode"
kind = "opencode"
[[providers.accounts]]
# name = "harvey"
# api_key = "sk-h"
[[providers.accounts]]
name = "kton2"
api_key = "sk-k2"
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := c.Providers[0]
	if len(p.Accounts) != 1 {
		t.Fatalf("accounts = %d (%+v), want only kton2 (empty shell dropped)", len(p.Accounts), p.Accounts)
	}
	if p.Accounts[0].Name != "kton2" {
		t.Fatalf("account = %+v, want kton2", p.Accounts[0])
	}
}

// TestNamedKeylessAccountKept: a named row with no key is the legitimate
// pre-sign-in OAuth shape — must survive the empty-shell filter.
func TestNamedKeylessAccountKept(t *testing.T) {
	path := writeAcctTOML(t, `
[[providers]]
name = "xai"
kind = "openai"
base_url = "https://example.invalid"
[[providers.accounts]]
name = "soon"
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.Providers[0].Accounts) != 1 || c.Providers[0].Accounts[0].Name != "soon" {
		t.Fatalf("accounts = %+v, want the named keyless row kept", c.Providers[0].Accounts)
	}
}
