// Package provider defines the upstream provider abstraction: named kinds
// (openai, anthropic, gemini, openai-compatible), account pools, and the
// single HTTP call contract the router drives.
package provider

import (
	"bytes"
	"context"
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
)

// Format returns the wire format a kind speaks.
func (k Kind) Format() translat.Format {
	switch k {
	case KindAnthropic:
		return translat.FmtAnthropic
	case KindGemini:
		return translat.FmtGemini
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
}

// Def is a configured provider instance: a kind + endpoint defaults + its
// account pool.
type Def struct {
	Name     string    `toml:"name"` // e.g. "openrouter", "glm"
	Kind     Kind      `toml:"kind"`
	BaseURL  string    `toml:"base_url"`
	Accounts []Account `toml:"accounts"`

	// Concurrency cap for in-flight upstream calls (0 = unlimited).
	MaxConc int `toml:"max_concurrency"`

	// Headers added to every upstream request (auth handled separately).
	ExtraHeaders map[string]string `toml:"extra_headers"`

	// AlwaysThinking lists model globs (path.Match syntax; "*" does not
	// cross "/") whose upstreams reason unconditionally and reject
	// "disable thinking" knobs (e.g. GLM 1210: use low|high|max). The
	// server rewrites such requests instead of forwarding them.
	AlwaysThinking []string `toml:"always_thinking"`

	pool     *accountPool
	inflight chan struct{}
}

// AlwaysThinkingModel reports whether the routed upstream model matches one
// of the provider's always-thinking globs (path.Match syntax).
func (d *Def) AlwaysThinkingModel(model string) bool {
	for _, pat := range d.AlwaysThinking {
		if ok, err := path.Match(pat, model); err == nil && ok {
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
		d.pool = newAccountPool(d.Accounts)
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
			d.pool = newAccountPool(d.Accounts)
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

// DefaultBaseURL gives the stock endpoint for a kind when unset.
func (k Kind) DefaultBaseURL() string {
	switch k {
	case KindAnthropic:
		return "https://api.anthropic.com"
	case KindGemini:
		return "https://generativelanguage.googleapis.com"
	default:
		return "https://api.openai.com"
	}
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

// Account pool: weighted round-robin with cooldown on quota errors
// ---------------------------------------------------------------------------

// NextAccount picks the next available account (weighted round-robin with
// quota cooldowns).
func (d *Def) NextAccount() *Account { return d.pool.next() }

// Cool marks an account as cooling after a quota error.
func (d *Def) Cool(a *Account, dDur time.Duration) { d.pool.cool(a, dDur) }

type accountState struct {
	acct     Account
	cooldown time.Time // until when the account is skipped
}

type accountPool struct {
	mu      sync.Mutex
	accts   []accountState
	rr      uint64
	stopped bool
}

func newAccountPool(accts []Account) *accountPool {
	if len(accts) == 0 {
		accts = []Account{{Name: "default"}}
	}
	p := &accountPool{}
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

// next picks the next non-cooling account; nil = all cooling (caller picks
// first anyway so the error names a concrete cause).
func (p *accountPool) next() *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	n := len(p.accts)
	for i := 0; i < n; i++ {
		s := &p.accts[(int(p.rr)+i)%n]
		if now.After(s.cooldown) {
			p.rr = (uint64(int(p.rr)+i) + 1) % uint64(n)
			return &s.acct
		}
	}
	s := &p.accts[int(p.rr)%n]
	p.rr = (p.rr + 1) % uint64(n)
	return &s.acct
}

// cool marks an account cooling for d (quota exhausted).
func (p *accountPool) cool(a *Account, d time.Duration) {
	if a == nil {
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

var client = &http.Client{
	Transport: &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 60 * time.Second}).DialContext,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	},
	Timeout: 0, // streams are long-lived; per-request ctx governs
}

// CallResult bundles the upstream HTTP response for the router to stream.
type CallResult struct {
	Resp   *http.Response
	Format translat.Format
	Acct   *Account
}

// Path builds the upstream URL path for a kind from the client's path intent.
// `op` is "chat" (chat completions / messages / generateContent).
func (d *Def) Path(op string) string {
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
	default:
		if op == "models" {
			return "/v1/models"
		}
		return "/v1/chat/completions"
	}
}

// Do performs one upstream call. body may be nil. The caller owns Resp.Body.
func (d *Def) Do(ctx context.Context, acct *Account, model string, body []byte, stream bool) (*CallResult, *types.APIError) {
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
	case KindGemini:
		// Non-streaming: :generateContent; streaming: :streamGenerateContent?alt=sse
		method := "generateContent"
		qs := ""
		if stream {
			method = "streamGenerateContent"
			qs = "?alt=sse"
		}
		url = fmt.Sprintf("%s/models/%s:%s%s", joinURL(base, "/v1beta"), model, method, qs)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("x-goog-api-key", acct.APIKey)
		}
	default:
		url = joinURL(base, d.Path("chat"))
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err == nil {
			switch d.Kind {
			case KindAnthropic:
				req.Header.Set("x-api-key", acct.APIKey)
				req.Header.Set("anthropic-version", "2023-06-01")
			default:
				req.Header.Set("Authorization", "Bearer "+acct.APIKey)
			}
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
	resp, err := client.Do(req)
	if err != nil {
		return nil, &types.APIError{Status: 502, Type: "upstream_unreachable", Message: err.Error()}
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		apiErr := decodeUpstreamError(d.Kind, limited, resp.StatusCode)
		if apiErr.OverQuota() && acct != nil {
			d.pool.cool(acct, coolDuration(resp.Header.Get("Retry-After")))
		}
		return nil, apiErr
	}
	return &CallResult{Resp: resp, Format: d.Kind.Format(), Acct: acct}, nil
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
	url := joinURL(base, d.Path("models"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 500, err
	}
	switch d.Kind {
	case KindAnthropic:
		req.Header.Set("x-api-key", acct.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	case KindGemini:
		req.Header.Set("x-goog-api-key", acct.APIKey)
	default:
		req.Header.Set("Authorization", "Bearer "+acct.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 502, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return body, resp.StatusCode, err
}
