package config

import (
	"strings"
	"testing"
)

func TestValidateRejectsNonHTTPExportURL(t *testing.T) {
	for _, url := range []string{"ftp://agg:8080/x", "agg:8080/admin/usage/import", "file:///tmp/x"} {
		c := &Config{Usage: UsageCfg{ExportURL: url}}
		if err := c.Validate(); err == nil {
			t.Fatalf("export_url %q must be rejected", url)
		}
	}
	c := &Config{Usage: UsageCfg{ExportURL: "https://agg:8080/admin/usage/import"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("https export_url must pass: %v", err)
	}
}

// A typo'd update.check_interval must fail the load loudly instead of
// silently falling back to the daily default.
func TestValidateCheckInterval(t *testing.T) {
	cases := []struct {
		val    string
		errSub string // empty = must pass
	}{
		{"", ""},
		{"24h", ""},
		{"30m", ""},
		{"off", ""},
		{"0", ""},
		{"24hr", `update.check_interval "24hr"`},
		{"daily", `update.check_interval "daily"`},
	}
	for _, c := range cases {
		cfg := &Config{Update: UpdateCfg{CheckInterval: c.val}}
		cfg.Defaults()
		err := cfg.Validate()
		if c.errSub == "" {
			if err != nil {
				t.Errorf("check_interval %q: unexpected error %v", c.val, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.errSub) {
			t.Errorf("check_interval %q: want error containing %q, got %v", c.val, c.errSub, err)
		}
	}
}
