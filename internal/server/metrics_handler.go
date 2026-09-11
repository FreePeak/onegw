// Package server — metrics_handler.go wires the Prometheus /metrics surface
// onto the gateway's request paths.
package server

import (
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"onegw/internal/metrics"
	"onegw/internal/provider"
	"onegw/internal/types"
)

// gatewayMetrics owns the process-lifetime Prometheus registry. It lives on
// Server (not in the hot-reloadable state) so counters survive config
// reloads and stay cumulative for the process lifetime.
type gatewayMetrics struct {
	srv      *Server // back-reference for the #19 log ring; set once in New
	reg      *metrics.Registry
	requests *metrics.Family // onegw_requests_total{provider,model,code}
	tokens   *metrics.Family // onegw_tokens_total{provider,model,type}
	errors   *metrics.Family // onegw_request_errors_total{provider,kind}

	inflight   *metrics.Family // onegw_inflight
	budgetHeld *metrics.Family // onegw_budget_inflight_bytes
	budgetCap  *metrics.Family // onegw_budget_cap_bytes
	uptime     *metrics.Family // onegw_uptime_seconds
	provTPS    *metrics.Family // onegw_provider_tokens_per_second_x100
	clientTPS  *metrics.Family // onegw_client_delivered_tokens_per_second_x100
	clientTTFT *metrics.Family // onegw_client_tokens_to_first_byte_ms

	delivered *deliveredTracker // client-experienced tok/s + TTFT per client model
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
		provTPS:    reg.Gauge("onegw_provider_tokens_per_second_x100", "Decode-speed EWMA (output tokens/sec) per provider, scaled x100 (int64 registry); refreshed at scrape, absent until the provider served streaming replies.", "provider"),
		clientTPS:  reg.Gauge("onegw_client_delivered_tokens_per_second_x100", "Delivered tokens/sec EWMA per CLIENT model (output tokens of the winning attempt over the whole request wall time — failed attempts, rotation and backoff included), scaled x100 (int64 registry); refreshed at scrape, absent until the model served >=4 output tokens.", "model"),
		clientTTFT: reg.Gauge("onegw_client_tokens_to_first_byte_ms", "First-byte TTFT EWMA in ms per CLIENT model (handler entry -> first upstream byte; failed attempts, rotation and backoff all land in it); refreshed at scrape.", "model"),

		delivered: &deliveredTracker{},
	}
}

// success records one completed upstream attempt and its token accounting.
// Called exactly where usage.Observe runs so /metrics and /admin/usage agree.
// e2eMs/dtps carry the CLIENT view on successful rows (0 when the
// request had no delivery context): the winning attempt's tokens over the
// whole request wall, matched with the decode pair ms/tps above.
func (m *gatewayMetrics) success(provider, model, acct string, u types.Usage, savedTokens int64, ms int64, tps float64, e2eMs int64, dtps float64) {
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
	m.logReq(provider, model, acct, 200, "", u, savedTokens, "", ms, tps, e2eMs, dtps)
}

// logReq routes one completion into the #19 ring; nil-safe because tests
// build gatewayMetrics without a Server. errMsg carries the upstream
// explanation (upstream body / failure text) so the console log answers
// "why" and not just "what" — it is diagnostic payload, not a secret:
// it can contain model names, request ids, and upstream error prose.
// ms/tps are the decode phase's duration and tokens/sec (0 when unknown).
func (m *gatewayMetrics) logReq(provider, model, acct string, code int, kind string, u types.Usage, saved int64, errMsg string, ms int64, tps float64, e2eMs int64, dtps float64) {
	if m.srv != nil {
		m.srv.observeLog(provider, model, acct, code, kind, u, saved, truncErr(errMsg), ms, tps, e2eMs, dtps)
	}
}

// truncErr caps the stored error text: upstream bodies are read up to
// 1 MiB and the ring/SSE payload must stay small. Cut on a rune boundary.
func truncErr(s string) string {
	const max = 300
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// acctName nil-safely renders the account name for log rows: entries from
// paths that never picked an account (or hand-built CallResults in tests)
// carry an empty account.
func acctName(a *provider.Account) string {
	if a == nil {
		return ""
	}
	return a.Name
}

// upstreamErr records one failed upstream attempt. The error object drives
// the row: status from herr.Status, kind from herr.Type — so the console
// log distinguishes upstream_unreachable / upstream_timeout / the
// upstream's own error type instead of lumping every failure as
// upstream_error — and the message rides as the err field. The Prometheus
// label stays the coarse "upstream_error" bucket (label contract).
func (m *gatewayMetrics) upstreamErr(provider, model, acct string, herr *types.APIError) {
	status := 502
	kind := "upstream_error"
	msg := ""
	if herr != nil {
		if herr.Status != 0 {
			status = herr.Status
		}
		if herr.Type != "" {
			kind = herr.Type
		}
		msg = herr.Message
	}
	m.requests.Inc(provider, model, strconv.Itoa(status))
	m.errors.Inc(provider, "upstream_error")
	m.logReq(provider, model, acct, status, kind, types.Usage{}, 0, msg, 0, 0, 0, 0)
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

// noRoute records a request the router could not resolve (unknown provider /
// model, missing model field). The model label is the fixed "unresolved"
// placeholder: the client string never resolved to a route, and series are
// never evicted, so raw client strings must not become label values.
func (m *gatewayMetrics) noRoute(status int, errMsg string) {
	m.requests.Inc("", "unresolved", strconv.Itoa(status))
	m.errors.Inc("", "no_route")
	m.logReq("", "", "", status, "no_route", types.Usage{}, 0, errMsg, 0, 0, 0, 0)
}

// saturated records a request rejected because the buffered-memory budget
// could not be acquired.
func (m *gatewayMetrics) saturated() {
	m.requests.Inc("", "", "503")
	m.errors.Inc("", "budget_saturated")
	m.logReq("", "", "", 503, "budget_saturated", types.Usage{}, 0, "", 0, 0, 0, 0)
}

// tooLarge records a request rejected because its body exceeded the body cap.
func (m *gatewayMetrics) tooLarge() {
	m.requests.Inc("", "", "413")
	m.logReq("", "", "", 413, "no_route", types.Usage{}, 0, "", 0, 0, 0, 0)
}

// invalidBody records an attempt aborted before the upstream call because
// the request body could not be prepared (translation/model rewrite failed).
// It is a client-side 400, not one of the three error kinds.
func (m *gatewayMetrics) invalidBody(provider, model, acct, errMsg string) {
	m.requests.Inc(provider, model, "400")
	m.logReq(provider, model, acct, 400, "", types.Usage{}, 0, errMsg, 0, 0, 0, 0)
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
	if st := s.cur(); st != nil && st.pool != nil && m.provTPS != nil {
		for _, name := range st.pool.Names() {
			if d, ok := st.pool.Get(name); ok && !d.Disabled {
				m.provTPS.Set(int64(d.ProviderTPS()*100), name)
			}
		}
	}
	if m.delivered != nil {
		for _, r := range m.delivered.rows() {
			m.clientTPS.Set(int64(r.TPS*100), r.Model)
			m.clientTTFT.Set(int64(r.TTFTMs), r.Model)
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(m.reg.Render()))
}
