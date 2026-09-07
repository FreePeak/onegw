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
	Kind        string            `toml:"kind"` // openai | anthropic | gemini | opencode
	BaseURL     string            `toml:"base_url"`
	APIKey      string            `toml:"api_key"` // convenience for single-account
	Keys        []string          `toml:"keys"`    // multi-key accounts, one account per key
	Accounts    []Acct            `toml:"accounts"`
	Models      []string          `toml:"models"` // advertised model ids
	MaxConc     int               `toml:"max_concurrency"`
	ExtraHeader map[string]string `toml:"extra_headers"`
	// AlwaysThinking lists model globs (path.Match; "*" does not cross
	// "/") that reason unconditionally upstream and reject
	// disable-thinking knobs; see README.
	AlwaysThinking []string `toml:"always_thinking"`
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
	// Aliases maps a client-facing name to "provider/model", a combo name,
	// or another alias. Chains resolve iteratively (depth-capped); aliases
	// never shadow a real provider/model or combo name.
	Aliases map[string]string `toml:"aliases"`
}

// Defaults fills zero values with production-safe defaults.
func (c *Config) Defaults() {
	if c.Server.Listen == "" {
		// Security default: loopback only. Expose explicitly via listen = "0.0.0.0:8080".
		c.Server.Listen = "127.0.0.1:8080"
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
		envName := "ONEGW_PROVIDER_" + strings.ToUpper(strings.ReplaceAll(p.Name, "-", "_"))
		if key := os.Getenv(envName + "_KEY"); key != "" {
			p.APIKey = key
		}
		// ONEGW_PROVIDER_<NAME>_KEY2..KEY9 add subscription keys as extra
		// rotating accounts when the provider defines none in TOML. With a
		// single _KEY only, the legacy single-account path applies.
		if len(p.Accounts) == 0 && len(p.Keys) == 0 && p.APIKey != "" {
			accts := []Acct{{Name: "key-1", APIKey: p.APIKey}}
			for i := 2; i <= 9; i++ {
				k := strings.TrimSpace(os.Getenv(fmt.Sprintf("%s_KEY%d", envName, i)))
				if k == "" {
					continue
				}
				accts = append(accts, Acct{Name: fmt.Sprintf("key-%d", i), APIKey: k})
			}
			if len(accts) > 1 {
				p.Accounts = accts
			}
		}
		// keys = [...] is shorthand for one named account per key; the
		// account pool round-robins and cools them exactly like explicit
		// [[providers.accounts]] entries.
		if len(p.Keys) > 0 {
			for i, k := range p.Keys {
				if k = strings.TrimSpace(k); k == "" {
					continue
				}
				p.Accounts = append(p.Accounts, Acct{Name: fmt.Sprintf("key-%d", i+1), APIKey: k})
			}
			p.Keys = nil
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
		case "openai", "anthropic", "gemini", "opencode":
		case "":
			return fmt.Errorf("provider %s missing kind", p.Name)
		default:
			return fmt.Errorf("provider %s unknown kind %q", p.Name, p.Kind)
		}
		if len(p.Accounts) == 0 && p.APIKey == "" && len(p.Keys) == 0 {
			return fmt.Errorf("provider %s needs api_key, keys, or accounts", p.Name)
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
	// Aliases: keys must be new names, targets must resolve (transitively,
	// depth-capped) to a "provider/model" literal or a combo. All matching
	// is case-insensitive, mirroring the router's lookup tables; two keys
	// differing only in case are a duplicate and rejected.
	aliasLower := make(map[string]string, len(c.Aliases)) // lower → original
	for alias := range c.Aliases {
		low := strings.ToLower(alias)
		if prev, dup := aliasLower[low]; dup {
			return fmt.Errorf("duplicate alias %q (case-variant of %q)", alias, prev)
		}
		aliasLower[low] = alias
	}
	for alias, target := range c.Aliases {
		low := strings.ToLower(alias)
		if alias == "" {
			return fmt.Errorf("alias missing name")
		}
		if strings.Contains(alias, "/") {
			return fmt.Errorf("alias %q must not contain \"/\"", alias)
		}
		if names["provider:"+low] || comboNames[low] {
			return fmt.Errorf("alias %q shadows an existing provider or combo", alias)
		}
		seen := map[string]bool{low: true}
		cur := target
		for hop := 0; ; hop++ {
			if hop >= maxAliasHops {
				return fmt.Errorf("alias %q: chain longer than %d hops or cyclic", alias, maxAliasHops)
			}
			if _, isAlias := aliasLower[strings.ToLower(cur)]; isAlias {
				curlow := strings.ToLower(cur)
				if seen[curlow] {
					return fmt.Errorf("alias %q: cycle at %q", alias, cur)
				}
				seen[curlow] = true
				cur = c.Aliases[aliasLower[strings.ToLower(cur)]]
				continue
			}
			if comboNames[strings.ToLower(cur)] {
				break // alias → combo: fine
			}
			if !strings.Contains(cur, "/") {
				return fmt.Errorf("alias %q target %q is neither provider/model, combo, nor alias", alias, cur)
			}
			prov := cur[:strings.Index(cur, "/")]
			if !names["provider:"+prov] {
				return fmt.Errorf("alias %q references unknown provider %s", alias, prov)
			}
			break
		}
	}
	return nil
}

// maxAliasHops caps alias chain resolution so a cyclic TOML table cannot
// loop the resolver; one hop is the normal case, chains are a convenience.
const maxAliasHops = 8

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
