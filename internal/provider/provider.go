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
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"path"
	"regexp"
	"sort"
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

	// DefaultEffort is the reasoning effort applied when a client sent no
	// effort knob for an always-thinking model ("" = disabled). Only
	// always-thinking models are touched: everywhere else an absent knob
	// means "no thinking", and injecting one would invent behavior the
	// client did not ask for. See DefaultEffortFor.
	DefaultEffort string

	ExtraHeaders map[string]string `toml:"extra_headers"`

	// AlwaysThinking lists model globs (path.Match syntax; "*" does not
	// cross "/") whose upstreams reason unconditionally and reject
	// "disable thinking" knobs (e.g. GLM 1210: use low|high|max). The
	// server rewrites such requests instead of forwarding them.
	AlwaysThinking []string `toml:"always_thinking"`

	// NoThinking lists model globs (path.Match syntax; "*" does not cross
	// "/") whose upstreams have no thinking mode at all and must never
	// receive reasoning-effort/thinking knobs (kilo-auto/*: kilo's gateway
	// duplicates reasoning_effort into reasoning.effort and rejects ANY
	// value with "conflicting values" — live 2026-09-11). The server
	// strips the knobs outright — strictly stronger than AlwaysThinking's
	// coercion, which keeps an accepted knob on the wire.
	NoThinking []string `toml:"no_thinking"`

	// EchoReasoning lists model globs (path.Match syntax; "*" does not
	// cross "/") whose upstreams validate the REPLAYED assistant history
	// in thinking mode: a tool-loop continuation 400s unless every
	// assistant turn carries reasoning_content — including turns other
	// combo legs served without reasoning. The server synthesizes the
	// missing echo (see synthesizeReasoningEcho); see ProviderCfg doc for
	// the live evidence.
	EchoReasoning []string `toml:"echo_reasoning"`

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

	// Rotation tunes this provider's rotation mechanics (#84); zero fields
	// keep the package defaults. Wired from config in server.apply (the
	// global [rotation] table merged with the provider override).
	Rotation RotationPolicy

	// Selection (#81) chooses among OPEN account slots: "" = shipped
	// behavior (fastest decode speed), or "p2c", "least-used",
	// "strict-random", "random". Headroom, when set, feeds the p2c score
	// with subscription headroom (#79) without this package importing it.
	Selection string
	Headroom  func(acct string) (float64, bool)

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

	// prefill EWMAs (prefill.go): the pre-first-byte phase per (model, size
	// bucket). Decode speed cannot predict a 200K-token request's cost; this
	// is the term that dominates it.
	prefill prefillState

	// learnedAT/learnedNT record models discovered at runtime: to coerce
	// thinking-effort/disable knobs (GLM 1210-family 400) or to strip ANY
	// reasoning knob (effort-conflict 400, kilocode), even though they are
	// not listed in AlwaysThinking/NoThinking. Learned state lives on the
	// Def on purpose: a SIGHUP reload rebuilds the pool with fresh Defs,
	// which re-syncs it with the on-disk config.
	learnedMu sync.RWMutex
	learnedAT map[string]struct{}
	learnedNT map[string]struct{}

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

// DefaultEffortFor reports the configured reasoning effort to apply to an
// always-thinking model when the client sent no effort knob at all, or "" to
// leave the request alone. Scope is deliberate (measured on b-ai/glm-5.3-flash
// 2026-09-11: unset → vendor default "max" produced 647/676 output tokens of
// which 395/392 were reasoning in 6.2-6.5s, while "low" produced 246/234 with
// 57/50 reasoning in 3.3-3.9s): only models whose upstream reasons
// unconditionally are touched, because on every other model an absent knob
// means "do not think" and adding one would change the client's contract.
func (d *Def) DefaultEffortFor(model string) string {
	if d.DefaultEffort == "" {
		return ""
	}
	if !d.AlwaysThinkingModel(model) {
		return ""
	}
	return d.DefaultEffort
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

// LearnNoThinking records model as runtime-discovered no-thinking (its
// upstream rejected ANY reasoning knob with the effort-conflict 400) and
// reports whether this call newly learned it, so callers can log once.
func (d *Def) LearnNoThinking(model string) bool {
	d.learnedMu.Lock()
	defer d.learnedMu.Unlock()
	if d.learnedNT == nil {
		d.learnedNT = make(map[string]struct{})
	}
	if _, ok := d.learnedNT[model]; ok {
		return false
	}
	d.learnedNT[model] = struct{}{}
	return true
}

// NoThinkingModel reports whether the routed upstream model matches one of
// the provider's no-thinking globs (path.Match syntax) or was learned
// no-thinking at runtime (see LearnNoThinking).
func (d *Def) NoThinkingModel(model string) bool {
	for _, pat := range d.NoThinking {
		if ok, err := path.Match(pat, model); err == nil && ok {
			return true
		}
	}
	d.learnedMu.RLock()
	defer d.learnedMu.RUnlock()
	_, ok := d.learnedNT[model]
	return ok
}

// ReasoningEchoModel reports whether the routed upstream model matches the
// provider's echo_reasoning globs (path.Match syntax): a thinking-mode
// upstream that validates the REPLAYED history and 400s when a tool-loop
// continuation carries an assistant turn without reasoning_content. The
// server synthesizes the missing echo for these models (server
// synthesizeReasoningEcho).
func (d *Def) ReasoningEchoModel(model string) bool {
	for _, pat := range d.EchoReasoning {
		if ok, err := path.Match(pat, model); err == nil && ok {
			return true
		}
	}
	return false
}

// RotationPolicy is the operator-tunable half of the rotation mechanics
// (#84): the 429 cooldown ladder, the provider-wide edge breaker, and the
// model-scoped bench TTL. Zero fields mean "use the shipped default", so a
// config that says nothing behaves EXACTLY as before this existed.
//
// Deliberately NOT here: which errors rotate at all, and how they are
// classified. Those rules came from live incidents (the shared-wall family,
// model-scoped refusals, the gated-403 deposit contract, wording-vs-behaviour
// burst walls) — promoting them to knobs would let a misconfiguration undo an
// RCA, and onegw has no multi-tenant operator to serve.
type RotationPolicy struct {
	CoolBase      time.Duration // 429 ladder start (default coolBase)
	CoolCap       time.Duration // 429 ladder ceiling (default coolCap)
	FlapThreshold int           // consecutive edge faults before opening (default flapThreshold)
	FlapOpen      time.Duration // whole-pool park while open (default flapOpen)
	BenchTTL      time.Duration // model-scoped bench from a refusal (0 = ModelBenchTTL)
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
		ttl = d.benchTTL() // #84: per-provider override, else ModelBenchTTL
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
	// Benches never SHORTEN an existing verdict: the burst-wall park
	// (wallParkTTL) and a model-scoped refusal (ModelBenchTTL) can strike
	// the same pair within seconds, and the longer window carries the
	// stronger evidence — mirrors cool()/rateLimited max semantics (a
	// missing entry reads zero-time, so the max is always the set).
	if t := time.Now().Add(ttl); t.After(d.modelBench[model]) {
		d.modelBench[model] = t
	}
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
		d.pool.policy = d.Rotation // #84: zero fields keep the package defaults
		d.pool.selection, d.pool.headroom = d.Selection, d.Headroom
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
			d.pool.policy = d.Rotation // #84: zero fields keep the package defaults
			d.pool.selection, d.pool.headroom = d.Selection, d.Headroom
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

// Burst-wall detection (live 2026-09-11, ring seqs 5960-6000): b-ai's
// one-api edge answers bursty shared-limit pressure with a raw 429 and an
// EMPTY body — no "concurrency limit"/"TPM limit" wording, no Retry-After,
// no request-count window, so the 103c253 text classifiers see nothing and
// every such 429 takes the per-key ladder. The ring shows three DIFFERENT
// accounts 429ing within 2s (5977 mnhatlinh, 5978/5979 clone2) while the
// same accounts served 200s seconds later — the cross-account clustering
// that by definition marks a shared lane, not per-key exhaustion.
const (
	// wallWindow is the clustering horizon: 429s from two distinct
	// accounts of one (provider, model) inside it read as one shared
	// burst. Ring clusters land within ~2s; 5s catches spaced bursts.
	wallWindow = 5 * time.Second
	// wallParkTTL is the (provider, model) park once a burst is proven:
	// the observed burst window is ~5s (coolBase's rationale) + 1s
	// margin. A wrong park costs one combo leg for 6s and self-heals —
	// the first request after expiry re-discovers and, if the wall is
	// still up, re-parks after two 429s.
	wallParkTTL = 6 * time.Second
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

type accountState struct {
	acct        Account
	invalidated bool         // terminal: vendor refused the key for billing (#80); no timer clears it
	lastUsed    time.Time    // when this slot last served (least-used/recency, #81)
	cooldown    time.Time    // until when the account is skipped
	benchedAt   time.Time    // when the ACTIVE cooldown was stamped (ok() recency rule)
	strikes     int          // consecutive 429s (adaptive ladder); reset on success
	bucket      *tokenBucket // RPM governor; nil = uncapped (shared across slots)
	speed       speedSample  // recent decode speed of this account (tokens/sec)
	// live counts the upstream calls currently in flight on this slot.
	// next() prefers the least-busy open slot: per-key concurrency is ~1
	// on b-ai's free keys (2026-09-11 live: 3 identical 330K-token prefills
	// stacked on ONE key serialized to TTFB 29s / 32s / 170s — the 170s
	// attempt is the seq-879 504 shape, past the 75s header budget).
	// Cooldowns and RPM buckets cannot express this: a prefill occupies
	// the key for tens of seconds while its bucket token refills in 12s.
	live int
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

	// policy is the operator-tunable rotation mechanics (#84); zero fields
	// resolve to the package defaults via the ladderBase/flapTrip family.
	policy RotationPolicy

	// Selection (#81) picks among the OPEN slots: "" = the shipped
	// behavior (fastest decode speed, round-robin among equals), or one of
	// "p2c", "least-used", "strict-random", "random". pickN and headroom
	// are injectable so the choice is deterministic in tests and can weigh
	// subscription headroom (#79) without this package importing it.
	selection string
	deck      []int
	deckPos   int
	pickN     func(n int) int
	headroom  func(acct string) (float64, bool)

	// Burst-wall evidence (wallStrike): the last wording-less 429 per
	// routed model, so the second distinct account within wallWindow
	// proves the shared lane. Keys are model names — cardinality bounded
	// by the route tables, like modelBench.
	walls map[string]wallSight
}

// stickyPin is one identity's pinned account, matched by Name+APIKey like
// cool() so weighted slot expansion never breaks a pin.
type stickyPin struct {
	name, key string
	expires   time.Time
}

// ladderBase/ladderCap/flapTrip/flapPark resolve a pool's RotationPolicy
// against the shipped defaults: every consumer goes through them so a
// hand-built pool (tests) and a config-less pool behave identically to the
// pre-#84 constants.
func (p *accountPool) ladderBase() time.Duration {
	if p.policy.CoolBase > 0 {
		return p.policy.CoolBase
	}
	return coolBase
}

func (p *accountPool) ladderCap() time.Duration {
	if p.policy.CoolCap > 0 {
		return p.policy.CoolCap
	}
	return coolCap
}

func (p *accountPool) flapTrip() int {
	if p.policy.FlapThreshold > 0 {
		return p.policy.FlapThreshold
	}
	return flapThreshold
}

func (p *accountPool) flapPark() time.Duration {
	if p.policy.FlapOpen > 0 {
		return p.policy.FlapOpen
	}
	return flapOpen
}

func (d *Def) benchTTL() time.Duration {
	if d != nil && d.Rotation.BenchTTL > 0 {
		return d.Rotation.BenchTTL
	}
	return ModelBenchTTL
}

func newAccountPool(accts []Account, sticky time.Duration, sharedRPM int) *accountPool {
	if len(accts) == 0 {
		accts = []Account{{Name: "default"}}
	}
	p := &accountPool{ttl: sticky, now: time.Now, pickN: rand.IntN}
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
	// keepPin: the pinned account is up but already serving another call.
	// This pick spreads (occupied slots lose the least-busy comparison
	// below) while the warm-pin claim itself survives — dropping it here
	// would migrate the identity to whichever account served the
	// concurrent request, and the pin exists for cache warmth.
	keepPin := false
	if p.ttl > 0 && id != "" {
		if pin, ok := p.sticky[id]; ok && now.Before(pin.expires) {
			for i := range p.accts {
				if s := &p.accts[i]; s.acct.Name == pin.name && s.acct.APIKey == pin.key {
					if ok, _ := p.available(s, now); ok && p.sharedReady(now).IsZero() {
						// The pin is a cache-warmth preference, not an
						// admission right: a pinned account already
						// serving an upstream call yields to the
						// least-busy scan instead of stacking a second
						// attempt behind it (the seq-879 shape). It
						// matters most where identity collapses to one
						// client key (requestIdentity's label fallback):
						// there the pin would otherwise funnel every
						// concurrent turn of every session onto ONE
						// credential — the herd this pick rule breaks.
						if s.live == 0 {
							p.grant(s, now)
							return &s.acct, time.Time{}
						}
						keepPin = true
						break
					}
					start = i + 1 // pinned account cooling or governed: rotate past it
					break
				}
			}
		}
		if !keepPin {
			delete(p.sticky, id)
		}
	}
	var ready time.Time // soonest cooldown expiry / bucket refill among blocked accounts
	anyOpen := false    // some slot passes its own gates but the shared budget is empty
	sharedReady := p.sharedReady(now)
	// Collect the OPEN slots in round-robin order from start, then let the
	// configured strategy pick among them (#81). Every mode shares these
	// gates, so no strategy can select a blocked slot; the default mode
	// reproduces the shipped pick exactly (least-busy, tie-break by decode
	// speed — accounts carry a speed EWMA, so idle traffic still prefers the
	// quicker credential while a slot with a live upstream call yields to an
	// idle sibling; ties keep round-robin order).
	open := make([]int, 0, n)
	for i := range n {
		s := &p.accts[(start+i)%n]
		if ok, r := p.available(s, now); ok {
			if sharedReady.IsZero() {
				open = append(open, (start+i)%n)
			} else {
				anyOpen = true // could serve at the shared refill — but not before
			}
		} else if !r.IsZero() {
			if ready.IsZero() || r.Before(ready) {
				ready = r
			}
		}
	}
	if len(open) > 0 {
		chosen := p.pickSlot(open, now)
		s := &p.accts[chosen]
		p.grant(s, now)
		p.rr = (uint64(chosen) + 1) % uint64(n)
		if !keepPin {
			p.pin(id, &s.acct, now)
		}
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
	s.lastUsed = now
	if p.shared != nil {
		p.shared.take(now)
	}
	if s.bucket != nil {
		s.bucket.take(now)
	}
}

// begin marks one upstream call in flight on the account's slot and reports
// the slot's new depth (0 when the account is not a pool slot, e.g. a
// hand-built Account in tests). Callers MUST pair it with end via defer:
// Do's return paths include the header-budget abort and the semaphore
// cancel, none of which report an outcome to the pool — releasing from the
// 429/403/success hooks instead would leak a permanent +1 on exactly the
// keys that storm, and once every slot reads busy the pick collapses back
// to speed-only selection.
func (p *accountPool) begin(a *Account) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.slot(a)
	if s == nil {
		return 0
	}
	s.live++
	return s.live
}

// end releases the in-flight mark taken by begin. A miss is a no-op so a
// pool rebuilt mid-request (SIGHUP reload) cannot drive a slot negative.
func (p *accountPool) end(a *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.slot(a); s != nil && s.live > 0 {
		s.live--
	}
}

// slot resolves an Account to its pool slot by name+key: callers may hold
// a copy rather than the pool's own instance (tests pass &def.Accounts[i],
// the sticky-pin path looks slots up the same way).
func (p *accountPool) slot(a *Account) *accountState {
	if a == nil {
		return nil
	}
	for i := range p.accts {
		if s := &p.accts[i]; s.acct.Name == a.Name && s.acct.APIKey == a.APIKey {
			return s
		}
	}
	return nil
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
	if s.invalidated {
		// Terminal: no instant makes this slot ready (next() ignores a zero
		// "ready" when computing the pool's soonest-recovery, so a fully
		// invalidated pool honestly reports "never" rather than a bogus
		// Retry-After). Only Revalidate — operator action, or a reload that
		// changed the credential — clears it.
		return false, time.Time{}
	}
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

// ok records a successful call on a that began at `since`: the strike
// count always resets — the next 429 starts the ladder at coolBase again —
// but an active cooldown is erased ONLY when it was stamped BEFORE the
// request started (a bench predating this success is older evidence than
// it: the gated-403 deposit recovery and post-expiry probes). A cooldown
// stamped DURING the request's flight is a fresher verdict than the
// success and must survive it: the pool grants the same account to
// overlapping requests, so a straggler success can complete AFTER a
// concurrent request 429ed that account and benched it. Live 2026-09-11
// (ring seqs 5960-6000): mnhatlinh 429ed at 5977 and served a 200 at
// 5983, 4s into its 10s bench — the bench vanished to a racing success
// and the picker re-entered the same burst wall on the same key minutes
// later (429 again at 5991). Cooldowns expire on their own; coolCap
// bounds the wait. All slots of the account are touched — weighted pools
// expand one account into several slots.
func (p *accountPool) ok(a *Account, since time.Time) {
	if p == nil || a == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accts {
		if s := &p.accts[i]; s.acct.Name == a.Name && s.acct.APIKey == a.APIKey {
			s.strikes = 0
			if !s.cooldown.IsZero() && s.benchedAt.Before(since) {
				s.cooldown = time.Time{}
				s.benchedAt = time.Time{}
			}
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
	if p.flapStrikes >= p.flapTrip() {
		if t := p.now().Add(p.flapPark()); t.After(p.flapOpenUntil) {
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
		d = p.ladderBase() << uint(min(cur, 3))
		if d > p.ladderCap() {
			d = p.ladderCap()
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
				s.benchedAt = now
			}
		}
	}
}

// wallSight is one burst-wall data point: a wording-less 429 from acct at
// time t against model.
type wallSight struct {
	acct string
	at   time.Time
}

// wallStrike records a ladder-path 429 from acct against model and
// reports whether it COMPLETES a proven shared-wall burst inside
// wallWindow. Two evidence modes:
//   - distinct (text=false): a DIFFERENT account struck the same model
//     within the window — the 103c253 behavioural rule for walls whose
//     wording says nothing (empty-body one-api 429s).
//   - text (wording-matched model walls): the message itself proves the
//     lane is shared, so any two strikes within the window complete the
//     burst — a lone sighting stays on the fall-through/replay path
//     (the fast path's whole-body replay rides out the transient window,
//     2026-09-09; TestStreamFastPathReplaysWholeBodyOnTransient429), and
//     only the second strike parks the (provider, model) pair so sibling
//     requests stop re-discovering a SUSTAINED wall.
//
// On a proven burst the sight resets, so a continuing wall must re-prove
// itself for every park instead of latching on stale evidence.
func (p *accountPool) wallStrike(model, acct string, text bool) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if prev, ok := p.walls[model]; ok && now.Sub(prev.at) <= wallWindow &&
		(text || prev.acct != acct) {
		delete(p.walls, model)
		return true
	}
	if p.walls == nil {
		p.walls = make(map[string]wallSight)
	}
	p.walls[model] = wallSight{acct: acct, at: now}
	return false
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
		if s := &p.accts[i]; s.acct.Name == a.Name && s.acct.APIKey == a.APIKey {
			if s.cooldown.Before(now) {
				s.cooldown = now
			}
			// Extend to the farthest slot so weighted pools cool as one.
			if t := now.Add(d); t.After(s.cooldown) {
				s.cooldown = t
				s.benchedAt = now
			}
		}
	}
}

// invalidate marks every slot of a terminal for selection (#80): the vendor
// refused this credential for billing reasons, a condition no amount of
// waiting fixes. Returns true when the account was not already terminal, so
// callers log once per invalidation instead of once per request.
func (p *accountPool) invalidate(a *Account) bool {
	if p == nil || a == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	fresh := false
	for i := range p.accts {
		if s := &p.accts[i]; s.acct.Name == a.Name && s.acct.APIKey == a.APIKey {
			if !s.invalidated {
				s.invalidated = true
				fresh = true
			}
		}
	}
	return fresh
}

// revalidate clears an account's terminal state (operator action, or the
// config reload path in CarryInvalidated when the credential changed).
// Returns true when something was actually cleared.
func (p *accountPool) revalidate(a *Account) bool {
	if p == nil || a == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cleared := false
	for i := range p.accts {
		if s := &p.accts[i]; s.acct.Name == a.Name && s.acct.APIKey == a.APIKey {
			if s.invalidated {
				s.invalidated = false
				s.strikes = 0
				s.cooldown = time.Time{}
				s.benchedAt = time.Time{}
				cleared = true
			}
		}
	}
	return cleared
}

func (p *accountPool) invalidatedNames() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for i := range p.accts {
		if s := &p.accts[i]; s.invalidated && !seen[s.acct.Name] {
			seen[s.acct.Name] = true
			out = append(out, s.acct.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Invalidate marks an account terminal after an upstream billing refusal
// (402 / insufficient_quota — see types.APIError.PaymentRequired). It joins
// the pool's fault state alongside the 429 ladder, but with no timer: the key
// stays out of rotation until Revalidate or a reload that rotates the
// credential. Returns true when this call newly invalidated the account.
func (d *Def) Invalidate(a *Account) bool { return d.pool.invalidate(a) }

// Revalidate clears an account's terminal state (dashboard/API action).
func (d *Def) Revalidate(a *Account) bool { return d.pool.revalidate(a) }

// RevalidateByName clears the named account; false when no account by that
// name exists or none was terminal.
func (d *Def) RevalidateByName(name string) bool {
	for i := range d.Accounts {
		if d.Accounts[i].Name == name && d.pool.revalidate(&d.Accounts[i]) {
			return true
		}
	}
	return false
}

// Invalidated lists terminal account names (sorted) for the dashboard and
// the pool-empty error message.
func (d *Def) Invalidated() []string { return d.pool.invalidatedNames() }

// AllInvalidated reports whether the pool has accounts and every one of them
// is terminal — the case where a provider's whole billing relationship is
// dead and the honest client answer is a billing error, not a rate limit.
func (d *Def) AllInvalidated() bool {
	if d.pool == nil || len(d.Accounts) == 0 {
		return false
	}
	return len(d.pool.invalidatedNames()) >= len(d.Accounts)
}

// CarryInvalidated copies terminal (billing-invalidated) accounts from an old
// account set onto a freshly built one (#80): a hot reload must not silently
// resurrect a key the vendor refused, because the very next request would
// burn a doomed upstream attempt to learn it again. Matching is by account
// name AND credential, so a rotated api_key under the same account name is a
// different credential and starts active — which is exactly the recovery
// path an operator takes when they top a balance back up.
func CarryInvalidated(old *Pool, fresh *Pool) {
	if old == nil || fresh == nil {
		return
	}
	old.mu.RLock()
	defer old.mu.RUnlock()
	fresh.mu.Lock()
	defer fresh.mu.Unlock()
	for name, od := range old.byName {
		nd, ok := fresh.byName[name]
		if !ok || od == nil || od.pool == nil || nd == nil || nd.pool == nil {
			continue
		}
		term := make(map[string]struct{}, len(od.pool.accts))
		for i := range od.pool.accts {
			if s := &od.pool.accts[i]; s.invalidated {
				term[s.acct.Name+"\x00"+s.acct.APIKey] = struct{}{}
			}
		}
		if len(term) == 0 {
			continue
		}
		for i := range nd.pool.accts {
			s := &nd.pool.accts[i]
			if _, ok := term[s.acct.Name+"\x00"+s.acct.APIKey]; ok {
				s.invalidated = true
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
	// Prefill is the measured pre-first-byte phase of THIS attempt: dial and
	// upload of the request body plus the upstream's queue and prefill,
	// ending when the response headers arrived. It is the term that dominates
	// large-context requests, and the one decode speed cannot see
	// (provider/prefill.go).
	Prefill time.Duration
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
	reqStart := time.Now() // ok()'s recency rule: benches stamped during this request's flight outlive it
	if d.inflight != nil {
		select {
		case d.inflight <- struct{}{}:
			defer func() { <-d.inflight }()
		case <-ctx.Done():
			return nil, &types.APIError{Status: 499, Type: "client_closed", Message: ctx.Err().Error()}
		}
	}
	if d.pool != nil && d.pool.begin(acct) > 0 {
		// Occupancy for the pick loop (see accountState.live): the slot
		// reads as busy for as long as this attempt waits on the upstream,
		// so concurrent selections spread instead of stacking on the
		// fastest key. Deferred, NOT reported from the outcome hooks:
		// the header-budget abort and the semaphore cancel return without
		// telling the pool anything, and a leaked +1 would silently
		// retire this slot from least-busy selection.
		defer d.pool.end(acct)
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
			// Shared-limit 429s skip the ladder: the limit is the
			// upstream's model-wide budget (all of the reseller's
			// traffic), not this key's — benching healthy keys only
			// shrinks the serving pool while the shared window clears by
			// itself. A second, behavioural detection catches the
			// walls the wording cannot (b-ai's EMPTY-BODY 429s, live
			// 2026-09-11 seqs 5960-6000: no Retry-After, no window, no
			// limit text, yet three DIFFERENT accounts struck within 2s
			// while serving 200s seconds later): wallStrike compares
			// accounts, not wording — a second distinct account inside
			// wallWindow proves the shared lane, sets SharedWall so
			// Router.Execute falls through to the next combo leg
			// immediately, and parks the (provider, model) pair so
			// sibling requests skip re-discovery for one burst window.
			// Wording-matched walls park too, but only on a SECOND sight
			// within the window (wallStrike text mode): a lone
			// Concurrency-limit 429 is the transient the whole-body
			// replay rides out, and a first-sight park turned that replay
			// into a client-visible 503 (reverted once already — see
			// 1da3c2e; TestStreamFastPathReplaysWholeBodyOnTransient429
			// pins the contract).
			shared := sharedLimit429(apiErr.Status, apiErr.Code, apiErr.Message)
			if !shared {
				// Burst detection applies only to a wall with nothing to
				// go on: RetryAfter is empty exactly when neither the
				// header nor the body named a duration (the empty-body
				// one-api shape). A 429 that states its own window knows
				// its scope — the ladder rides it verbatim.
				if apiErr.RetryAfter != "" || !d.pool.wallStrike(model, acct.Name, false) {
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
				} else {
					// Proven shared burst: the key is healthy — benching
					// it only shrinks the serving pool across the park.
					apiErr.SharedWall = true
					d.BenchModel(model, wallParkTTL)
				}
			} else if apiErr.ModelWall() && d.pool.wallStrike(model, acct.Name, true) {
				// Sustained wording wall (second strike in the window):
				// park so siblings skip re-discovery. No SharedWall flag
				// and no key bench — the text already classifies this 429
				// shared, so Execute fell through on this request too;
				// the park only saves the NEXT requests their one doomed
				// attempt.
				d.BenchModel(model, wallParkTTL)
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
			// repeat hits double to coolCap; once the deposit lands the
			// bench lifts on cooldown expiry and the next success resets
			// the ladder via pool.ok) and mark the error Fallbackable so
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
			// Pass 0 (not ModelBenchTTL) so the #84 per-provider knob
			// (model_bench_ttl) governs this bench; the wallParkTTL and
			// headerTimeoutBench sites above are congestion-scale waits and
			// deliberately do NOT follow the knob.
			d.BenchModel(model, 0)
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
	// The recency rule compares benches against WHEN THIS REQUEST BEGAN:
	// a bench stamped at any point during the flight (a concurrent 429 on
	// the same account) is a fresher verdict than this success and
	// survives it; a bench predating the request is older evidence and
	// clears (the #48 deposit-recovery path).
	d.pool.ok(acct, reqStart) // success resets the 429 ladder (fresh verdicts only)
	d.pool.flapHeal()         // and closes the flap breaker: the edge is serving
	return &CallResult{Resp: resp, Format: d.UpstreamFormat(model), Acct: acct,
		FirstByte: time.Now(), Prefill: time.Since(reqStart)}, nil
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
	if d.pool != nil && d.pool.begin(acct) > 0 {
		// Same occupancy contract as Do: the slot is busy until the
		// upstream answers, and the deferred end covers the early
		// internal-error return below.
		defer d.pool.end(acct)
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

// SetRotationPolicy installs the operator-tunable rotation mechanics (#84)
// on the Def and any pool already built for it. Call before Pool.Set for a
// fresh def; safe on a live def too (the field is read under the pool lock).
func (d *Def) SetRotationPolicy(pol RotationPolicy) {
	d.Rotation = pol
	if d.pool != nil {
		d.pool.policy = pol
	}
}

// ---------------------------------------------------------------------------
// Selection strategies (#81)
// ---------------------------------------------------------------------------

// p2cSpeedRef is the decode speed (tok/s) at which the P2C speed penalty
// saturates: at or above it an account pays nothing for being slow, below it
// the penalty grows linearly. 100 tok/s is the observed good-provider band
// (the throughput session measured b-ai 40-76, commandcode ~51, glm ~25).
const p2cSpeedRef = 100.0

// pickSlot chooses one of the open slots (absolute indexes, round-robin order
// from the pool's cursor). The default preserves the shipped behavior exactly:
// fastest open slot, round-robin order among equal/no-data speeds.
func (p *accountPool) pickSlot(open []int, now time.Time) int {
	if len(open) == 1 {
		return open[0]
	}
	// Occupancy gate, applied to EVERY mode: per-key concurrency is ~1 on
	// several upstreams, so a second concurrent call on a key that already
	// has one in flight queues upstream and rides past the pre-first-byte
	// budget (2026-09-11: 3 stacked 330K prefills → TTFB 29s/32s/170s, the
	// last the seq-879 504). Narrow to the least-busy slots first; a
	// strategy may then choose freely among those, but none can stack on an
	// idle sibling's key.
	minLive := p.accts[open[0]].live
	for _, i := range open[1:] {
		if l := p.accts[i].live; l < minLive {
			minLive = l
		}
	}
	// Always narrow (not just when everything is busy): with a=1, b=0 the
	// minimum is 0 and the busy slot must be excluded — gating only on
	// minLive > 0 let the default mode spend an idle slot's turn on a key
	// that already had a call in flight.
	idle := make([]int, 0, len(open))
	for _, i := range open {
		if p.accts[i].live == minLive {
			idle = append(idle, i)
		}
	}
	if len(idle) > 0 {
		open = idle
	}
	if len(open) == 1 {
		return open[0]
	}
	switch p.selection {
	case "least-used":
		best := open[0]
		for _, i := range open[1:] {
			a, b := p.accts[i].lastUsed, p.accts[best].lastUsed
			if a.IsZero() {
				return i // never served beats any served slot
			}
			if b.IsZero() {
				continue
			}
			if a.Before(b) {
				best = i
			}
		}
		return best
	case "random":
		return open[p.pickN(len(open))]
	case "strict-random":
		return p.deckPick(open)
	case "p2c":
		if len(open) == 2 {
			return p.cheaper(open[0], open[1], now)
		}
		i := p.pickN(len(open))
		j := p.pickN(len(open) - 1)
		if j >= i {
			j++
		}
		return p.cheaper(open[i], open[j], now)
	default: // "" = shipped: least-busy (already filtered), then fastest,
		// then round-robin order among equals (strictly-greater keeps the
		// first candidate).
		best := open[0]
		for _, i := range open[1:] {
			if p.accts[i].speed.tps() > p.accts[best].speed.tps() {
				best = i
			}
		}
		return best
	}
}

// cheaper returns the lower-scoring of two slots; ties keep a (round-robin
// order), matching the default mode's strictly-greater comparison.
func (p *accountPool) cheaper(a, b int, now time.Time) int {
	sa, sb := p.score(a, now), p.score(b, now)
	if sb < sa {
		return b
	}
	return a
}

// score is the P2C cost of serving from slot i: lower is better. It is built
// from signals the pool already owns plus an optional headroom probe, which is
// what makes it different from "fastest": a recently-429ed or nearly-spent
// key is avoided even while its gates are technically open, so load moves
// before a failure forces it.
func (p *accountPool) score(i int, now time.Time) float64 {
	s := &p.accts[i]
	// Recently rate-limited: the strike count already gated the cooldown, so
	// this only breaks ties among open slots — a key that just recovered is
	// still the likelier one to hit the wall again.
	score := float64(min(s.strikes, 5)) * 8
	// Speed: unknown keys take a middle penalty rather than being starved
	// (a fresh pool has no samples and must still circulate).
	if t := s.speed.tps(); t > 0 {
		score += 40 * (1 - math.Min(t, p2cSpeedRef)/p2cSpeedRef)
	} else {
		score += 20
	}
	// Recency: discourage hammering the key that just served (bounded, so a
	// single fast account is still preferred over a slow one).
	if !s.lastUsed.IsZero() {
		if rec := now.Sub(s.lastUsed); rec > 0 && rec < 60*time.Second {
			score += 12 * (1 - rec.Seconds()/60)
		}
	}
	// Subscription headroom (#79): a key whose plan is nearly spent is
	// deprioritized BEFORE it starts refusing.
	if p.headroom != nil {
		if h, ok := p.headroom(s.acct.Name); ok {
			score += math.Min(80, (100-h)/1.25)
			if h <= 10 {
				score += 10 // about to be unusable: strong, not absolute (others may be worse)
			}
		}
	}
	return score
}

// deckPick serves strict-random: each open slot is used once per shuffled
// deck before any repeats (uniform wear without the clustering of pure
// randomness). The deck holds slot indexes; entries that are not open now are
// skipped and retried on a later request.
func (p *accountPool) deckPick(open []int) int {
	inOpen := func(i int) bool {
		for _, o := range open {
			if o == i {
				return true
			}
		}
		return false
	}
	for tries := 0; tries < 2; tries++ {
		for p.deckPos < len(p.deck) {
			i := p.deck[p.deckPos]
			p.deckPos++
			if inOpen(i) {
				return i
			}
		}
		// Deck exhausted (or all remaining entries are blocked): reshuffle.
		p.deck = make([]int, len(p.accts))
		for i := range p.deck {
			p.deck[i] = i
		}
		rand.Shuffle(len(p.deck), func(a, b int) { p.deck[a], p.deck[b] = p.deck[b], p.deck[a] })
		p.deckPos = 0
	}
	return open[0]
}

// SetSelection installs the pick strategy and optional headroom probe (#81).
func (d *Def) SetSelection(mode string, headroom func(acct string) (float64, bool)) {
	d.Selection = mode
	if d.pool != nil {
		d.pool.selection = mode
		d.pool.headroom = headroom
		d.pool.deck, d.pool.deckPos = nil, 0
	}
}
