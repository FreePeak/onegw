package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeListenConfig writes a minimal valid config, optionally with a
// [server] listen, so an override can be told apart from the default.
func writeListenConfig(t *testing.T, listen string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "onegw.toml")
	body := fmt.Sprintf(`
[server]
%[1]sdata_dir = %[2]q
admin_password = "secretpw"

[[providers]]
name = "p"
kind = "openai"
api_key = "k"
`, listenLine(listen), filepath.Join(dir, "data"))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func listenLine(listen string) string {
	if listen == "" {
		return ""
	}
	return fmt.Sprintf("listen = %q\n", listen)
}

// TestListenEnvOverride pins the defect: the README ("Set ONEGW_LISTEN or
// ONEGW_KEYS to override") and `onegw help` both promise that ONEGW_LISTEN
// overrides the listen address, but nothing read it while starting a
// gateway — install.sh only used it to WRITE the config, and
// update/apply.go only reads it for the update handoff. An isolated
// bring-up that set it therefore bound the config's port, which pointed at
// the live config is the live port.
func TestListenEnvOverride(t *testing.T) {
	t.Setenv("ONEGW_LISTEN", "127.0.0.1:18099")

	cfg, err := Load(writeListenConfig(t, "127.0.0.1:8080"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:18099" {
		t.Fatalf("ONEGW_LISTEN must override the file: got %q, want 127.0.0.1:18099", cfg.Server.Listen)
	}
}

// TestListenEnvTrimsAndIgnoresBlank pins the whitespace handling that
// mirrors ONEGW_TASK_ROUTING, and that a blank value is not an override.
func TestListenEnvTrimsAndIgnoresBlank(t *testing.T) {
	path := writeListenConfig(t, "127.0.0.1:8080")

	t.Setenv("ONEGW_LISTEN", "  127.0.0.1:18100  ")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:18100" {
		t.Fatalf("ONEGW_LISTEN must be trimmed: got %q", cfg.Server.Listen)
	}

	t.Setenv("ONEGW_LISTEN", "   ")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:8080" {
		t.Fatalf("a blank ONEGW_LISTEN must not override: got %q", cfg.Server.Listen)
	}
}

// TestListenFallsBackToDefault keeps the security default: loopback only.
func TestListenFallsBackToDefault(t *testing.T) {
	t.Setenv("ONEGW_LISTEN", "")

	cfg, err := Load(writeListenConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:8080" {
		t.Fatalf("want the loopback default, got %q", cfg.Server.Listen)
	}
}
