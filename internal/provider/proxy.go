package provider

// Shared upstream-proxy pool: one bulk URL list with rotation, opted into
// per provider. The pool is global (one [proxy] table); each opted-in Def
// gets its own selector cursor over the same URLs, so providers rotate
// independently (9router's per-provider rotateState, same shape).

import (
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
)

// ProxyPool is the resolved shared pool. URLs are the bulk proxy list,
// NoProxy the comma-separated bypass hosts, Rotation one of "" (default
// round-robin), "round-robin", "random", "order". Empty URLs = direct.
type ProxyPool struct {
	URLs     []string
	NoProxy  string
	Rotation string
}

// proxySelector picks the proxy URL for one request. The zero value is
// unusable; build with newProxySelector (nil when the pool is empty).
type proxySelector struct {
	urls     []*url.URL
	noProxy  []string
	strategy string
	rr       atomic.Uint64
}

// newProxySelector resolves the pool into a selector. Malformed entries
// are skipped defensively (config.Validate rejects them first, so this
// only fires on hand-built pools); nil when nothing usable remains.
func newProxySelector(pool ProxyPool) *proxySelector {
	var urls []*url.URL
	for _, s := range pool.URLs {
		u, err := url.Parse(strings.TrimSpace(s))
		if err != nil || u.Scheme == "" || u.Host == "" {
			continue
		}
		urls = append(urls, u)
	}
	if len(urls) == 0 {
		return nil
	}
	strat := strings.ToLower(strings.TrimSpace(pool.Rotation))
	if strat == "" {
		strat = "round-robin"
	}
	var noProxy []string
	for _, h := range strings.Split(pool.NoProxy, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			noProxy = append(noProxy, h)
		}
	}
	return &proxySelector{urls: urls, noProxy: noProxy, strategy: strat}
}

// proxyFunc matches http.Transport.Proxy: nil (direct) for bypassed hosts,
// else the next pool URL per the rotation strategy.
func (s *proxySelector) proxyFunc(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	if proxyBypassed(req.URL.Hostname(), s.noProxy) {
		return nil, nil
	}
	switch s.strategy {
	case "random":
		return s.urls[rand.IntN(len(s.urls))], nil
	case "order":
		// ponytail: naive pin to urls[0] with no failure-driven advance
		// (O(1) per request, no tracking state); upgrade path is advancing
		// on edgeFault/rateLimited strikes if operators ever need it.
		return s.urls[0], nil
	default: // round-robin
		return s.urls[(s.rr.Add(1)-1)%uint64(len(s.urls))], nil
	}
}

// proxyBypassed reports whether host skips the pool via the no_proxy list.
// Matches Go's NO_PROXY semantics: exact host, leading-dot suffix, or
// bare-suffix match, case-insensitive. Hostname() already strips the port
// and IPv6 brackets.
func proxyBypassed(host string, list []string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	for _, p := range list {
		switch {
		case p == "*":
			return true
		case h == p:
			return true
		case strings.HasPrefix(p, ".") && strings.HasSuffix(h, p):
			return true
		case strings.HasSuffix(h, "."+p):
			return true
		}
	}
	return false
}

// SetProxyPool installs the shared pool on an opted-in Def. Call before
// Pool.Set for a fresh def; safe on a live def too (the selector is read
// only via the memoized http client, rebuilt with the Def on reload).
func (d *Def) SetProxyPool(pool ProxyPool) {
	d.Proxy = pool
	d.proxySel = newProxySelector(pool)
}
