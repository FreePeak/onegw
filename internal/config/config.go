// Package config loads onegw's TOML configuration with environment
// overrides. Secrets can come from env (ONEGW_PROVIDER_<NAME>_KEY,
// ONEGW_KEYS, ONEGW_ADMIN_PASSWORD).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Server holds process-level settings.
type Server struct {
	Listen    string `toml:"listen"`
	DataDir   string `toml:"data_dir"`
	MaxBody   int64  `toml:"max_body_bytes"`        // per-request body cap (0 = 32 MiB)
	BufferCap int64  `toml:"buffered_budget_bytes"` // global in-flight buffered bytes (0 = 48 MiB)
	// AdminPassword: set via env ONEGW_ADMIN_PASSWORD; plain config allowed
	// for homelab.
	AdminPassword string `toml:"admin_password"`
	AccessLog     bool   `toml:"access_log"`
}

// Auth holds gateway API keys clients authenticate with.
type Auth struct {
	Keys []string `toml:"keys"`
}

// SaverCfg configures the token saver.
type SaverCfg struct {
	Enabled bool `toml:"enabled"`
}

// UsageCfg configures usage persistence.
type UsageCfg struct {
	FlushInterval string `toml:"flush_interval"` // e.g. "5s" (0 default 5s)
	RetentionDays int    `toml:"retention_days"` // 0 default 90
}

// ProviderCfg is one upstream provider definition.
type ProviderCfg struct {
	Name        string            `toml:"name"`
	Kind        string            `toml:"kind"` // openai | anthropic | gemini
	BaseURL     string            `toml:"base_url"`
	APIKey      string            `toml:"api_key"` // convenience for single-account
	Accounts    []Acct            `toml:"accounts"`
	Models      []string          `toml:"models"` // advertised model ids
	MaxConc     int               `toml:"max_concurrency"`
	ExtraHeader map[string]string `toml:"extra_headers"`
}

// Acct is one provider account.
type Acct struct {
	Name    string `toml:"name"`
	APIKey  string `toml:"api_key"`
	BaseURL string `toml:"base_url"`
	Weight  int    `toml:"weight"`
}

// ComboCfg is an ordered fallback chain.
type ComboCfg struct {
	Name    string   `toml:"name"`
	Targets []string `toml:"targets"` // "provider/model" strings
}

// Config is the whole file.
type Config struct {
	Server    Server        `toml:"server"`
	Auth      Auth          `toml:"auth"`
	Saver     SaverCfg      `toml:"saver"`
	Usage     UsageCfg      `toml:"usage"`
	Providers []ProviderCfg `toml:"providers"`
	Combos    []ComboCfg    `toml:"combo"`
}

// Defaults fills zero values with production-safe defaults.
func (c *Config) Defaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.DataDir == "" {
		c.Server.DataDir = defaultDataDir()
	}
	if c.Server.MaxBody == 0 {
		c.Server.MaxBody = 32 << 20
	}
	if c.Server.BufferCap == 0 {
		c.Server.BufferCap = 48 << 20
	}
	if c.Server.AdminPassword == "" {
		c.Server.AdminPassword = os.Getenv("ONEGW_ADMIN_PASSWORD")
	}
	if c.Server.AdminPassword == "" {
		c.Server.AdminPassword = "admin"
	}
	if c.Usage.FlushInterval == "" {
		c.Usage.FlushInterval = "5s"
	}
	if c.Usage.RetentionDays == 0 {
		c.Usage.RetentionDays = 90
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if key := os.Getenv("ONEGW_PROVIDER_" + strings.ToUpper(strings.ReplaceAll(p.Name, "-", "_")) + "_KEY"); key != "" {
			p.APIKey = key
		}
	}
	if keys := os.Getenv("ONEGW_KEYS"); keys != "" {
		c.Auth.Keys = strings.Split(keys, ",")
	}
}

// FlushEvery parses the flush interval.
func (c *Config) FlushEvery() time.Duration {
	d, err := time.ParseDuration(c.Usage.FlushInterval)
	if err != nil || d <= 0 {
		return 5 * time.Second
	}
	return d
}

// Validate checks required invariants.
func (c *Config) Validate() error {
	names := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("provider missing name")
		}
		if names["provider:"+p.Name] {
			return fmt.Errorf("duplicate provider %s", p.Name)
		}
		names["provider:"+p.Name] = true
		switch p.Kind {
		case "openai", "anthropic", "gemini":
		case "":
			return fmt.Errorf("provider %s missing kind", p.Name)
		default:
			return fmt.Errorf("provider %s unknown kind %q", p.Name, p.Kind)
		}
		if len(p.Accounts) == 0 && p.APIKey == "" {
			return fmt.Errorf("provider %s needs api_key or accounts", p.Name)
		}
	}
	comboNames := map[string]bool{}
	for _, cb := range c.Combos {
		if cb.Name == "" {
			return fmt.Errorf("combo missing name")
		}
		if comboNames[strings.ToLower(cb.Name)] {
			return fmt.Errorf("duplicate combo %s", cb.Name)
		}
		comboNames[strings.ToLower(cb.Name)] = true
		for _, t := range cb.Targets {
			if !strings.Contains(t, "/") {
				return fmt.Errorf("combo %s target %q must be provider/model", cb.Name, t)
			}
			prov := t[:strings.Index(t, "/")]
			if !names["provider:"+prov] {
				return fmt.Errorf("combo %s references unknown provider %s", cb.Name, prov)
			}
		}
	}
	return nil
}

// Load reads and validates the TOML file at path.
func Load(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func defaultDataDir() string {
	if v := os.Getenv("ONEGW_DATA_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/onegw"
	}
	return home + "/.onegw"
}

// Bool parses env-style bools.
func Bool(s string) bool {
	b, _ := strconv.ParseBool(s)
	return b
}
