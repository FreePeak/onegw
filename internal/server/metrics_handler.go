// Package server — metrics_handler.go wires the Prometheus /metrics surface
// onto the gateway's request paths.
package server

import (
	"net/http"
	"strconv"
	"time"

	"onegw/internal/metrics"
	"onegw/internal/types"
)

// gatewayMetrics owns the process-lifetime Prometheus registry. It lives on
// Server (not in the hot-reloadable state) so counters survive config
// reloads and stay cumulative for the process lifetime.
type gatewayMetrics struct {
	reg      *metrics.Registry
	requests *metrics.Family // onegw_requests_total{provider,model,code}
	tokens   *metrics.Family // onegw_tokens_total{provider,model,type}
	errors   *metrics.Family // onegw_request_errors_total{provider,kind}

	inflight   *metrics.Family // onegw_inflight
	budgetHeld *metrics.Family // onegw_budget_inflight_bytes
	budgetCap  *metrics.Family // onegw_budget_cap_bytes
	uptime     *metrics.Family // onegw_uptime_seconds
}

func newGatewayMetrics() *gatewayMetrics {
	reg := metrics.NewRegistry()
	return &gatewayMetrics{
		reg:      reg,
		requests: reg.Counter("onegw_requests_total", "Upstream attempts by routed provider, model, and HTTP outcome code; retries and combo fallbacks record one entry per target tried.", "provider", "model", "code"),
		tokens:   reg.Counter("onegw_tokens_total", "Tokens by routed provider, model, and type (input, output, cache_read, cache_write, reasoning, saved).", "provider", "model", "type"),
		errors:   reg.Counter("onegw_request_errors_total", "Failed request paths by provider and kind (upstream_error, budget_saturated, no_route); provider is empty when the request never reached a route.", "provider", "kind"),

		inflight:   reg.Gauge("onegw_inflight", "Requests currently live in the gateway pipeline."),
		budgetHeld: reg.Gauge("onegw_budget_inflight_bytes", "Bytes currently reserved under the global buffered-memory budget."),
		budgetCap:  reg.Gauge("onegw_budget_cap_bytes", "Capacity of the global buffered-memory budget in bytes."),
		uptime:     reg.Gauge("onegw_uptime_seconds", "Seconds since the gateway process started."),
	}
}

// success records one completed upstream attempt and its token accounting.
// Called exactly where usage.Observe runs so /metrics and /admin/usage agree.
func (m *gatewayMetrics) success(provider, model string, u types.Usage, savedTokens int64) {
	m.requests.Inc(provider, model, "200")
	for _, e := range [...]struct {
		typ string
		n   int64
	}{
		{"input", u.InputTokens},
		{"output", u.OutputTokens},
		{"cache_read", u.CacheReadTokens},
		{"cache_write", u.CacheWriteTokens},
		{"reasoning", u.ReasoningTokens},
		{"saved", savedTokens},
	} {
		if e.n != 0 {
			m.tokens.Add(e.n, provider, model, e.typ)
		}
	}
}

// boundedModel clamps a routed model string to config-defined routes
// ("provider/model" tables, combos, aliases, advertised models). Clients
// can name arbitrary models via direct "provider/model" strings or the
// bare-model fallback, and series are never evicted — unroutable names
// collapse to "unresolved" so metric cardinality stays bounded by config.
func (s *Server) boundedModel(model string) string {
	if s.cur().router.KnownModel(model) {
		return model
	}
	return "unresolved"
}

// upstreamErr records one failed upstream attempt (HTTP error from the
// provider, or a failure translating its stream).
func (m *gatewayMetrics) upstreamErr(provider, model string, status int) {
	m.requests.Inc(provider, model, strconv.Itoa(status))
	m.errors.Inc(provider, "upstream_error")
}

// noRoute records a request the router could not resolve (unknown provider /
// model, missing model field). The model label is the fixed "unresolved"
// placeholder: the client string never resolved to a route, and series are
// never evicted, so raw client strings must not become label values.
func (m *gatewayMetrics) noRoute(status int) {
	m.requests.Inc("", "unresolved", strconv.Itoa(status))
	m.errors.Inc("", "no_route")
}

// saturated records a request rejected because the buffered-memory budget
// could not be acquired.
func (m *gatewayMetrics) saturated() {
	m.requests.Inc("", "", "503")
	m.errors.Inc("", "budget_saturated")
}

// tooLarge records a request rejected because its body exceeded the body cap.
func (m *gatewayMetrics) tooLarge() {
	m.requests.Inc("", "", "413")
}

// invalidBody records an attempt aborted before the upstream call because
// the request body could not be prepared (translation/model rewrite failed).
// It is a client-side 400, not one of the three error kinds.
func (m *gatewayMetrics) invalidBody(provider, model string) {
	m.requests.Inc(provider, model, "400")
}

// handleMetrics serves GET /metrics in the Prometheus text exposition
//
// Auth: intentionally OPEN (no bearer/admin gate). The payload carries only
// aggregate counters and gauges — never API keys or request data — and
// Prometheus scrapes conventionally run unauthenticated inside the trust
// boundary; the gateway also binds 127.0.0.1 by default. This differs from
// /admin/health and /admin/usage, which are password-gated because they are
// part of the operator dashboard. Expose /metrics beyond loopback only
// behind a firewall, or add an admin gate here if your network is shared.
//
// Scrape safety: counters are updated on the request path with pure atomics
// (plus a read-locked map hit when a label set first appears); a scrape
// refreshes the gauges from live state and renders the registry without
// touching the usage tracker. Token totals are deliberately maintained as
// atomic counters at the usage.Observe call site instead of being derived
// from usage.Tracker.Snapshot() per scrape: Snapshot would take every shard
// mutex and copy all rollup buckets on each scrape, and hot reloads swap the
// tracker (resetting derived values), while these counters stay cumulative
// and lock-free. Per-key/per-hour token detail remains on /admin/usage.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m := s.m
	m.inflight.Set(s.inflight.Load())
	if st := s.cur(); st != nil && st.budget != nil {
		held, _ := st.budget.Stats()
		m.budgetHeld.Set(held)
		m.budgetCap.Set(st.budget.Capacity())
	}
	m.uptime.Set(int64(time.Since(s.start).Seconds()))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(m.reg.Render()))
}
