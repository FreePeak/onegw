package config

import (
	"strings"
	"testing"
	"time"
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

// The 2026-09-08 502 storm included gateway-side pre-first-byte aborts: the
// fixed 60s header timeout is too tight for massive thinking-model prefills.
// The knob must parse, and fall back to 60s when empty or invalid.
func TestResponseHeaderTimeoutDur(t *testing.T) {
	cases := []struct {
		val  string
		want time.Duration
	}{
		{"", 60 * time.Second},
		{"120s", 120 * time.Second},
		{"2m", 2 * time.Minute},
		{"bogus", 60 * time.Second},
		{"-5s", 60 * time.Second},
	}
	for _, c := range cases {
		cfg := &Config{}
		cfg.Server.ResponseHeaderTimeout = c.val
		if got := cfg.ResponseHeaderTimeoutDur(); got != c.want {
			t.Errorf("response_header_timeout %q: got %v, want %v", c.val, got, c.want)
		}
	}
}

func TestValidateComboStrategy(t *testing.T) {
	ok := []string{"", "order", "fastest"}
	for _, s := range ok {
		c := &Config{Providers: []ProviderCfg{{Name: "b", Kind: "openai", APIKey: "k"}}, Combos: []ComboCfg{{Name: "c", Targets: []string{"b/m"}, Strategy: s}}}
		if err := c.Validate(); err != nil {
			t.Fatalf("strategy %q must pass: %v", s, err)
		}
	}
	for _, s := range []string{"speed", "FASTEST", "auto"} {
		c := &Config{Providers: []ProviderCfg{{Name: "b", Kind: "openai", APIKey: "k"}}, Combos: []ComboCfg{{Name: "c", Targets: []string{"b/m"}, Strategy: s}}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "strategy") {
			t.Fatalf("strategy %q must be rejected naming the knob: %v", s, err)
		}
	}
}

// Duplicate account names inside one provider shadow each other in the
// account pool — a silent config corruption (a dashboard splice once
// produced exactly this shape and validated fine). Reject loudly.
func TestValidateRejectsDuplicateAccountName(t *testing.T) {
	mk := func(acctName string) *Config {
		return &Config{Providers: []ProviderCfg{{
			Name: "p", Kind: "openai", APIKey: "sk-x",
			Accounts: []Acct{
				{Name: acctName, APIKey: "sk-1"},
				{Name: acctName, APIKey: "sk-2"},
			},
		}}}
	}
	if err := mk("acct").Validate(); err == nil || !strings.Contains(err.Error(), "duplicate account") {
		t.Fatalf("duplicate account name must be rejected, got %v", err)
	}
	if err := mk("").Validate(); err != nil {
		t.Fatalf("nameless accounts are out of scope, got %v", err)
	}
}
