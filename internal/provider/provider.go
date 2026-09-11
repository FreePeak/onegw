// Package provider defines the upstream provider abstraction: named kinds
// (openai, anthropic, gemini, openai-compatible clones, plus the virtual
// searxng search kind), account pools, and the single HTTP call contract
// the router drives.
package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"

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

// perKeySession derives the stable opaque id for one credential: the same
// key always maps to the same id (upstream prompt caches stay warm),
// different keys differ, and the salt keeps ids unrelated across
// mechanisms. Never returns "".
func perKeySession(salt, apiKey string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + apiKey))
	return "ses_" + hex.EncodeToString(sum[:16])
}

// opencodeSession returns the session id to send for this upstream call.
// Precedence: client header (trimmed, length-capped) > per-key derived id
// (stable across requests so the same credential maps to one upstream
// session). Never returns "".
func opencodeSession(clientVal, apiKey string) string {
	if s := strings.TrimSpace(clientVal); s != "" && len(s) <= maxOpenCodeSessionLen {
		return s
	}
	return perKeySession("opencode-go", apiKey)
}

// sessionAffinityHeaders are the client-sent conversation/session headers
// forwarded verbatim to every upstream (issue #36): xAI's documented
// prompt-cache stickiness ids and generic session ids. Lookup is
// case-insensitive (clients pick their own casing); only values the
// client actually sent are forwarded — nothing is invented here.
var sessionAffinityHeaders = [...]string{
	"x-grok-conv-id",
	"x-grok-session-id",
	"x-session-id",
	"session_id",
}

// clientHeader returns the first value of the named header, matched
// case-insensitively (Header.Get canonicalizes only the query key, so a
// client-sent X-GROK-CONV-ID would miss a canonical Get). "" when absent.
func clientHeader(h http.Header, name string) string {
	for k, vs := range h {
		if len(k) == len(name) && strings.EqualFold(k, name) && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
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
	// KindCursor + KindSearXNG intentionally fall through to OpenAI:
	// cursor's doCursor (cursor.go) returns a synthetic OpenAI SSE stream
	// decoded from the upstream's Connect-RPC protobuf, and searxng's
	// doSearch returns a synthetic OpenAI completion, so every surface
	// sees normal OpenAI shape from both kinds.
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
	// RPM caps this account's upstream attempts per minute with a refill
	// bucket (proactive governor): next() skips a drained bucket like a
	// cooldown, so the pool rotates BEFORE the upstream per-account rate
	// limit (您的账户已达到速率限制) benches the key reactively. 0 = uncapped.
	RPM int `toml:"rpm"`

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

	// Shared request-rate budget for the WHOLE provider (0 = uncapped):
	// one token bucket gating every account of this provider. For
	// upstreams whose rate limit is per-user/per-model-lane rather than
	// per-key (live tokenrouter 2026-09-09: 8 req/min shared across both
	// keys — harvey 429ed with only ~5 attempts in its trailing window),
	// per-account RPM cannot express the wall; this gates the pool as
	// one, so the pool reports an honest fall-through instead of feeding
	// the shared window doomed attempts. Set from ProviderCfg.RPM.
	RPM int

	// Disabled pauses routing to this provider (from ProviderCfg.Disabled):
	// Router.Execute skips it at target lookup with a 503 the combo loop
	// falls through, and the dashboard grid renders the off state. The
	// Def stays in the pool so the dashboard can show its config.
	Disabled bool

	// Headers added to every upstream request (auth handled separately).
	ExtraHeaders map[string]string `toml:"extra_headers"`

	// AlwaysThinking lists model globs (path.Match syntax; "*" does not
	// cross "/") whose upstreams reason unconditionally and reject
	// "disable thinking" knobs (e.g. GLM 1210: use low|high|max). The
	// server rewrites such requests instead of forwarding them.
	AlwaysThinking []string `toml:"always_thinking"`

	// CacheProfile opts the provider into upstream prompt-cache anchoring
	// (issue #34, set from ProviderCfg.CacheProfile): "claude-anchor"
	// re-anchors Anthropic cache_control breakpoints after normalization,
	// "dashscope-marker" keeps DashScope/Qwen markers within the 4-marker
	// ceiling, "sticky-key" injects prompt_cache_key for sticky-routing
	// upstreams. ""/"none" forwards bodies untouched.
	CacheProfile string

	// Tiers declares per-model task-routing metadata (issue #54):
	// power scores, capability flags, and context/output limits used
	// by the router's local classifier to reorder combo targets. Set
	// from ProviderCfg.Tiers in server.apply; nil = no tier data for
	// this provider (targets use neutral defaults when task-routing on).
	Tiers []ModelTier `toml:"tiers"`

	// SearXNG virtual-kind settings (kind = "searxng" only); zero values
	// fall back to the defaults in searxng.go.
	SearchMaxResults int
	SearchTimeout    time.Duration

	// StickyTTL enables account affinity for the identities passed to
	// NextAccount (0 = plain round-robin). Set from ProviderCfg.Sticky.
	StickyTTL time.Duration

	// SessionHeader opts the provider into derived session affinity
	// (issue #36): when the client sent none of sessionAffinityHeaders,
	// Do sends a stable per-key opaque id (perKeySession) in this header
	// so repeat calls with one credential land on a warm upstream cache.
	// Only for upstreams documented to use it (xai: x-grok-conv-id).
	// "" (default) never invents a header. Set from
	// ProviderCfg.SessionHeader.
	SessionHeader string

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

	speed speedState // decode-speed EWMAs (speed.go): per-model + provider-wide

	// learnedAT records models discovered at runtime to reject
	// thinking-effort/disable knobs (GLM 1210-family 400) even though they
	// are not listed in AlwaysThinking. Learned state lives on the Def on
	// purpose: a SIGHUP reload rebuilds the pool with fresh Defs, which
	// re-syncs it with the on-disk config.
	learnedMu sync.RWMutex
	learnedAT map[string]struct{}

	// modelBenched records (model → bench-until) for upstream refusals
	// that indict the MODEL, not the credential (types.APIError.ModelScoped:
	// Zhipu per-model model_access_denied 403s, model_not_found 404s): the
	// (provider, model) pair is dead for now, but sibling models on the
	// same account serve fine, so benching the whole pool would punish
	// innocent keys. See BenchModel/ModelBenched. Bench state lives on the
	// Def like learnedAT: a SIGHUP reload rebuilds the pool with fresh
	// Defs, re-syncing it with the on-disk config.
	modelMu    sync.RWMutex
	modelBench map[string]time.Time

	// Header-timeout storm tracking (see noteHeaderTimeout): per-model
	// strikes of the gateway's own pre-first-byte aborts in a tumbling
	// window. A lone timeout stays request-shaped (the design note at
	// edgeFault); a storm of them indicts the (provider, model) leg.
	htMu      sync.Mutex
	htStrikes map[string]htWindow
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

// ModelBenchTTL is how long a model-scoped upstream refusal benches the
// (provider, model) pair. Short on purpose: a deprovisioned model stays
// dead, but a "model access denied" that later clears (plan grant, model
// re-enabled) must not stay locked out for long — and the bench expires
// silently, so the next request after expiry is the probe.
const ModelBenchTTL = 5 * time.Minute

// maxModelBenches bounds the bench map. Do only benches the routed model
// (routed model strings come from the route tables, so cardinality is
// naturally bounded), but the cap keeps even a pathological config honest:
// at the cap the sweep drops expired entries first, and a still-full map
// of LIVE benches simply stops accepting new ones rather than growing.
const maxModelBenches = 256

// BenchModel benches the (provider, model) pair for ttl (0 = ModelBenchTTL):
// Router.Execute skips the target without any upstream attempt until expiry,
// while sibling models and every account of this provider keep serving.
func (d *Def) BenchModel(model string, ttl time.Duration) {
	if ttl <= 0 {
		ttl = ModelBenchTTL
	}
	d.modelMu.Lock()
	defer d.modelMu.Unlock()
	if d.modelBench == nil {
		d.modelBench = make(map[string]time.Time)
	}
	if len(d.modelBench) >= maxModelBenches {
		// At the cap: sweep expired entries first; if none expired, evict
		// the soonest-to-expire bench — the freshest verdict carries the
		// most information, and the map must never outgrow the cap.
		now := time.Now()
		for m, until := range d.modelBench {
			if !now.Before(until) {
				delete(d.modelBench, m)
			}
		}
		if len(d.modelBench) >= maxModelBenches {
			evict := ""
			var evictAt time.Time
			for m, until := range d.modelBench {
				if evict == "" || until.Before(evictAt) {
					evict, evictAt = m, until
				}
			}
			delete(d.modelBench, evict)
		}
	}
	d.modelBench[model] = time.Now().Add(ttl)
}

// ModelBenched reports whether the (provider, model) pair is benched, and
// when the bench lifts (zero when not benched). Expired entries are removed
// on read, so the map self-cleans.
func (d *Def) ModelBenched(model string) (benched bool, ready time.Time) {
	d.modelMu.RLock()
	until, ok := d.modelBench[model]
	d.modelMu.RUnlock()
	if !ok {
		return false, time.Time{}
	}
	if !time.Now().Before(until) {
		d.modelMu.Lock()
		// Re-check under the write lock: a concurrent BenchModel may have
		// re-benched the model between the read and this promotion.
		if until2, ok2 := d.modelBench[model]; ok2 && !time.Now().Before(until2) {
			delete(d.modelBench, model)
		}
		d.modelMu.Unlock()
		return false, time.Time{}
	}
	return true, until
}

// ---------------------------------------------------------------------------
// Header-timeout storm bench
// ---------------------------------------------------------------------------

// A lone pre-first-byte abort (the gateway's own ResponseHeaderTimeout,
// classified NoSameTargetRetry) stays request-shaped by design: one oversized
// prefill on a serving provider must not exile the model for everyone
// (see the edgeFault doc). But a STORM of them on the same (provider, model)
// within a short window indicts the leg itself — b-ai 2026-09-10 16:29-16:48
// and 2026-09-11: every request re-burned the full header budget on leg #1
// because the timeout recorded zero state. Three strikes inside the tumbling
// window bench the model briefly (shorter than ModelBenchTTL: congestion,
// not deprovisioning); the next request then skips the leg with a ZERO-cost
// fall-through instead of a 75s wait.
const (
	headerTimeoutStrikes = 3               // timeouts before the leg is benched
	headerTimeoutWindow  = 3 * time.Minute // tumbling: re-armed by each strike
	headerTimeoutBench   = 3 * time.Minute // bench TTL for a confirmed storm
)

// htWindow is one model's strike window.
type htWindow struct {
	count int
	since time.Time
}

// noteHeaderTimeout records one header-budget abort on model and benches the
// (provider, model) leg once headerTimeoutStrikes timeouts land inside
// headerTimeoutWindow of each other. Call BenchModel outside htMu (it takes
// modelMu; lock order htMu→modelMu must stay consistent — never invert).
func (d *Def) noteHeaderTimeout(model string, now time.Time) {
	d.htMu.Lock()
	if d.htStrikes == nil {
		d.htStrikes = make(map[string]htWindow)
	}
	w := d.htStrikes[model]
	if w.count == 0 || now.Sub(w.since) > headerTimeoutWindow {
		w = htWindow{count: 1, since: now}
	} else {
		w.count++
	}
	benched := false
	if w.count >= headerTimeoutStrikes {
		delete(d.htStrikes, model) // fresh window after a trip
		benched = true
	} else {
		d.htStrikes[model] = w
	}
	d.htMu.Unlock()
	if benched {
		d.BenchModel(model, headerTimeoutBench)
		log.Printf("provider %s: model %s benched %s after %d header-timeout strikes",
			d.Name, model, headerTimeoutBench, headerTimeoutStrikes)
	}
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
		d.pool = newAccountPool(d.Accounts, d.StickyTTL, d.RPM)
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
			d.pool = newAccountPool(d.Accounts, d.StickyTTL, d.RPM)
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
// ladder (pool.ok). A Retry-After header, when present, always wins — as
// does a request-count window the 429 BODY names (APIError.RateWindow,
// e.g. "Maximum 8 requests within 1 minutes"): that bench rides the same
// verbatim, uncapped path as a header hint, because coolBase's 10s
// re-enters the still-closed window (live tokenrouter 2026-09-09: 429 at
// :46, ladder retry at :57 429s again, success only ~30-40s later).
const (
	coolBase = 10 * time.Second
	coolCap  = 60 * time.Second
)

// Flap breaker (b-ai edge 502 storms, live 2026-09-09 22:54): when a
// provider-wide fault strikes every account at once — an HTML "502 Bad
// Gateway" page served by an nginx-style edge while the origin pool flaps —
// per-account ladders cannot see it: the error indicts the provider, not the
// key. consecutive edge-class faults (HTML body, empty body, unreachable,
// timeout) trip a provider-wide breaker that parks the whole pool for
// flapOpen, so combos fall through instantly instead of fanning N accounts
// into a dead edge (a 7-account fan wastes ~14 attempts and 2 retries before
// a single 200 survives). A success resets the strike count (pool.ok). Sizing
// fits the observed ~25s windows; sustained outages keep re-tripping, one
// probe per window at most.
const (
	flapThreshold = 4                // consecutive edge-class faults before opening
	flapOpen      = 15 * time.Second // whole-pool park when the breaker opens
)

// edgeFault reports whether an upstream error indicts the provider's
// edge rather than the request or the credential: the whole 5xx
// edge/origin family (502/503/504, Cloudflare 52x) plus the transport
// observation types. Deliberate per-request rewrites (the auth-verify
// blip, the parse-rejected channel fault — both routed here as 502s)
// and shared-concurrency walls are NOT edge outages: they never strike
// the breaker. Header-budget 504s (NoSameTargetRetry — the gateway's
// own pre-first-byte abort on ONE oversized prefill) are request-shaped
// too: a smaller request to the same provider succeeds, so they never
// strike. 4xx never strikes.
func edgeFault(apiErr *types.APIError) bool {
	if apiErr == nil || apiErr.Status < 500 ||
		apiErr.SharedConcurrency() || apiErr.NoSameTargetRetry {
		return false
	}
	switch apiErr.Type {
	case "upstream_auth_verify_failed", "upstream_parse_rejected":
		return false
	}
	switch apiErr.Type {
	case "upstream_html_error", "upstream_empty_body", "upstream_unreachable", "upstream_timeout":
		return true
	}
	return edgeFaultStatus(apiErr.Status)
}

// edgeFaultStatus matches the edge-fault status family for callers that
// only have an HTTP code (passthrough surfaces relay errors undecoded).
func edgeFaultStatus(status int) bool {
	switch status {
	case 502, 503, 504, 520, 521, 522, 523, 524, 525, 526, 527:
		return true
	}
	return false
}

var htmlTitleRe = regexp.MustCompile(`(?is)<title>(.*?)</title>`)

// htmlErrPage reports whether an upstream error body is an HTML page —
// the stock nginx/CDN edge rendering ("502 Bad Gateway") served during
// origin outages — and extracts its <title> as a bounded one-line hint.
// JSON API errors never start with '<', so the check cannot misfire on
// a decoded body.
func htmlErrPage(body []byte) (title string, ok bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '<' {
		return "", false
	}
	if m := htmlTitleRe.FindSubmatch(trimmed); m != nil {
		title = strings.TrimSpace(string(m[1]))
		if len(title) > 100 {
			title = title[:100] + "…"
		}
		return ": " + title, true
	}
	return "", true
}

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

// Gated benches a after a premium-gating rejection (issue #48: upstream
// 403 access_denied, "Deposit required"): the CREDENTIAL lacks access to
// the model, so the request must rotate to another account, but the gate
// is a sales state that can clear (a deposit lands, plan upgrades) — not
// a hard revocation like a region lock. Reuse the 429 escalation ladder
// (gating is account state; repeated hits extend the window, capped at
// coolCap) rather than a fixed long park.
func (d *Def) Gated(a *Account) { d.pool.rateLimited(a, 0) }

// OK records an account success and clears its consecutive-429 strike
// count, so a recovered key re-enters the ladder at coolBase.
func (d *Def) OK(a *Account) { d.pool.ok(a) }

type accountState struct {
	acct     Account
	cooldown time.Time    // until when the account is skipped
	strikes  int          // consecutive 429s (adaptive ladder); reset on success
	bucket   *tokenBucket // RPM governor; nil = uncapped (shared across slots)
	speed    speedSample  // recent decode speed of this account (tokens/sec)
}

// tokenBucket is a refill bucket enforcing Account.RPM. Capacity is two
// (one short burst is cheaper than a cache-breaking rotation), refilling at
// rpm/60 tokens per second. take consumes one token or reports when the
// next is ready; it never queues — a drained bucket skips like a cooldown.
type tokenBucket struct {
	tokens float64
	rate   float64 // tokens per second
	last   time.Time
}

func newTokenBucket(rpm int) *tokenBucket {
	return &tokenBucket{tokens: 2, rate: float64(rpm) / 60}
}

// take drains one token if available, else returns the instant the next
// token refills (readyAt zero means the take succeeded).
func (b *tokenBucket) take(now time.Time) (readyAt time.Time) {
	if b.last.IsZero() {
		b.last = now
	} else {
		b.tokens += now.Sub(b.last).Seconds() * b.rate
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return time.Time{}
	}
	return now.Add(time.Duration((1 - b.tokens) / b.rate * float64(time.Second)))
}

// refillAt reports when the next token becomes available without
// consuming one — take() stays the sole token-consuming path.
func (b *tokenBucket) refillAt(now time.Time) time.Time {
	if b.last.IsZero() {
		return now
	}
	if b.tokens+(now.Sub(b.last).Seconds()*b.rate) >= 1 {
		return now
	}
	return now.Add(time.Duration((1 - b.tokens) / b.rate * float64(time.Second)))
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

	// Shared provider-wide request budget (Def.RPM): one bucket gating
	// every account. nil = uncapped (the default; per-account buckets
	// still apply).
	shared *tokenBucket

	// Flap breaker (provider-wide edge faults): consecutive edge-class
	// failures park the whole pool until flapOpenUntil, after which the
	// next request probes normally. See edgeFault.
	flapStrikes   int
	flapOpenUntil time.Time
}

// stickyPin is one identity's pinned account, matched by Name+APIKey like
// cool() so weighted slot expansion never breaks a pin.
type stickyPin struct {
	name, key string
	expires   time.Time
}

func newAccountPool(accts []Account, sticky time.Duration, sharedRPM int) *accountPool {
	if len(accts) == 0 {
		accts = []Account{{Name: "default"}}
	}
	p := &accountPool{ttl: sticky, now: time.Now}
	if sharedRPM > 0 {
		p.shared = newTokenBucket(sharedRPM)
	}
	buckets := make(map[string]*tokenBucket) // one bucket per account, shared by weighted slots
	for _, a := range accts {
		w := a.Weight
		if w <= 0 {
			w = 1
		}
		if a.RPM > 0 {
			buckets[a.Name+"\x00"+a.APIKey] = newTokenBucket(a.RPM)
		}
		for range w {
			p.accts = append(p.accts, accountState{acct: a, bucket: buckets[a.Name+"\x00"+a.APIKey]})
		}
	}
	return p
}

// next picks the account for this request. With a sticky TTL and a
// non-empty identity, a live pin returns its account untouched; an
// expired or cooling pin is dropped and rotation starts after that
// account's slot, re-pinning the winner.
//
// When every account is blocked — cooling, own-bucket drained, or the
// shared provider budget empty (Def.RPM) — it returns (nil, ready) instead
// of handing out a doomed pick: the caller falls through to the next combo
// target or answers pool-empty (breaker-open or cooling) with the honest
// Retry-After, rather than burning a ~1s upstream attempt that digs the
// provider's fault state deeper.
func (p *accountPool) next(id string) (*Account, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if !p.flapOpenUntil.IsZero() && now.Before(p.flapOpenUntil) {
		// Flap breaker open: a provider-wide edge fault is striking every
		// account — no doomed upstream call. The caller falls through to
		// the next combo target or answers pool-empty with
		// Retry-After = seconds until the breaker half-opens (one probe
		// window, not one probe per queued request).
		return nil, p.flapOpenUntil
	}
	n := len(p.accts)
	start := int(p.rr)
	if p.ttl > 0 && id != "" {
		if pin, ok := p.sticky[id]; ok && now.Before(pin.expires) {
			for i := range p.accts {
				if s := &p.accts[i]; s.acct.Name == pin.name && s.acct.APIKey == pin.key {
					if ok, _ := p.available(s, now); ok && p.sharedReady(now).IsZero() {
						p.grant(s, now)
						return &s.acct, time.Time{}
					}
					start = i + 1 // pinned account cooling or governed: rotate past it
					break
				}
			}
		}
		delete(p.sticky, id)
	}
	var ready time.Time // soonest cooldown expiry / bucket refill among blocked accounts
	anyOpen := false    // some slot passes its own gates but the shared budget is empty
	sharedReady := p.sharedReady(now)
	// Scan every slot (the pool is small) and take the FASTEST open one:
	// accounts carry a decode-speed EWMA (speed.go), so once real samples
	// exist traffic prefers the quicker credential while its own gates
	// (cooldown, RPM bucket) are open — the per-account ladder still
	// spreads load the moment the fast key hits a wall. Strictly-greater
	// comparison keeps round-robin order among equal and no-data speeds,
	// so a fresh pool behaves exactly like the old first-open pick.
	best := -1 // index offset from start of the fastest open slot
	for i := range n {
		s := &p.accts[(start+i)%n]
		if ok, r := p.available(s, now); ok {
			if sharedReady.IsZero() {
				if best < 0 || s.speed.tps() > p.accts[(start+best)%n].speed.tps() {
					best = i
				}
			} else {
				anyOpen = true // could serve at the shared refill — but not before
			}
		} else if !r.IsZero() {
			if ready.IsZero() || r.Before(ready) {
				ready = r
			}
		}
	}
	if best >= 0 {
		s := &p.accts[(start+best)%n]
		p.grant(s, now)
		p.rr = (uint64(start+best) + 1) % uint64(n)
		p.pin(id, &s.acct, now)
		return &s.acct, time.Time{}
	}
	if !sharedReady.IsZero() {
		// Shared budget empty: no account can attempt before the refill.
		// With an own-open slot the pool serves AT the refill; otherwise
		// the earliest own gate clamps the wait longer.
		if anyOpen || ready.IsZero() || sharedReady.After(ready) {
			ready = sharedReady
		}
	}
	return nil, ready
}

// sharedReady reports (zero) when the shared provider bucket holds a token
// for a pick, else (non-zero) the refill instant. Consumes nothing — take()
// runs in grant() so examined slots never spend the pool's budget.
func (p *accountPool) sharedReady(now time.Time) time.Time {
	if p.shared == nil {
		return time.Time{}
	}
	if r := p.shared.refillAt(now); r.After(now) {
		return r
	}
	return time.Time{}
}

// grant consumes the admission gates a pick passes: one token from the
// shared provider bucket (Def.RPM) and one from the account's own bucket
// (Account.RPM). Called under p.mu with every gate verified open — a pick
// examined but not granted must not spend anything.
func (p *accountPool) grant(s *accountState, now time.Time) {
	if p.shared != nil {
		p.shared.take(now)
	}
	if s.bucket != nil {
		s.bucket.take(now)
	}
}

// available reports whether slot s may take an upstream attempt now: past
// its cooldown AND, when governed by an RPM bucket, holding a token —
// WITHOUT consuming anything; grant() spends the tokens once a pick is
// final (available() also runs on slots the caller never grants, so a
// consuming check would silently burn budget on every examined sibling).
// When blocked it yields the instant the slot can serve —
// max(cooldown, refill), both gates must pass — so a fully blocked pool
// reports an honest soonest-ready for the fall-through Retry-After.
func (p *accountPool) available(s *accountState, now time.Time) (bool, time.Time) {
	cool := !s.cooldown.IsZero() && !now.After(s.cooldown)
	if s.bucket == nil {
		if cool {
			return false, s.cooldown
		}
		return true, time.Time{}
	}
	if cool {
		serve := s.cooldown
		if r := s.bucket.refillAt(now); r.After(serve) {
			serve = r
		}
		return false, serve
	}
	if r := s.bucket.refillAt(now); r.After(now) {
		return false, r
	}
	return true, time.Time{}
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

// flapStrike records one edge-class fault (HTML page, empty body,
// unreachable, timeout — see edgeFault) and opens the breaker when
// flapThreshold consecutive faults have struck. Not tied to an account:
// the fault indicts the provider's edge, so every key counts once.
func (p *accountPool) flapStrike() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flapStrikes++
	if p.flapStrikes >= flapThreshold {
		if t := p.now().Add(flapOpen); t.After(p.flapOpenUntil) {
			p.flapOpenUntil = t
		}
	}
}

// flapHeal records one successful call: the edge is serving again, so the
// breaker closes immediately and the strike count resets — a short blip
// inside a longer serving period must not accumulate toward the next trip.
// Any successful account clears it: the edge fault was provider-wide.
func (p *accountPool) flapHeal() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flapStrikes = 0
	p.flapOpenUntil = time.Time{}
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
// configureHTTP2 turns on h2 health pings for the shared upstream transport:
// a connection that goes silent (stalled peer, dropped path) is probed and
// dropped within readIdle+ping, and in-flight requests retry on a fresh
// connection instead of riding the dead one to the header timeout. Returns
// the configured h2 transport for tests.
func configureHTTP2(tr *http.Transport) *http2.Transport {
	h2, err := http2.ConfigureTransports(tr)
	if err != nil {
		return nil
	}
	h2.ReadIdleTimeout = 30 * time.Second
	h2.PingTimeout = 15 * time.Second
	return h2
}

func newHTTPClient(headerTimeout time.Duration) *http.Client {
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 60 * time.Second}).DialContext,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
		// Body streaming after headers stays unbounded — streams are
		// long-lived.
		ResponseHeaderTimeout: headerTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
	}
	// HTTP/2 health pings. All accounts of one provider multiplex onto a
	// single h2 connection per host, so one degraded connection stalls EVERY
	// account at once — the "http2: timeout awaiting response headers" 504
	// storm (2026-09-10 16:29-16:48: every b-ai account timing out while
	// fresh connections served instantly). The ping loop detects a
	// dead/stalled connection in ~readIdle+ping instead of letting each
	// request burn the full response-header budget; the transport then
	// retries on a fresh connection.
	configureHTTP2(tr)
	return &http.Client{
		Transport: tr,
		Timeout:   0, // streams are long-lived; per-request ctx governs
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
		if headerBudgetExhausted(err) {
			// The gateway's own pre-first-byte budget fired (the exact
			// net/http error text ResponseHeaderTimeout produces — a dial
			// timeout reports "dial tcp …: i/o timeout" instead). The
			// pre-first-byte demand is a property of the request
			// (prefill size), so a same-target retry just burns a second
			// full budget on the same queued upstream: mark
			// NoSameTargetRetry so Router.Execute falls through
			// immediately. Tokenrouter live evidence (2026-09-09 14:46):
			// glm-5.3-free prefill TTFB 17-37s nominal but the free lane
			// queues past 120s; the failed attempt retried the same
			// target, adding a second silent 120s before surfacing.
			return &types.APIError{
				Status: 504, Type: "upstream_timeout",
				Message: err.Error(), NoSameTargetRetry: true,
			}
		}
		return &types.APIError{Status: 504, Type: "upstream_timeout", Message: err.Error()}
	}
	return &types.APIError{Status: 502, Type: "upstream_unreachable", Message: err.Error()}
}

// headerBudgetExhausted reports whether err is the ResponseHeaderTimeout
// abort (as opposed to a dial/TLS timeout, which is genuinely per-attempt
// and stays retryable). The transport error is opaque, so match the only
// text net/http produces for this abort; dial timeouts report
// "dial tcp …: i/o timeout". Constant text, checked upstream of any
// dynamic content.
func headerBudgetExhausted(err error) bool {
	return strings.Contains(err.Error(), "timeout awaiting response headers")
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
	// FirstByte is when the upstream response HEADERS arrived. The server
	// measures decode speed (tokens/sec) from here to relay end — the
	// streaming phase proper, prefill excluded.
	FirstByte time.Time
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
// clientHdr carries the client request's headers (nil for headerless
// callers): the conversation/session ids among them are forwarded
// verbatim (issue #36) and KindOpenCode always sends a session id
// upstream, the client's own value when present.
func (d *Def) Do(ctx context.Context, acct *Account, model string, clientHdr http.Header, body io.Reader, stream bool) (*CallResult, *types.APIError) {
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
	case KindCursor:
		// Cursor protobuf executor (issue #12 follow-up): body is the
		// client's OpenAI-format request; doCursor re-encodes it into the
		// right service's wire format and answers with a synthetic OpenAI
		// stream (see cursor.go).
		return d.doCursor(ctx, acct, model, body)
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
				req.Header.Set(OpenCodeSessionHeader, opencodeSession(clientHeader(clientHdr, OpenCodeSessionHeader), acct.bearerToken()))
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
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	for k, v := range d.ExtraHeaders {
		req.Header.Set(k, v)
	}
	// Cache-affinity identity (issue #36): forward the client's
	// conversation/session ids verbatim — the live xai route loses
	// x-grok-conv-id today because Do builds a fresh upstream request —
	// and, when the client sent none and the provider opted in via
	// SessionHeader, derive a stable per-key id (same trade
	// opencodeSession makes for OpenCode Zen). Last: an explicit
	// operator extra_headers pin is config-error territory, but a
	// static pin must never collapse per-conversation ids, so the
	// client's per-request values win.
	d.applySessionAffinity(req.Header, clientHdr, acct.bearerToken())
	resp, err := d.httpClient().Do(req)
	if err != nil {
		apiErr := transportErr(ctx, err)
		if apiErr.NoSameTargetRetry {
			// Header-budget abort: strike the storm bench. A lone timeout
			// changes nothing downstream (it breaks to the next leg in
			// Execute); three in-window bench the leg so the next request
			// skips it at zero cost instead of burning another budget.
			d.noteHeaderTimeout(model, time.Now())
		} else if edgeFault(apiErr) {
			// Dial/TLS failures and timeouts are provider-edge shaped:
			// they count toward the flap breaker, not the account ladder.
			d.pool.flapStrike()
		}
		return nil, apiErr
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		apiErr := decodeUpstreamError(d.Kind, limited, resp.StatusCode)
		// Some upstreams answer error statuses with a ZERO-byte body
		// (live glm evidence 2026-09-09: api.z.ai /api/v1 returned 500s
		// with no payload while model access was being deprovisioned).
		// decodeUpstreamError would surface that as Type "upstream_error"
		// with an EMPTY Message — a dashboard row and a client error that
		// say nothing. Name the actual observation instead: the status is
		// real, the body is not.
		if hint, isHTML := htmlErrPage(limited); isHTML {
			// nginx/CDN edge placeholder (b-ai live 2026-09-09 22:54:
			// "<html><head><title>502 Bad Gateway</title>…" served by ALL
			// seven accounts at once while the origin pool flapped): the
			// page says nothing about the request. Surface a bounded
			// honest message instead of raw HTML — the breaker strike for
			// this and every other error shape happens once at the exit.
			apiErr.Type = "upstream_html_error"
			apiErr.Message = fmt.Sprintf("upstream %s returned HTTP %d with an HTML error page%s", d.Name, resp.StatusCode, hint)
		} else if len(limited) == 0 && apiErr.Message == "" && apiErr.Type == "upstream_error" {
			apiErr.Type = "upstream_empty_body"
			apiErr.Message = fmt.Sprintf("upstream %s returned HTTP %d with an empty error body", d.Name, resp.StatusCode)
		}
		if translat.UpstreamAuthVerifyFailed(apiErr.Status, apiErr.Type, apiErr.Message) {
			// Transient failure of the upstream's own auth/verify service
			// (b-ai one-api forwards the bearer to an internal verify
			// endpoint; a network blip there answers 401). The gateway key
			// is fine and the NEXT request can succeed, so this is an
			// upstream fault, not a credential fault: rewrite to a
			// retryable 502-class error (combo fall-through), keep the
			// upstream message for the dashboard, and do NOT bench or
			// rotate the account — cooling it would blame a healthy key.
			apiErr.Status = 502
			apiErr.Type = "upstream_auth_verify_failed"
		}
		if translat.UpstreamParseRejected(apiErr.Status, apiErr.Type, apiErr.Message) {
			// b-ai's distributor fans each request to heterogeneous GLM
			// backend nodes, and some reject large (>= ~228 KB) valid
			// bodies with a bare "Invalid request body. (request id: …)"
			// 400 while byte-identical replays serve 200 on other nodes
			// (live RCA 2026-09-09: 22 client-visible failures over ~40 h,
			// every one from the same request-id node). Channel fault, not
			// request fault: rewrite to a retryable 502-class error so
			// Router.Execute retries the target — a fresh node may serve —
			// and combos fall through. Do NOT bench or rotate the account;
			// the credential is healthy.
			apiErr.Status = 502
			apiErr.Type = "upstream_parse_rejected"
		}
		if apiErr.OverQuota() && acct != nil {
			// Upstream's Retry-After header, when present, wins verbatim —
			// on the account ladder (coolDuration below) AND on the error
			// object, so Router.Execute never stamps its generic default
			// over an upstream-provided hint.
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				apiErr.RetryAfter = ra
			} else if w := apiErr.RateWindow(); w > 0 {
				// Same contract as a header hint, carried in the body:
				// Router.Execute must not stamp its generic 10s over an
				// upstream-stated window.
				apiErr.RetryAfter = strconv.FormatInt(int64((w+time.Second-1)/time.Second), 10)
			}
			// doubling to 60s per consecutive 429 — empty-body one-api
			// style limits recover fast but re-trigger immediately under
			// sustained load). Shared-limit 429s are the exception and
			// skip the ladder: the limit is the upstream's model-wide
			// concurrency (all of the reseller's traffic), not this key's
			// — live evidence (2026-09-09 10:52): a ~2s window 429ed two
			// keys, every other request succeeded on the remaining five;
			// benching healthy keys only shrinks the serving pool while the
			// shared window clears by itself. Dampening happens in the
			// retry backoff.
			if !sharedLimit429(apiErr.Status, apiErr.Code, apiErr.Message) {
				dDur := coolDuration(resp.Header.Get("Retry-After"))
				if dDur == 0 {
					// No header hint: bench for the request-count window
					// the body names ("Maximum 8 requests within 1
					// minutes" — live tokenrouter 2026-09-09) instead of
					// the 10s ladder base, which only digs deeper into
					// the still-closed window.
					dDur = apiErr.RateWindow()
				}
				d.pool.rateLimited(acct, dDur)
			}
		}
		if apiErr.RegionLocked() && acct != nil {
			// The credential is refused by policy, not load: park it long
			// enough that the pool hands the next attempt a healthy key.
			d.pool.cool(acct, 5*time.Minute)
		}
		if gated403(resp.StatusCode, limited) && acct != nil {
			// Premium-gated account (issue #48): the credential itself is
			// refused for this model until a deposit lands — account state,
			// not transient load, so unlike a 429 the request must rotate.
			// Bench the account on the adaptive ladder (first hit coolBase,
			// repeat hits double to coolCap; a deposit clearing the gate
			// resets it via pool.ok) and mark the error Fallbackable so
			// Router.Execute retries this target on the next account and
			// falls through to the next combo target instead of surfacing
			// the 403.
			d.Gated(acct)
			apiErr.Fallbackable = true
		}
		if apiErr.ModelScoped() {
			// Per-model lockout: the upstream refused THIS MODEL, not the
			// credential (Zhipu model_access_denied 403, model_not_found
			// 404). The account-level benches above already rotate the
			// in-flight request, but they would re-burn the whole pool on
			// every retry for a model no key can serve; benching the
			// (provider, model) pair lets Router.Execute skip the target
			// outright on the next request while sibling models on the
			// same accounts keep serving. 429s never reach here — they
			// are per-key walls (OverQuota/SharedConcurrency above).
			d.BenchModel(model, ModelBenchTTL)
		}
		if edgeFault(apiErr) {
			// Single strike site for every decoded error shape: HTML and
			// empty bodies classified above, plain JSON 502/503/504s
			// (the common one-api shape), and the rewritten 502s that
			// edgeFault does NOT count. The transport exit above strikes
			// separately — errors here are all upstream HTTP answers.
			d.pool.flapStrike()
		}
		return nil, apiErr
	}
	d.pool.ok(acct)   // success resets the 429 ladder
	d.pool.flapHeal() // and closes the flap breaker: the edge is serving
	return &CallResult{Resp: resp, Format: d.UpstreamFormat(model), Acct: acct, FirstByte: time.Now()}, nil
}

// DoPassthrough performs one upstream call for a passthrough surface
// ("embeddings", "transcriptions", "speech"). The body is relayed
// byte-for-byte from body without buffering: multipart streams stay streams.
// contentType is forwarded verbatim — a multipart boundary must reach the
// upstream intact. clientHdr carries the client's headers for the
// session-affinity forward (issue #36; nil = none). Upstream errors are not
// decoded here because passthrough bodies may be non-JSON (audio); the
// caller relays status and payload. The caller owns resp.Body.
func (d *Def) DoPassthrough(ctx context.Context, acct *Account, op, model, contentType string, clientHdr http.Header, body io.Reader, contentLen int64) (*http.Response, *types.APIError) {
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
	d.applySessionAffinity(req.Header, clientHdr, acct.bearerToken())
	resp, err := d.httpClient().Do(req)
	if err != nil {
		return nil, transportErr(ctx, err)
	}
	return resp, nil
}

// gated403 reports whether an upstream error is the premium-gating
// refusal (issue #48): b-ai premium-gated accounts answer 403 with
// "access_denied" / "Deposit required to unlock premium models." instead
// of a retryable error. Deliberately narrow: any other 403 (invalid key,
// permission denied) keeps failing fast — only these markers rotate.
// Zhipu's per-model "model_access_denied" 403 also matches: it benches the
// account and rotates, which for a combo target is the desired fall-through
// (the direct-request pool-empty answer is made honest separately).
func gated403(status int, body []byte) bool {
	if status != 403 {
		return false
	}
	b := strings.ToLower(string(body))
	return strings.Contains(b, "access_denied") || strings.Contains(b, "deposit required")
}

// sharedLimit429 reports whether an upstream 429 is the SHARED model-wide
// concurrency limit rather than a per-key rate/quota limit (see
// types.APIError.SharedConcurrency). Such 429s skip the per-account
// cooldown ladder entirely: benching keys for a limit shared by all of
// the reseller's traffic blames healthy credentials and shrinks the
// serving pool while the seconds-long window self-clears.
func sharedLimit429(status int, code, msg string) bool {
	return (&types.APIError{Status: status, Code: code, Message: msg}).SharedConcurrency()
}

// applySessionAffinity forwards the client's conversation/session ids to
// the upstream request (issue #36). Client-sent values ride verbatim for
// EVERY provider — nothing is invented. When the client sent none and the
// provider opted in via SessionHeader, a stable per-key opaque id is
// derived instead, so repeat calls with the same credential land on one
// warm upstream cache (the same trade opencodeSession makes for OpenCode
// Zen). Kind-specific headers run BEFORE this, so a kind that claims an
// allow-listed name (commandcode's per-request x-session-id) wins.
func (d *Def) applySessionAffinity(up, client http.Header, apiKey string) {
	sent := false
	for _, name := range sessionAffinityHeaders {
		if v := clientHeader(client, name); v != "" {
			up.Set(name, v)
			sent = true
		}
	}
	if sent || d.SessionHeader == "" {
		return
	}
	up.Set(d.SessionHeader, perKeySession("cache-affinity", apiKey))
}

// coolDuration parses an upstream Retry-After header (seconds form or
// HTTP-date). Zero when the header is missing or unusable — the caller
// (pool.rateLimited) then applies the adaptive ladder (coolBase doubling
// to coolCap), which is the documented behavior for b-ai's empty-body
// one-api style 429s: they carry no Retry-After and recover fast, so a
// flat 30s park would starve a healthy pool. A GARBAGE header is treated
// the same: no usable hint, ladder decides.
func coolDuration(retryAfter string) time.Duration {
	if retryAfter == "" {
		return 0
	}
	if secs, err := time.ParseDuration(retryAfter + "s"); err == nil && secs > 0 {
		return secs
	}
	if t, err := time.Parse(http.TimeFormat, retryAfter); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
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
