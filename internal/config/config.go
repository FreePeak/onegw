// Package config loads onegw's TOML configuration with environment
// overrides. Secrets can come from env (ONEGW_PROVIDER_<NAME>_KEY,
// ONEGW_KEYS, ONEGW_ADMIN_PASSWORD).
package config

import (
	"fmt"
	"net/url"
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
	// StreamRequests relays same-format request bodies to the upstream
	// without buffering them fully (fixed byte reservation per request).
	// Default false: bodies are read fully under the 4x budget.
	StreamRequests bool `toml:"stream_requests"`
	// ResponseHeaderTimeout bounds the upstream pre-first-byte phase
	// (dial + TLS + full request-body upload + upstream prefill) as a Go
	// duration, e.g. "120s". Massive thinking-model prefills can
	// legitimately exceed the historical fixed 60s; empty/invalid keeps
	// 60s. Applied when providers are built (startup and SIGHUP reload).
	ResponseHeaderTimeout string `toml:"response_header_timeout"`
	// TaskRouting enables per-request task-aware combo reordering
	// (issue #54): local difficulty classifier sorts combo targets so
	// the best-fit model is tried first; full fallback chain preserved.
	// "off" (default) = no-op; "on" = active. Env ONEGW_TASK_ROUTING
	// overrides. Applied on startup and SIGHUP reload.
	TaskRouting string `toml:"task_routing"`
}

// Auth holds gateway API keys clients authenticate with. Keys may be
// defined flat (keys = ["sk-..."], unlimited) or as policy tables
// ([[auth.keys]] with optional rpm/tpm/models); both shapes share the
// same TOML path, so the raw form is captured and normalized by
// (*Auth).decodeKeys in keys.go.
type Auth struct {
	Raw     *toml.Primitive `toml:"keys"`
	KeyList []AuthKey       `toml:"-"`
}

// SaverCfg configures the token saver (input-side tool_result compression
// plus output-side injection and external compress hooks).
type SaverCfg struct {
	Enabled bool `toml:"enabled"`
	// Output-side: system-prompt injection rules. See saver.InjectCfg.
	Inject []InjectCfg `toml:"inject"`
	// Output-side: external compress hook (Headroom /v1/compress).
	External ExternalCfg `toml:"external"`
}

// UpdateCfg configures release checking and self-update. CheckInterval
// is a Go duration ("24h"); "0"/"off" disables background checks (the
// `onegw update` command still works). Auto applies a newer release by
// zero-drop handoff restart; inside a container auto is ignored (the
// filesystem belongs to the image) and only the check runs. Repo points
// at the GitHub releases source.
type UpdateCfg struct {
	CheckInterval string `toml:"check_interval"`
	Auto          bool   `toml:"auto"`
	Repo          string `toml:"repo"`
}

// InjectCfg is one terse-output injection rule: when the request model
// matches (path.Match globs; empty = all), the mode's directive is
// prepended to the system prompt. Mode: caveman | terse | custom (text).
type InjectCfg struct {
	Mode   string   `toml:"mode"`
	Models []string `toml:"models"`
	Text   string   `toml:"text"`
}

// ExternalCfg points at an external compress service (Headroom protocol:
// POST {messages} -> {messages}). Requests whose body is at least
// min_bytes get their messages[] compressed; any failure passes the
// request through uncompressed unless fail_open is explicitly false.
type ExternalCfg struct {
	Enabled   bool   `toml:"enabled"`
	URL       string `toml:"url"`
	TimeoutMS int    `toml:"timeout_ms"`
	MinBytes  int    `toml:"min_bytes"`
	FailOpen  *bool  `toml:"fail_open"`
}

// UsageCfg configures usage persistence.
type UsageCfg struct {
	FlushInterval string `toml:"flush_interval"` // e.g. "5s" (0 default 5s)
	RetentionDays int    `toml:"retention_days"` // 0 default 90
	// ExportURL + ExportPassword turn this instance into a usage shipper:
	// after each flush, newly flushed buckets are also POSTed as JSONL to
	// export_url (typically another onegw's /admin/usage/import).
	// Fire-and-forget: one retry, never blocks the flush loop.
	ExportURL      string `toml:"export_url"`
	ExportPassword string `toml:"export_password"`
}

// ProviderCfg is one upstream provider definition.
type ProviderCfg struct {
	Name        string            `toml:"name"`
	Kind        string            `toml:"kind"` // openai | anthropic | gemini | opencode | searxng | openai-responses | commandcode | cursor
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

	// CacheProfile opts the provider into upstream prompt-cache anchoring
	// (issue #34): "" or "none" (default) forwards request bodies
	// untouched — byte-identical, since GLM/DeepSeek/b-ai-style upstreams
	// ignore cache fields entirely; "claude-anchor" strips client
	// cache_control markers and re-anchors {"type":"ephemeral"} at
	// canonical positions on Anthropic-format bodies; "dashscope-marker"
	// keeps Qwen/DashScope cache_control markers but caps them at the
	// documented 4-marker ceiling on OpenAI-format bodies; "sticky-key"
	// injects the gateway session identity as prompt_cache_key for
	// implicit sticky-routing upstreams (xai, OpenRouter, Kimi).
	CacheProfile string `toml:"cache_profile"`

	// SearXNG virtual provider (kind = "searxng"): web search surfaced as
	// a chat model. max_results caps how many results are formatted into
	// the completion; timeout bounds the search call (Go duration). If the
	// instance requires auth, configure it via extra_headers (SearXNG
	// accepts X-API-Key or basic auth) — the gateway adds no auth code.
	// Zero values mean 5 results and a 10s timeout, applied where the
	// search runs (internal/provider/searxng.go).
	MaxResults int    `toml:"max_results"`
	Timeout    string `toml:"timeout"`
	// Sticky pins one upstream account to a request identity (the client
	// session header, else the auth key label) for this long — "5m", "30s" —
	// so repeat calls reuse the same key (prompt-cache friendly). A failed
	// attempt unpins; a cooling account rotates. "" = plain round-robin.
	Sticky string `toml:"sticky"`
	// SessionHeader opts the provider into derived session affinity
	// (issue #36): when the client sent none of the forwarded session
	// headers (x-grok-conv-id, x-grok-session-id, x-session-id,
	// session_id), the gateway sends a stable per-key opaque id in this
	// header — only for upstreams documented to use it (xai:
	// "x-grok-conv-id"). "" (default) never invents a header.
	SessionHeader string `toml:"session_header"`
	// Quota tracking (issue #7): Window "" = off | "5h" | "daily" |
	// "weekly". QuotaResetAnchor optionally pins the reset grid to an ISO
	// instant (its time-of-day phases daily resets; its instant phases 5h/
	// weekly grids); empty = UTC midnight / ISO Monday / first-seen. The
	// limits cap input+output+reasoning tokens (0 = track only).
	QuotaWindow        string `toml:"quota_window"`
	QuotaResetAnchor   string `toml:"quota_reset_anchor"`
	QuotaLimitTokens   int64  `toml:"quota_limit_tokens"`
	QuotaLimitRequests int64  `toml:"quota_limit_requests"`
	// Passthrough opts the provider into the narrow OpenAI-format surfaces
	// served without translation: "embeddings", "stt", "tts".
	Passthrough []string `toml:"passthrough"`
	// Tiers declares per-model task-routing metadata (issue #54):
	// power 0–150, vision/reasoning flags, context/output limits.
	Tiers []TierCfg `toml:"tier"`
}

// Acct is one provider account.
type Acct struct {
	Name    string `toml:"name"`
	APIKey  string `toml:"api_key"`
	BaseURL string `toml:"base_url"`
	Weight  int    `toml:"weight"`
	// RPM proactively caps upstream attempts per minute for this account
	// (token bucket, 0 = uncapped) so the pool rotates before the
	// upstream's per-account rate limit benches the key reactively.
	RPM int `toml:"rpm"`
}

// TierCfg is one model's task-routing metadata (issue #54), declared as
// [[providers.tier]]. Power is the capability score on a 0–150 scale —
// higher = more capable; the router tries the combo target whose power
// is closest to the classified task's target first. Vision/Reasoning
// gate hard-miss penalties; Context/MaxOut are token limits for the
// prompt-overflow guard (0 = unknown, no size penalty).
type TierCfg struct {
	Model     string `toml:"model"`     // exact model id or path.Match glob
	Power     int    `toml:"power"`     // 0–150 capability score
	Vision    bool   `toml:"vision"`    // handles image inputs
	Reasoning bool   `toml:"reasoning"` // reasoning model
	Context   int    `toml:"context"`   // token context window (0 = unknown)
	MaxOut    int    `toml:"max_out"`   // max output tokens (0 = unknown)
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
	OAuth   OAuthCfg          `toml:"oauth"` // device-flow accounts; see oauth.go (#2)
	Update  UpdateCfg         `toml:"update"`
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
	if c.Usage.ExportPassword == "" {
		c.Usage.ExportPassword = os.Getenv("ONEGW_EXPORT_PASSWORD")
	}
	if c.Update.Repo == "" {
		c.Update.Repo = "FreePeak/onegw"
	}
	if c.Update.CheckInterval == "" {
		c.Update.CheckInterval = "24h"
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
		// Env override replaces the file's keys entirely, as before.
		c.Auth.Raw = nil
		for _, k := range strings.Split(keys, ",") {
			if k = strings.TrimSpace(k); k != "" {
				c.Auth.KeyList = append(c.Auth.KeyList, AuthKey{Key: k})
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("ONEGW_TASK_ROUTING")); v != "" {
		c.Server.TaskRouting = v
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

// ResponseHeaderTimeoutDur parses [server] response_header_timeout; empty
// or invalid keeps the historical 60s pre-first-byte budget.
func (c *Config) ResponseHeaderTimeoutDur() time.Duration {
	d, err := time.ParseDuration(c.Server.ResponseHeaderTimeout)
	if err != nil || d <= 0 {
		return 60 * time.Second
	}
	return d
}

// TaskRoutingOn reports whether task-aware combo reordering (issue #54)
// is enabled. Anything other than "on" (case-insensitive) is off.
func (c *Config) TaskRoutingOn() bool {
	return strings.ToLower(strings.TrimSpace(c.Server.TaskRouting)) == "on"
}

// UpdateEvery parses the release-check interval; 0 means disabled.
func (c *Config) UpdateEvery() time.Duration {
	s := strings.ToLower(strings.TrimSpace(c.Update.CheckInterval))
	if s == "0" || s == "off" || s == "false" || s == "disabled" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 24 * time.Hour
	}
	return d
}

// Validate checks required invariants.
func (c *Config) Validate() error {
	if err := validateKeys(c.Auth.KeyList); err != nil {
		return err
	}
	// A typo'd check_interval ("24hr") must fail the load, not silently
	// check daily — UpdateEvery's fallback is only for untouched configs.
	if s := strings.ToLower(strings.TrimSpace(c.Update.CheckInterval)); s != "" {
		switch s {
		case "0", "off", "false", "disabled":
		default:
			if _, err := time.ParseDuration(s); err != nil {
				return fmt.Errorf("update.check_interval %q is not a Go duration (e.g. \"24h\", \"30m\") or 0/off", c.Update.CheckInterval)
			}
		}
	}
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
		case "openai-responses", "commandcode", "cursor":
		// Custom wire formats (issue #12). cursor is a fail-fast
		// skeleton: valid here, errors at request time.
		case "searxng":
			// Virtual search provider: no upstream credential needed
			// (public instances are open; private ones auth via
			// extra_headers), but an instance URL is mandatory.
			if p.BaseURL == "" {
				return fmt.Errorf("provider %s (searxng) needs base_url", p.Name)
			}
		case "":
			return fmt.Errorf("provider %s missing kind", p.Name)
		default:
			return fmt.Errorf("provider %s unknown kind %q", p.Name, p.Kind)
		}
		if p.Kind != "searxng" && len(p.Accounts) == 0 && p.APIKey == "" && len(p.Keys) == 0 {
			return fmt.Errorf("provider %s needs api_key, keys, or accounts", p.Name)
		}
		switch p.QuotaWindow {
		case "", "5h", "daily", "weekly":
		default:
			return fmt.Errorf("provider %s invalid quota_window %q (want 5h, daily or weekly)", p.Name, p.QuotaWindow)
		}
		if p.QuotaResetAnchor != "" {
			if _, err := time.Parse(time.RFC3339, p.QuotaResetAnchor); err != nil {
				return fmt.Errorf("provider %s invalid quota_reset_anchor %q: %w", p.Name, p.QuotaResetAnchor, err)
			}
		}
		if p.QuotaWindow == "" && (p.QuotaLimitTokens != 0 || p.QuotaLimitRequests != 0) {
			return fmt.Errorf("provider %s sets quota limits without quota_window", p.Name)
		}

		switch p.CacheProfile {
		case "", "none", "claude-anchor", "dashscope-marker", "sticky-key":
		default:
			return fmt.Errorf("provider %s unknown cache_profile %q (want none, claude-anchor, dashscope-marker or sticky-key)", p.Name, p.CacheProfile)
		}
		for _, tier := range p.Tiers {
			if tier.Model == "" {
				return fmt.Errorf("provider %s: [[tier]] missing model", p.Name)
			}
			if tier.Power < 0 || tier.Power > 150 {
				return fmt.Errorf("provider %s: tier %q power %d out of range (0-150)", p.Name, tier.Model, tier.Power)
			}
			if tier.Context < 0 || tier.MaxOut < 0 {
				return fmt.Errorf("provider %s: tier %q context/max_out must be >= 0", p.Name, tier.Model)
			}
		}
		if p.QuotaLimitTokens < 0 || p.QuotaLimitRequests < 0 {
			return fmt.Errorf("provider %s quota limits must be >= 0", p.Name)
		}
		if p.Sticky != "" {
			if d, err := time.ParseDuration(p.Sticky); err != nil || d <= 0 {
				return fmt.Errorf("provider %s invalid sticky %q (want a positive duration like \"5m\")", p.Name, p.Sticky)
			}
		}
		if h := strings.TrimSpace(p.SessionHeader); p.SessionHeader != "" && !validHeaderName(h) {
			return fmt.Errorf("provider %s invalid session_header %q", p.Name, p.SessionHeader)
		}
		for _, pc := range p.Passthrough {
			switch pc {
			case "embeddings", "stt", "tts":
			default:
				return fmt.Errorf("provider %s unknown passthrough capability %q", p.Name, pc)
			}
		}
	}
	// Task routing: accept "" (default off) plus "off" and "on", case-insensitive.
	switch strings.ToLower(strings.TrimSpace(c.Server.TaskRouting)) {
	case "", "off", "on":
	default:
		return fmt.Errorf("server.task_routing %q is not \"off\" or \"on\"", c.Server.TaskRouting)
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
	if u := c.Usage.ExportURL; u != "" {
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("usage.export_url %q must be an http(s) URL", u)
		}
	}
	for i, in := range c.Saver.Inject {
		switch in.Mode {
		case "caveman", "terse":
		case "custom":
			if strings.TrimSpace(in.Text) == "" {
				return fmt.Errorf("saver.inject[%d]: custom mode needs text", i)
			}
		case "":
			return fmt.Errorf("saver.inject[%d]: missing mode", i)
		default:
			return fmt.Errorf("saver.inject[%d]: unknown mode %q (caveman|terse|custom)", i, in.Mode)
		}
	}
	if c.Saver.External.Enabled && c.Saver.External.URL == "" {
		return fmt.Errorf("saver.external enabled but url missing")
	}
	if err := validateOAuth(c); err != nil {
		return err
	}
	return nil
}

// maxAliasHops caps alias chain resolution so a cyclic TOML table cannot
// loop the resolver; one hop is the normal case, chains are a convenience.
const maxAliasHops = 8

// validHeaderName reports whether s is a usable HTTP header name for
// session_header: RFC 7230 token characters, no spaces. Invalid values
// must fail config load, not silently drop the derived ids upstream.
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return false
		}
	}
	return true
}

// Load reads and validates the TOML file at path.
func Load(path string) (*Config, error) {
	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	cfg.Defaults()
	if err := cfg.Auth.decodeKeys(md); err != nil {
		return nil, err
	}
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
