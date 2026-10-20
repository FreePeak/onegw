package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The [free] table defaults: the catalog refresh runs daily against
// models.dev, and the table can be turned off explicitly.
func TestFreeCatalogDefaults(t *testing.T) {
	c := &Config{}
	c.Defaults()
	if got := c.Free.SyncValue(); got != DefaultFreeCatalogSync {
		t.Fatalf("catalog_sync = %q, want %q", got, DefaultFreeCatalogSync)
	}
	if got := c.Free.SyncInterval(); got != 24*time.Hour {
		t.Fatalf("sync interval = %s, want 24h", got)
	}
	if c.Free.CatalogURL != DefaultFreeCatalogURL {
		t.Fatalf("catalog_url = %q, want %q", c.Free.CatalogURL, DefaultFreeCatalogURL)
	}
}

// An explicit catalog_sync = "" turns the refresh off; the pointer keeps
// that apart from an absent table.
func TestFreeCatalogOff(t *testing.T) {
	cases := []struct {
		val string
		off bool
	}{
		{"", true},
		{"off", true},
		{"0", true},
		{"none", true},
		{"24h", false},
		{"30m", false},
	}
	for _, tc := range cases {
		c := &Config{Free: FreeCfg{CatalogSync: &tc.val}}
		c.Defaults()
		got := c.Free.SyncInterval()
		if tc.off && got != 0 {
			t.Fatalf("catalog_sync %q: interval = %s, want off", tc.val, got)
		}
		if !tc.off && got <= 0 {
			t.Fatalf("catalog_sync %q: interval = %s, want a cadence", tc.val, got)
		}
	}
}

// The env overrides follow the repo's ONEGW_* convention.
func TestFreeCatalogEnvOverride(t *testing.T) {
	t.Setenv("ONEGW_FREE_CATALOG_SYNC", "45m")
	t.Setenv("ONEGW_FREE_CATALOG_URL", "https://example.test/api.json")
	c := &Config{}
	c.Defaults()
	if got := c.Free.SyncInterval(); got != 45*time.Minute {
		t.Fatalf("env sync interval = %s, want 45m", got)
	}
	if c.Free.CatalogURL != "https://example.test/api.json" {
		t.Fatalf("env catalog_url = %q", c.Free.CatalogURL)
	}
}

// A typo'd catalog_sync or catalog_url must fail the load loudly.
func TestFreeCatalogValidate(t *testing.T) {
	cases := []struct {
		sync   string
		url    string
		errSub string // empty = must pass
	}{
		{"24h", DefaultFreeCatalogURL, ""},
		{"", DefaultFreeCatalogURL, ""},
		{"off", DefaultFreeCatalogURL, ""},
		{"24hr", DefaultFreeCatalogURL, `free.catalog_sync "24hr"`},
		{"daily", DefaultFreeCatalogURL, `free.catalog_sync "daily"`},
		{"24h", "ftp://x/api.json", "free.catalog_url"},
		{"24h", "not-a-url", "free.catalog_url"},
	}
	for _, tc := range cases {
		cfg := &Config{}
		s := tc.sync
		cfg.Free.CatalogSync = &s
		cfg.Free.CatalogURL = tc.url
		cfg.Defaults()
		err := cfg.Validate()
		if tc.errSub == "" {
			if err != nil {
				t.Fatalf("sync=%q url=%q must pass: %v", tc.sync, tc.url, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("sync=%q url=%q must be rejected", tc.sync, tc.url)
		}
		if !strings.Contains(err.Error(), tc.errSub) {
			t.Fatalf("error %q must mention %q", err.Error(), tc.errSub)
		}
	}
}

// The table must survive a TOML round-trip: an explicit "" (sync off)
// must not read back as the default.
func TestFreeCatalogTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "onegw.toml")
	body := "[server]\nlisten = \"127.0.0.1:0\"\n\n[free]\ncatalog_sync = \"\"\ncatalog_url = \"https://example.test/api.json\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Free.SyncInterval(); got != 0 {
		t.Fatalf("explicit \"\" must read back as off, got %s", got)
	}
	if cfg.Free.CatalogURL != "https://example.test/api.json" {
		t.Fatalf("catalog_url = %q", cfg.Free.CatalogURL)
	}
}
