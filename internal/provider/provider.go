// Package provider defines the upstream provider abstraction: named kinds
// (openai, anthropic, gemini, openai-compatible clones, plus the virtual
// searxng search kind), account pools, and the single HTTP call contract
// the router drives.
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"onegw/internal/translat"
	"onegw/internal/types"
)

// Kind is a provider protocol family.
type Kind string

const (
	KindOpenAI    Kind = "openai"    // OpenAI Chat Completions (also most clones)
	KindAnthropic Kind = "anthropic" // Anthropic Messages
	KindGemini    Kind = "gemini"    // Gemini generateContent
	KindOpenCode  Kind = "opencode"  // OpenCode Zen Go subscription (OpenAI wire)
	// Custom wire formats (issue #12): commandcode = CommandCode /alpha/generate
	// NDJSON; openai-responses = Grok CLI Responses API; cursor = skeleton.
	KindCommandCode     Kind = "commandcode"      // CommandCode NDJSON executor
	KindOpenAIResponses Kind = "openai-responses" // Grok CLI Responses API
	KindCursor          Kind = "cursor"           // Cursor protobuf (skeleton: fail-fast)
)

// OpenCode Zen session header. The gateway always sends one: the client's
// own x-opencode-session when present (bounded length), else a stable
// per-key opaque session derived from the credential. This is what keeps
// upstream prompt caches warm and conversations isolated.
const OpenCodeSessionHeader = "X-Opencode-Session"

// maxOpenCodeSessionLen bounds the client-supplied session id we forward,
// mirroring the OpenCode gateway's own limit.
const maxOpenCodeSessionLen = 256

// opencodeSession returns the session id to send for this upstream call.
// Precedence: client header (trimmed, length-capped) > per-key derived id
// (stable across requests so the same credential maps to one upstream
// session). Never returns "".
func opencodeSession(clientVal, apiKey string) string {
	if s := strings.TrimSpace(clientVal); s != "" && len(s) <= maxOpenCodeSessionLen {
		return s
	}
	sum := sha256.Sum256([]byte("opencode-go\x00" + apiKey))
	return "ses_" + hex.EncodeToString(sum[:16])
}

// ResponsesOnlyModel reports whether an OpenCode catalog model is served
// only on the OpenAI Responses API (/v1/responses) — never on
// /v1/chat/completions. Derived from the OpenCode Zen endpoint table: the
// entire gpt-*, grok-* and muse-spark-* families.
func ResponsesOnlyModel(model string) bool {
	for _, p := range []string{"gpt-", "grok-", "muse-spark"} {
		if strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}

// UpstreamFormat returns the wire format the routed model actually speaks
// on this provider. Only KindOpenCode varies per model (chat-completions
// for the open-weight catalog, Responses API for gpt/grok/muse-spark).
func (d *Def) UpstreamFormat(model string) translat.Format {
	if d.Kind == KindOpenCode && ResponsesOnlyModel(model) {
		return translat.FmtResponses
	}
	return d.Kind.Format()
}

// Format returns the wire format a kind speaks.
func (k Kind) Format() translat.Format {
	switch k {
	case KindAnthropic:
		return translat.FmtAnthropic
	case KindGemini:
		return translat.FmtGemini
	case KindCommandCode:
		return translat.FmtCommandCode
	case KindOpenAIResponses:
		return translat.FmtOpenAIResponses
	case KindCursor:
		return translat.FmtCursor // skeleton; fail-fast upstream
	// KindSearXNG (searxng.go) intentionally falls through to OpenAI: its
	// Do() returns a synthetic OpenAI completion, so clients see a normal
	// chat response in every surface.
	default:
		return translat.FmtOpenAI
	}
}

// Account is one credential against a provider: API key (or token), optional
// base URL override, and optional per-account weight.
type Account struct {
	Name    string `toml:"name"`
	APIKey  string `toml:"api_key"`
	BaseURL string `toml:"base_url"` // optional override of kind default
	Weight  int    `toml:"weight"`   // round-robin weight, 0 = 1

	// OAuthToken resolves the credential at request time (issue #2): OAuth-
	// managed accounts rotate it in the background; nil = static APIKey.
	OAuthToken TokenProvider
}

// Def is a configured provider instance: a kind + endpoint defaults + its
// account pool.
type Def struct {
	Name     string    `toml:"name"` // e.g. "openrouter", "glm"
	Kind     Kind      `toml:"kind"`
	BaseURL  string    `toml:"base_url"`
	Accounts []Account `toml:"accounts"`

	// Models advertises the bare model ids this provider serves (empty =
	// pass any model through). Bare-model passthrough routing matches here.
	Models []string `toml:"models"`

	// Passthrough lists the OpenAI-format surfaces this provider may serve
	// verbatim without translation: "embeddings", "stt", "tts".
	Passthrough []string `toml:"passthrough"`

	// Concurrency cap for in-flight upstream calls (0 = unlimited).
	MaxConc int `toml:"max_concurrency"`

	// Headers added to every upstream request (auth handled separately).
	ExtraHeaders map[string]string `toml:"extra_headers"`

	// AlwaysThinking lists model globs (path.Match syntax; "*" does not
	// cross "/") whose upstreams reason unconditionally and reject
	// "disable thinking" knobs (e.g. GLM 1210: use low|high|max). The
	// server rewrites such requests instead of forwarding them.
	AlwaysThinking []string `toml:"always_thinking"`

	// SearXNG virtual-kind settings (kind = "searxng" only); zero values
	// fall back to the defaults in searxng.go.
	SearchMaxResults int
	SearchTimeout    time.Duration

	// StickyTTL enables account affinity for the identities passed to
	// NextAccount (0 = plain round-robin). Set from ProviderCfg.Sticky.
	StickyTTL time.Duration

	// HeaderTimeout bounds the pre-first-byte phase (dial, TLS, full body
	// upload, upstream prefill) of every upstream call. 0 = 60s default.
	// Massive-prefill providers (thinking models, ~100K-token sessions)
	// need more; set from [server] response_header_timeout.
	HeaderTimeout time.Duration

	// Memoized per-Def HTTP client (see httpClient).
	clientOnce sync.Once
	http       *http.Client

	pool     *accountPool
	inflight chan struct{}

	// learnedAT records models discovered at runtime to reject
	// thinking-effort/disable knobs (GLM 1210-family 400) even though they
	// are not listed in AlwaysThinking. Learned state lives on the Def on
	// purpose: a SIGHUP reload rebuilds the pool with fresh Defs, which
	// re-syncs it with the on-disk config.
	learnedMu sync.RWMutex
	learnedAT map[string]struct{}
}

// LearnAlwaysThinking records model as runtime-discovered always-thinking
// (its upstream rejected thinking knobs with the always-thinking 400) and
// reports whether this call newly learned it, so callers can log once.
func (d *Def) LearnAlwaysThinking(model string) bool {
	d.learnedMu.Lock()
	defer d.learnedMu.Unlock()
	if d.learnedAT == nil {
		d.learnedAT = make(map[string]struct{})
	}
	if _, ok := d.learnedAT[model]; ok {
		return false
	}
	d.learnedAT[model] = struct{}{}
	return true
}

// AlwaysThinkingModel reports whether the routed upstream model matches one
// of the provider's always-thinking globs (path.Match syntax) or was
// learned always-thinking at runtime (see LearnAlwaysThinking).
func (d *Def) AlwaysThinkingModel(model string) bool {
	for _, pat := range d.AlwaysThinking {
		if ok, err := path.Match(pat, model); err == nil && ok {
			return true
		}
	}
	d.learnedMu.RLock()
	defer d.learnedMu.RUnlock()
	_, ok := d.learnedAT[model]
	return ok
}

// AllowsPassthrough reports whether the provider declares the OpenAI-format
// passthrough surface op ("embeddings", "stt", "tts").
func (d *Def) AllowsPassthrough(op string) bool {
	for _, p := range d.Passthrough {
		if p == op {
			return true
		}
	}
	return false
}

// Models optionally advertises static model IDs; empty = pass through.
type ModelEntry struct {
	ID string `toml:"id"`
}

// Pool is the set of live provider definitions.
type Pool struct {
	mu     sync.RWMutex
	byName map[string]*Def
	order  []string
}

func NewPool() *Pool { return &Pool{byName: map[string]*Def{}} }

// Set (re)registers a provider definition.
func (p *Pool) Set(d *Def) {
	if d.pool == nil {
		d.pool = newAccountPool(d.Accounts, d.StickyTTL)
	}
	if d.MaxConc > 0 {
		d.inflight = make(chan struct{}, d.MaxConc)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.byName[d.Name]; !exists {
		p.order = append(p.order, d.Name)
	}
	p.byName[d.Name] = d
}

// Replace atomically swaps the whole provider set, preserving the given
// order. In-flight requests that already resolved a *Def keep serving from
// it; new requests resolve against the new set. Used by SIGHUP hot reload.
func (p *Pool) Replace(defs []*Def) {
	byName := make(map[string]*Def, len(defs))
	order := make([]string, 0, len(defs))
	for _, d := range defs {
		if d.pool == nil {
			d.pool = newAccountPool(d.Accounts, d.StickyTTL)
		}
		if d.MaxConc > 0 {
			d.inflight = make(chan struct{}, d.MaxConc)
		}
		if _, dup := byName[d.Name]; dup {
			continue // Validate rejects duplicates; never clobber on malice
		}
		byName[d.Name] = d
		order = append(order, d.Name)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byName = byName
	p.order = order
}

// Get returns a provider by name.
func (p *Pool) Get(name string) (*Def, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	d, ok := p.byName[name]
	return d, ok
}

// Names lists provider names in config order.
func (p *Pool) Names() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.order...)
}

func (k Kind) DefaultBaseURL() string {
	switch k {
	case KindAnthropic:
		return "https://api.anthropic.com"
	case KindGemini:
		return "https://generativelanguage.googleapis.com"
	case KindOpenCode:
		return "https://opencode.ai/zen/go"
	case KindOpenAIResponses:
		return "https://cli-chat-proxy.grok.com"
	case KindCursor:
		return "https://api2.cursor.sh"
	case KindCommandCode:
		return "https://api.commandcode.ai/alpha/generate"
	case KindSearXNG:
		return "" // no stock endpoint: base_url is required in config
	default:
		return "https://api.openai.com"
	}
}

// OpenCode Zen Go catalog: the live subscription model list (verified via
// GET /zen/go/v1/models, 2026-09-08). Chat-capable models route to
// /v1/chat/completions; the Responses-only families (gpt-*, grok-*,
// muse-spark-*) route to /v1/responses — see ResponsesOnlyModel.
var openCodeGoModels = []string{
	"deepseek-v4-flash", "deepseek-v4-flash-vision-exp", "deepseek-v4-pro",
	"glm-5", "glm-5.1", "glm-5.2", "glm-5.3", "glm-5.3-flash",
	"gpt-5.6-luna",
	"grok-4.5", "grok-4.6",
	"hy3", "hy3-preview", "hy4-preview",
	"kimi-k2.5", "kimi-k2.6", "kimi-k2.7-code", "kimi-k3",
	"longcat-2.0",
	"mimo-v2-omni", "mimo-v2-pro", "mimo-v2.5", "mimo-v2.5-pro",
	"minimax-m2.5", "minimax-m2.7", "minimax-m3",
	"muse-spark-1.2-contributor", "muse-spark-1.3-contributor",
	"omen-alpha",
	"qwen3.5-plus", "qwen3.6-plus", "qwen3.7-max", "qwen3.7-plus",
	"qwen3.8-flash", "qwen3.8-max",
}

// DefaultModels returns the stock catalog for kinds with a curated upstream
// model list; nil = none (the provider advertises only configured models).
func DefaultModels(k Kind) []string {
	if k == KindOpenCode {
		return openCodeGoModels
	}
	return nil
}

// Base resolves the effective base URL for an account, verbatim except for
// a trailing slash. Do() joins the API path such that both "https://host"
// and "https://host/v1" base_url conventions produce the correct endpoint:
// a base already ending in the kind's version prefix is used as-is.
func (d *Def) Base(acct *Account) string {
	base := d.BaseURL
	if acct != nil && acct.BaseURL != "" {
		base = acct.BaseURL
	}
	base = strings.TrimRight(base, "/")
	if base == "" {
		return d.Kind.DefaultBaseURL()
	}
	return base
}

// joinURL appends the kind's API path to a base. When the base's last path
// segment is itself a version segment (…/v1, …/v4 from GLM's /api/paas/v4),
// the path's version prefix is dropped and the unversioned remainder is
// appended — so "https://host/v1" + "/v1/chat/completions" and
// "https://host/api/paas/v4" + "/v1/chat/completions" both yield a single
// version segment.
func joinURL(base, path string) string {
	if seg := lastPathSegment(base); versionRe.MatchString(seg) {
		rest := path
		if i := strings.Index(path[1:], "/"); i >= 0 {
			rest = path[i+1:] // "/chat/completions"
		} else {
			rest = ""
		}
		return base + rest
	}
	return base + path
}

// lastPathSegment extracts the final path segment of a URL ("" if none).
func lastPathSegment(base string) string {
	i := strings.LastIndexByte(base, '/')
	if i < 0 || strings.HasSuffix(base, "://") {
		return ""
	}
	return base[i+1:]
}

var versionRe = regexp.MustCompile(`^v\d+$`)

// Account pool: weighted round-robin with adaptive cooldown on rate limits
// ---------------------------------------------------------------------------

// Adaptive cooldown ladder for upstream 429s that carry no Retry-After
// (empty-body one-api style limits). The first 429 benches the account for
// coolBase — long enough for a transient burst window (~5s observed) to
// clear, short enough not to starve a healthy pool. Each consecutive 429
// doubles the bench (the key is sinking into longer upstream blocks, not
// just a burst window), capped at coolCap. A successful call resets the
// ladder (pool.ok). A Retry-After header, when present, always wins.
const (
	coolBase = 10 * time.Second
	coolCap  = 60 * time.Second
)

// NextAccount picks the next available account (weighted round-robin with
// adaptive rate-limit cooldowns). With a sticky TTL configured, the identity
// (client session or auth-key label) is pinned to one account for the
// window: the first pick rotates and pins, repeats within the window reuse
// the pin, and expired or cooling pins rotate to the next account and
// re-pin. Identity "" disables pinning.
//
// When every account is cooling, NextAccount returns (nil, ready) where
// ready is the soonest cooldown expiry — callers must NOT send an upstream
// call against a cooling pool (it burns a doomed ~1s attempt and digs the
// upstream limit deeper); they fall through to the next combo target or
// answer 429 with Retry-After = time until ready.
func (d *Def) NextAccount(id string) (*Account, time.Time) { return d.pool.next(id) }

// Unpin drops an identity's pinned account so the next NextAccount rotates.
// Call after a failed upstream attempt to avoid re-sticking to a dead key.
func (d *Def) Unpin(id string) { d.pool.unpin(id) }

// Cool parks an account for d (quota/rate-limit error). Used for errors
// carrying an explicit duration (Retry-After, region lock) and for
// provider-wide quota exhaustion; plain 429s should use RateLimited to
// get the adaptive ladder.
func (d *Def) Cool(a *Account, dDur time.Duration) { d.pool.cool(a, dDur) }

// RateLimited applies the adaptive 429 cooldown ladder to a: coolBase,
// doubling per consecutive 429, capped at coolCap. A Retry-After duration
// > 0 bypasses the ladder and wins verbatim.
func (d *Def) RateLimited(a *Account, retryAfter time.Duration) {
	d.pool.rateLimited(a, retryAfter)
}

// OK records an account success and clears its consecutive-429 strike
// count, so a recovered key re-enters the ladder at coolBase.
func (d *Def) OK(a *Account) { d.pool.ok(a) }

type accountState struct {
	acct     Account
	cooldown time.Time // until when the account is skipped
	strikes  int       // consecutive 429s (adaptive ladder); reset on success
}

// maxStickyPins bounds the affinity map. Identities are client session ids
// and auth-key labels — normally a handful; the cap only matters against
// runaway session ids. At the cap, expired pins are swept, then the map
// resets (affinity is a cache, never load-bearing).
const maxStickyPins = 4096

type accountPool struct {
	mu      sync.Mutex
	accts   []accountState
	rr      uint64
	stopped bool
	ttl     time.Duration // sticky affinity window; 0 = plain round-robin
	sticky  map[string]stickyPin
	now     func() time.Time // injectable clock (tests)
}

// stickyPin is one identity's pinned account, matched by Name+APIKey like
// cool() so weighted slot expansion never breaks a pin.
type stickyPin struct {
	name, key string
	expires   time.Time
}

func newAccountPool(accts []Account, sticky time.Duration) *accountPool {
	if len(accts) == 0 {
		accts = []Account{{Name: "default"}}
	}
	p := &accountPool{ttl: sticky, now: time.Now}
	for _, a := range accts {
		w := a.Weight
		if w <= 0 {
			w = 1
		}
		for i := 0; i < w; i++ {
			p.accts = append(p.accts, accountState{acct: a})
		}
	}
	return p
}

// next picks the account for this request. With a sticky TTL and a
// non-empty identity, a live pin returns its account untouched; an
// expired or cooling pin is dropped and rotation starts after that
// account's slot, re-pinning the winner.
//
// When every account is cooling it returns (nil, ready) instead of
// handing out a doomed pick: the caller falls through to the next combo
// target or answers 429 with Retry-After, rather than burning a ~1s
// upstream attempt that extends the pool's rate-limit damage.
func (p *accountPool) next(id string) (*Account, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	n := len(p.accts)
	start := int(p.rr)
	if p.ttl > 0 && id != "" {
		if pin, ok := p.sticky[id]; ok && now.Before(pin.expires) {
			for i := range p.accts {
				if s := &p.accts[i]; s.acct.Name == pin.name && s.acct.APIKey == pin.key {
					if now.After(s.cooldown) {
						return &s.acct, time.Time{}
					}
					start = i + 1 // pinned account cooling: rotate past it
					break
				}
			}
		}
		delete(p.sticky, id)
	}
	var ready time.Time // soonest cooldown expiry among cooling accounts
	for i := range n {
		s := &p.accts[(start+i)%n]
		if now.After(s.cooldown) {
			p.rr = (uint64(start+i) + 1) % uint64(n)
			p.pin(id, &s.acct, now)
			return &s.acct, time.Time{}
		}
		if t := s.cooldown; ready.IsZero() || t.Before(ready) {
			ready = t
		}
	}
	return nil, ready
}

// ok records a successful call on a: the account is healthy, so both its
// strike count and any leftover cooldown reset — the next 429 starts the
// ladder at coolBase again. All slots of the account are touched —
// weighted pools expand one account into several slots.
func (p *accountPool) ok(a *Account) {
	if p == nil || a == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accts {
		if p.accts[i].acct.Name == a.Name && p.accts[i].acct.APIKey == a.APIKey {
			p.accts[i].strikes = 0
			p.accts[i].cooldown = time.Time{}
		}
	}
}

// rateLimited benches a after an upstream 429. retryAfter > 0 (upstream
// Retry-After) wins verbatim; otherwise the adaptive ladder benches for
// coolBase << strikes, capped at coolCap.
func (p *accountPool) rateLimited(a *Account, retryAfter time.Duration) {
	if p == nil || a == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// One account may occupy several weighted slots. Read the shared
	// strike count once (slots stay in sync: every 429 adds exactly one
	// strike, every success zeroes all), compute the ladder once, then
	// apply the same bench and strike value to every slot — cool()
	// semantics: a weighted pool cools as one.
	cur := -1
	for i := range p.accts {
		if s := &p.accts[i]; s.acct.Name == a.Name && s.acct.APIKey == a.APIKey && cur < 0 {
			cur = s.strikes
		}
	}
	if cur < 0 {
		return // account not in this pool
	}
	d := retryAfter
	if d <= 0 {
		d = coolBase << uint(min(cur, 3))
		if d > coolCap {
			d = coolCap
		}
	}
	now := p.now()
	for i := range p.accts {
		if s := &p.accts[i]; s.acct.Name == a.Name && s.acct.APIKey == a.APIKey {
			s.strikes = cur + 1
			if s.cooldown.Before(now) {
				s.cooldown = now
			}
			if t := now.Add(d); t.After(s.cooldown) {
				s.cooldown = t
			}
		}
	}
}

// pin records the identity → account affinity, keeping the map bounded.
func (p *accountPool) pin(id string, a *Account, now time.Time) {
	if p.ttl <= 0 || id == "" {
		return
	}
	if p.sticky == nil {
		p.sticky = make(map[string]stickyPin)
	}
	if len(p.sticky) >= maxStickyPins {
		for k, v := range p.sticky {
			if now.After(v.expires) {
				delete(p.sticky, k)
			}
		}
		if len(p.sticky) >= maxStickyPins {
			p.sticky = make(map[string]stickyPin)
		}
	}
	p.sticky[id] = stickyPin{name: a.Name, key: a.APIKey, expires: now.Add(p.ttl)}
}

// unpin drops an identity's pin so the next next() rotates.
func (p *accountPool) unpin(id string) {
	if id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sticky, id)
}

// cool marks an account cooling for d (quota exhausted).
func (p *accountPool) cool(a *Account, d time.Duration) {
	if p == nil || a == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for i := range p.accts {
		if p.accts[i].acct.Name == a.Name && p.accts[i].acct.APIKey == a.APIKey {
			if p.accts[i].cooldown.Before(now) {
				p.accts[i].cooldown = now
			}
			// Extend to the farthest slot so weighted pools cool as one.
			if t := now.Add(d); t.After(p.accts[i].cooldown) {
				p.accts[i].cooldown = t
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Upstream calls
// ---------------------------------------------------------------------------

// newHTTPClient builds the shared transport with a per-provider bound on
// the pre-first-byte phase (dial + TLS + full request-body upload +
// upstream prefill). 60s was sized for interactive chat: massive-session
// prefills on thinking models routinely exceed it, aborting with a 502
// before the upstream says a word (2026-09-08 502 storm: ~115K-token
// requests dying in waves while small probes succeeded). Streams after
// headers stay unbounded — streams are long-lived.
func newHTTPClient(headerTimeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 60 * time.Second}).DialContext,
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
			// Body streaming after headers stays unbounded — streams are
			// long-lived.
			ResponseHeaderTimeout: headerTimeout,
			TLSHandshakeTimeout:   10 * time.Second,
		},
		Timeout: 0, // streams are long-lived; per-request ctx governs
	}
}

// httpClient resolves the client for one call: a per-Def override when the
// config tuned response_header_timeout (memoized once per Def — Defs are
// rebuilt on SIGHUP reload, so state stays in sync with config), else the
// package default.
func (d *Def) httpClient() *http.Client {
	d.clientOnce.Do(func() {
		if d.HeaderTimeout > 0 && d.HeaderTimeout != 60*time.Second {
			d.http = newHTTPClient(d.HeaderTimeout)
		}
	})
	if d.http != nil {
		return d.http
	}
	return client
}

// transportErr classifies a failed upstream round-trip so the console log
// can tell a gateway-side timeout from a dead endpoint from a client that
// hung up — pre-2026-09-08 all three logged as bare 502 upstream_error.
// Go reports ResponseHeaderTimeout/dial timeouts as net.Error with
// Timeout() set; a canceled or client-deadline context means the CLIENT
// hung up, not the upstream.
func transportErr(ctx context.Context, err error) *types.APIError {
	// The request context is the authoritative client-hangup signal, and it
	// MUST be checked before the error chain: a ResponseHeaderTimeout's
	// error also satisfies errors.Is(err, context.DeadlineExceeded) on the
	// current Go (verified h1 + h2), so chain-matching alone cannot tell a
	// client deadline from a gateway-side pre-first-byte timeout.
	if ctx != nil && ctx.Err() != nil {
		return &types.APIError{Status: 499, Type: "client_closed", Message: err.Error()}
	}
	if anyChainTimeout(err) {
		return &types.APIError{Status: 504, Type: "upstream_timeout", Message: err.Error()}
	}
	return &types.APIError{Status: 502, Type: "upstream_unreachable", Message: err.Error()}
}

// anyChainTimeout reports whether ANY error in the unwrap chain reports a
// transport timeout. errors.As alone is not enough: it stops at the first
// net.Error in the chain, and (*url.Error).Timeout() only type-asserts its
// direct child — an extra wrap layer between them (fmt.Errorf, middleware)
// hides a real timeout deeper in the chain.
func anyChainTimeout(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if ne, ok := e.(net.Error); ok && ne.Timeout() {
			return true
		}
	}
	return false
}

// defaultClient keeps the previous package-level behavior for callers
// without per-Def overrides (passthrough diagnostics, searxng).
var client = newHTTPClient(60 * time.Second)

// CallResult bundles the upstream HTTP response for the router to stream.
type CallResult struct {
	Resp   *http.Response
	Format translat.Format
	Acct   *Account
}

// Path builds the upstream URL path for a kind from the client's path
// intent. `op` is "chat" (chat completions / messages / generateContent)
// or "models". For KindOpenCode the chat path depends on the routed model:
// Responses-only families (gpt/grok/muse-spark) live on /v1/responses.
// Passthrough surfaces ("embeddings", "transcriptions", "speech") speak
// OpenAI shape for every kind.
func (d *Def) Path(op, model string) string {
	switch op {
	case "embeddings":
		return "/v1/embeddings"
	case "transcriptions":
		return "/v1/audio/transcriptions"
	case "speech":
		return "/v1/audio/speech"
	}
	switch d.Kind {
	case KindAnthropic:
		if op == "models" {
			return "/v1/models"
		}
		return "/v1/messages"
	case KindGemini:
		if op == "models" {
			return "/v1beta/models"
		}
		// model + method appended by caller (needs model name)
		return ""
	case KindOpenAIResponses:
		// Grok CLI answers on the Responses endpoint only.
		return "/v1/responses"
	case KindCommandCode:
		if op == "models" {
			return "/v1/models"
		}
		return "" // base_url IS the /alpha/generate endpoint
	case KindCursor:
		return "" // skeleton
	case KindOpenCode:
		if op == "models" {
			return "/v1/models"
		}
		if ResponsesOnlyModel(model) {
			return "/v1/responses"
		}
		return "/v1/chat/completions"
	default:
		if op == "models" {
			return "/v1/models"
		}
		return "/v1/chat/completions"
	}
}

// Do performs one upstream call. body supplies the request payload; it is
// sent as-is (Content-Length is derived for *bytes.Reader, *bytes.Buffer,
// and *strings.Reader; any other reader goes out chunked). body may be nil.
// clientSession is the value of the client's x-opencode-session header (""
// when absent); it is only consumed by KindOpenCode, which always sends a
// session id upstream.
func (d *Def) Do(ctx context.Context, acct *Account, model, clientSession string, body io.Reader, stream bool) (*CallResult, *types.APIError) {
	if d.inflight != nil {
		select {
		case d.inflight <- struct{}{}:
			defer func() { <-d.inflight }()
		case <-ctx.Done():
			return nil, &types.APIError{Status: 499, Type: "client_closed", Message: ctx.Err().Error()}
		}
	}
	base := d.Base(acct)
	var url string
	var req *http.Request
	var err error
	switch d.Kind {
	case KindSearXNG:
		// Virtual provider: answer with a SearXNG search instead of a chat
		// call (see searxng.go). The search body is tiny; read it whole.
		raw, rerr := io.ReadAll(body)
		if rerr != nil {
			return nil, &types.APIError{Status: 400, Type: "invalid_request", Message: rerr.Error()}
		}
		return d.doSearch(ctx, acct, model, raw, stream)
	case KindGemini:
		// Non-streaming: :generateContent; streaming: :streamGenerateContent?alt=sse
		method := "generateContent"
		qs := ""
		if stream {
			method = "streamGenerateContent"
			qs = "?alt=sse"
		}
		url = fmt.Sprintf("%s/models/%s:%s%s", joinURL(base, "/v1beta"), model, method, qs)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, url, body)
		if err == nil {
			req.Header.Set("x-goog-api-key", acct.APIKey)
		}
	default:
		url = joinURL(base, d.Path("chat", model))
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, url, body)
		if err == nil {
			// Kind-specific NON-credential headers (fingerprints, versions).
			// Credentials are set once below via applyAuth (issue #2: also
			// resolves OAuth tokens).
			switch d.Kind {
			case KindAnthropic:
				req.Header.Set("anthropic-version", "2023-06-01")
			case KindOpenCode:
				req.Header.Set(OpenCodeSessionHeader, opencodeSession(clientSession, acct.bearerToken()))
			case KindCommandCode:
				// 9router fingerprint: per-request session id + CLI version.
				req.Header.Set("x-command-code-version", translat.CommandCodeVersion)
				req.Header.Set("x-cli-environment", "cli")
				req.Header.Set("x-session-id", newRequestUUID())
			case KindOpenAIResponses:
				// Grok CLI fingerprint headers.
				req.Header.Set("x-grok-client-identifier", "xai-grok-cli")
				req.Header.Set("x-grok-client-version", "0.2.99")
			}
			// applyAuth is the single credential owner for EVERY kind.
			applyAuth(req.Header, d.Kind, acct.bearerToken())
		}
	}
	if err != nil {
		return nil, &types.APIError{Status: 500, Type: "internal", Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	for k, v := range d.ExtraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := d.httpClient().Do(req)
	if err != nil {
		return nil, transportErr(ctx, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		apiErr := decodeUpstreamError(d.Kind, limited, resp.StatusCode)
		if apiErr.OverQuota() && acct != nil {
			// Retry-After (when upstream sends one) wins verbatim; otherwise
			// the adaptive ladder benches the account (10s doubling to 60s
			// per consecutive 429 — empty-body one-api style limits recover
			// fast but re-trigger immediately under sustained load).
			d.pool.rateLimited(acct, coolDuration(resp.Header.Get("Retry-After")))
		}
		if apiErr.RegionLocked() && acct != nil {
			// The credential is refused by policy, not load: park it long
			// enough that the pool hands the next attempt a healthy key.
			d.pool.cool(acct, 5*time.Minute)
		}
		return nil, apiErr
	}
	d.pool.ok(acct) // success resets the 429 ladder
	return &CallResult{Resp: resp, Format: d.UpstreamFormat(model), Acct: acct}, nil
}

// DoPassthrough performs one upstream call for a passthrough surface
// ("embeddings", "transcriptions", "speech"). The body is relayed
// byte-for-byte from body without buffering: multipart streams stay streams.
// contentType is forwarded verbatim — a multipart boundary must reach the
// upstream intact. Upstream errors are not decoded here because passthrough
// bodies may be non-JSON (audio); the caller relays status and payload.
// The caller owns resp.Body.
func (d *Def) DoPassthrough(ctx context.Context, acct *Account, op, model, contentType string, body io.Reader, contentLen int64) (*http.Response, *types.APIError) {
	if d.inflight != nil {
		select {
		case d.inflight <- struct{}{}:
			defer func() { <-d.inflight }()
		case <-ctx.Done():
			return nil, &types.APIError{Status: 499, Type: "client_closed", Message: ctx.Err().Error()}
		}
	}
	base := d.Base(acct)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(base, d.Path(op, model)), body)
	if err != nil {
		return nil, &types.APIError{Status: 500, Type: "internal", Message: err.Error()}
	}
	if contentLen >= 0 {
		req.ContentLength = contentLen
	}
	req.Header.Set("Content-Type", contentType)
	switch d.Kind {
	case KindAnthropic:
		req.Header.Set("x-api-key", acct.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	case KindGemini:
		req.Header.Set("x-goog-api-key", acct.APIKey)
	default:
		req.Header.Set("Authorization", "Bearer "+acct.APIKey)
	}
	for k, v := range d.ExtraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := d.httpClient().Do(req)
	if err != nil {
		return nil, transportErr(ctx, err)
	}
	return resp, nil
}

func coolDuration(retryAfter string) time.Duration {
	if retryAfter == "" {
		return 30 * time.Second
	}
	if secs, err := time.ParseDuration(retryAfter + "s"); err == nil && secs > 0 {
		return secs
	}
	if t, err := time.Parse(http.TimeFormat, retryAfter); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

// decodeUpstreamError normalizes any upstream error body into APIError by
// trying the three wire error shapes.
func decodeUpstreamError(kind Kind, body []byte, status int) *types.APIError {
	switch kind {
	case KindAnthropic:
		return translat.DecodeAnthropicError(body, status)
	case KindGemini:
		return translat.DecodeGeminiError(body, status)
	default:
		return translat.DecodeOpenAIError(body, status)
	}
}

// FetchModels lists upstream models in OpenAI `/v1/models` shape (best
// effort; used by the /v1/models surface for kinds that support it).
func (d *Def) FetchModels(ctx context.Context, acct *Account) ([]byte, int, error) {
	base := d.Base(acct)
	url := joinURL(base, d.Path("models", ""))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 500, err
	}
	// Kind-specific non-credential headers; credential via applyAuth below
	// (issue #2: also resolves OAuth tokens for subscription accounts).
	switch d.Kind {
	case KindAnthropic:
		req.Header.Set("anthropic-version", "2023-06-01")
	case KindOpenCode:
		req.Header.Set(OpenCodeSessionHeader, opencodeSession("", acct.bearerToken()))
	case KindCommandCode:
		// 9router fingerprint: per-request session id + CLI version.
		req.Header.Set("x-command-code-version", translat.CommandCodeVersion)
		req.Header.Set("x-cli-environment", "cli")
		req.Header.Set("x-session-id", newRequestUUID())
	case KindOpenAIResponses:
		// Grok CLI fingerprint headers.
		req.Header.Set("x-grok-client-identifier", "xai-grok-cli")
		req.Header.Set("x-grok-client-version", "0.2.99")
	}
	applyAuth(req.Header, d.Kind, acct.bearerToken())
	resp, err := client.Do(req)
	if err != nil {
		return nil, transportErr(ctx, err).Status, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return body, resp.StatusCode, err
}
