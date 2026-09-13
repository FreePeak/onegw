package oauthcmd

import (
	"os"
	"path/filepath"
	"testing"
)

// The container is the case that matters: the image sets ONEGW_DATA_DIR=/data
// and mounts the volume there, while the config it ships says data_dir="/data".
// A login that lands anywhere else writes a token the gateway never reads, so
// the resolution order is pinned here.
func TestResolveDataDirPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "onegw.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\ndata_dir = \""+filepath.Join(dir, "from-config")+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("explicit flag wins", func(t *testing.T) {
		t.Setenv("ONEGW_DATA_DIR", filepath.Join(dir, "from-env"))
		if got := resolveDataDir(filepath.Join(dir, "flag"), cfgPath); got != filepath.Join(dir, "flag") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("config wins over the env override", func(t *testing.T) {
		// config.Load resolves server.data_dir itself (config, then env, then
		// ~/.onegw), so the CLI must ask it rather than re-implement the order:
		// an explicit data_dir in the file is what the gateway actually uses.
		t.Setenv("ONEGW_DATA_DIR", filepath.Join(dir, "from-env"))
		if got := resolveDataDir("", cfgPath); got != filepath.Join(dir, "from-config") {
			t.Fatalf("got %q, want the config's data_dir", got)
		}
	})

	t.Run("env when there is no config", func(t *testing.T) {
		t.Setenv("ONEGW_DATA_DIR", filepath.Join(dir, "from-env"))
		missing := filepath.Join(dir, "absent.toml")
		if got := resolveDataDir("", missing); got != filepath.Join(dir, "from-env") {
			t.Fatalf("got %q, want the env value", got)
		}
	})

	t.Run("home fallback", func(t *testing.T) {
		t.Setenv("ONEGW_DATA_DIR", "")
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home dir")
		}
		if got := resolveDataDir("", filepath.Join(dir, "absent.toml")); got != home+"/.onegw" {
			t.Fatalf("got %q, want ~/.onegw", got)
		}
	})
}

// A missing subcommand must not silently do something else.
func TestRunRejectsUnknownCommand(t *testing.T) {
	if code := Run([]string{"logni"}); code != 2 {
		t.Fatalf("typo exit code = %d, want 2", code)
	}
	if code := Run(nil); code != 2 {
		t.Fatalf("no command exit code = %d, want 2", code)
	}
	if code := Run([]string{"help"}); code != 0 {
		t.Fatalf("help exit code = %d, want 0", code)
	}
}

// A mounted config that redirects the device flow (self-hosted IdP, staging,
// tests) must be honoured by the CLI exactly as it is by the dashboard's
// sign-in — otherwise `onegw oauth login` silently reaches the real vendor
// while the config says otherwise, and a "test" login rotates the operator's
// live session.
func TestProfileHonoursConfigEntryOverrides(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "onegw.toml")
	cfg := `[server]
data_dir = "` + dir + `"

[[providers]]
name = "grokbuild2"
kind = "openai"
base_url = "http://up.invalid"
models = ["m1"]

[[providers.accounts]]
name = "main"

[[oauth.accounts]]
provider   = "grokbuild2"
account    = "main"
service    = "xai"
device_url = "http://127.0.0.1:9/device"
token_url  = "http://127.0.0.1:9/token"
client_id  = "from-config"
scope      = "api:access"
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	o, err := newOpts("login", []string{"-provider", "grokbuild2", "-account", "main", "-config", cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	p, err := o.profile()
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "xai" {
		t.Fatalf("profile = %q, want the entry's service profile", p.Name)
	}
	if p.DeviceCodeURL != "http://127.0.0.1:9/device" || p.TokenURL != "http://127.0.0.1:9/token" {
		t.Fatalf("config endpoints ignored: %s / %s", p.DeviceCodeURL, p.TokenURL)
	}
	if p.ClientID != "from-config" || p.Scope != "api:access" {
		t.Fatalf("config client/scope ignored: %q %q", p.ClientID, p.Scope)
	}
	if o.dataDir != dir {
		t.Fatalf("data dir = %q, want the config's", o.dataDir)
	}

	// Explicit flags still win: staging overrides without editing the file.
	o2, err := newOpts("login", []string{"-provider", "grokbuild2", "-account", "main",
		"-config", cfgPath, "-device-url", "http://flag.invalid/device", "-service", "kilocode"})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := o2.profile()
	if err != nil {
		t.Fatal(err)
	}
	if p2.Name != "kilocode" || p2.DeviceCodeURL != "http://flag.invalid/device" {
		t.Fatalf("-service/-device-url must win: %q %s", p2.Name, p2.DeviceCodeURL)
	}
}
