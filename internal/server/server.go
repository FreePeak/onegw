// Package server exposes the gateway HTTP surfaces: OpenAI, Anthropic, and
// Gemini compatibility endpoints plus admin/dashboard.
package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"onegw/internal/config"
	"onegw/internal/idempotency"
	"onegw/internal/oauth"
	"onegw/internal/owner"
	"onegw/internal/provider"
	"onegw/internal/quota"
	"onegw/internal/ratelimit"
	"onegw/internal/router"
	"onegw/internal/saver"
	"onegw/internal/server/dashboard"
	"onegw/internal/store"
	"onegw/internal/subquota"
	"onegw/internal/translat"
	"onegw/internal/types"
	"onegw/internal/update"
	"onegw/internal/usage"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// state bundles everything a hot reload swaps as one atomic snapshot. A
// request loads it once and runs against it; in-flight requests finish on
// the old snapshot.
type state struct {
	cfg    *config.Config
	pool   *provider.Pool
	router *router.Router
	saver  *saver.Saver
	usage  *usage.Tracker
	quota  *quota.Tracker
	subq   *subquota.Tracker
	budget *ByteBudget
	// ido is the idempotency dedup cache; nil (idempotency_ttl "0"/"off")
	// disables the feature entirely.
	ido *idempotency.Cache
}

// Server wires the gateway together.
type Server struct {
	st     *store.Store
	nodeID string
	start  time.Time
	oauth  *oauth.Manager // device-flow token manager; nil-safe after Close

	state    atomic.Pointer[state]
	inflight atomic.Int64 // requests currently live in the gateway pipeline
	// rl holds per-key rate-limit windows. It lives on the Server, not the
	// reloadable state, so SIGHUP does not reset in-progress windows.
	rl *ratelimit.Limiter
	m  *gatewayMetrics
	// cfgPath is the on-disk TOML file the process started from (set once
	// by main); it powers the admin config endpoints (masked view, reload,
	// keys/aliases PATCH).
	cfgPath atomic.Pointer[string]
	// dataDir is fixed at process start (the store's directory, or the
	// "memory" sentinel); owner.json is written here.
	dataDir string
	// owner records the running process (pid, build stamp, config mtime)
	// for /admin/health and <data_dir>/owner.json (#42).
	owner atomic.Pointer[owner.Info]
	// upd is the dashboard-visible update service (#61), wired once by
	// main via SetUpdater. Nil-safe: the endpoints answer 409 without it.
	upd atomic.Pointer[*update.Service]
	// cfgReloadHook is the wiring-layer callback fired after every
	// successful config swap (see SetOnConfigReload, #63).
	cfgReloadHook atomic.Pointer[func(*config.Config)]
	// sessions/logins power the dashboard cookie login (#45); events is
	// the bounded SSE fan-out hub; reqlog is the #19 request ring. All
	// live on the Server (not the reloadable state) so reloads neither
	// drop sessions nor lose the log history.
	sessions *adminSessions
	logins   *loginGuard
	events   *sseHub
	reqlog   *requestLog
	// retainStop closes the daily rollup-prune loop on Close.
	retainStop chan struct{}
	// cfgMu serializes admin config mutations so concurrent PATCH/reload
	// read-modify-write cycles on the TOML file stay atomic.
	cfgMu sync.Mutex
}

// cur returns the active state snapshot (non-nil once New has run).
func (s *Server) cur() *state { return s.state.Load() }

// New builds the server from config. The store and its data dir are fixed
// at process start — moving data_dir needs a restart; everything else is
// swappable via apply/Reload.
func New(cfg *config.Config) (*Server, error) {
	var st *store.Store
	dataDir := cfg.Server.DataDir
	if dataDir != "" && dataDir != "memory" {
		var err error
		st, err = store.Open(dataDir + "/usage.db")
		if err != nil {
			return nil, fmt.Errorf("open store: %w", err)
		}
	}
	s := &Server{st: st, nodeID: nodeID(dataDir), start: time.Now(), rl: ratelimit.New(), dataDir: dataDir,
		sessions: newAdminSessions(), logins: newLoginGuard(), events: newSSEHub(), reqlog: newRequestLog()}
	s.m = newGatewayMetrics()
	s.m.srv = s
	s.reqlog.next = s.events
	if st != nil {
		st.SetNodeID(s.nodeID)
	}
	s.initOAuth(cfg)
	s.startRetention()
	if err := s.apply(cfg, true); err != nil {
		return nil, err
	}
	return s, nil
}

// apply (re)builds the mutable parts of the server from cfg and swaps them
// in as one atomic snapshot: provider pool, router tables, saver, auth
// keys, admin password, buffered-memory budget, and the usage flush
// interval.

// loopbackListen reports whether addr binds only loopback interfaces.
// An empty host (":port"), "0.0.0.0", or "::" is NOT loopback.
func loopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hasKey reports whether any usable client auth key is configured.
func hasKey(keys []config.AuthKey) bool {
	for _, k := range keys {
		if strings.TrimSpace(k.Key) != "" {
			return true
		}
	}
	return false
}

// apply (re)builds the mutable parts of the server from cfg and swaps them
// in as one atomic snapshot.
func (s *Server) apply(cfg *config.Config, initial bool) error {
	// Fail closed: a non-loopback listener with no auth keys is an open
	// proxy over every upstream account quota. Returning before the atomic
	// swap keeps the previous config live on reload; on startup it refuses
	// to start.
	if !loopbackListen(cfg.Server.Listen) && !hasKey(cfg.Auth.KeyList) {
		return fmt.Errorf("refusing to serve %q with no auth keys — set [auth] keys or bind a loopback address", cfg.Server.Listen)
	}
	pool := provider.NewPool()
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		kind := provider.Kind(p.Kind)
		stickyTTL, _ := time.ParseDuration(p.Sticky) // "" stays 0; Validate rejects unparseable values
		def := &provider.Def{
			Name:             p.Name,
			Kind:             kind,
			BaseURL:          p.BaseURL,
			MaxConc:          p.MaxConc,
			RPM:              p.RPM,
			Disabled:         p.Disabled,
			ExtraHeaders:     p.ExtraHeader,
			Models:           p.Models,
			AlwaysThinking:   p.AlwaysThinking,
			NoThinking:       p.NoThinking,
			CacheProfile:     p.CacheProfile,
			Passthrough:      p.Passthrough,
			SearchMaxResults: p.MaxResults,
			SearchTimeout:    provider.ParseSearchTimeout(p.Timeout),
			StickyTTL:        stickyTTL,
			SessionHeader:    p.SessionHeader,
			HeaderTimeout:    cfg.ResponseHeaderTimeoutDur(),
		}
		// Task-routing model metadata (issue #54): per-model power and
		// capability declarations from [[providers.tier]] tables.
		for _, tier := range p.Tiers {
			def.Tiers = append(def.Tiers, provider.ModelTier{
				Model: tier.Model, Power: tier.Power, Vision: tier.Vision,
				Reasoning: tier.Reasoning, Context: tier.Context, MaxOut: tier.MaxOut,
			})
		}
		if len(p.Accounts) > 0 {
			for _, a := range p.Accounts {
				def.Accounts = append(def.Accounts, provider.Account{
					Name: a.Name, APIKey: a.APIKey, BaseURL: a.BaseURL, Weight: a.Weight, RPM: a.RPM,
				})
			}
		} else {
			def.Accounts = []provider.Account{{Name: "default", APIKey: p.APIKey, BaseURL: p.BaseURL}}
		}
		s.wireOAuthTokens(cfg, def)
		pool.Set(def)
		// Materialize the kind's default catalog onto the config copy so
		// routing AND every surface that reads cfg.Providers (models list,
		// dashboard) agree.
		if len(p.Models) == 0 {
			p.Models = provider.DefaultModels(kind)
		}
	}
	rt := router.New(pool)
	// Quota semantics own the cooling-pool answer: when the pool is empty
	// because the provider's quota window is exhausted, answer the same
	// 503 the attempt() gate would have produced (the gate lives inside
	// the Caller, which Execute skips when it never picks an account).
	// Genuine 429-limits keep the default 429 + Retry-After fast-fail.
	rt.PoolEmptyError = func(def *provider.Def, ready time.Time) *types.APIError {
		return s.poolEmptyError(def, ready)
	}
	// Advertised models must reach the router as ONE SetModels call:
	// SetModels REPLACES the direct-route table wholesale, so the historical
	// per-provider loop let the last provider's table clobber every earlier
	// one — and its bare (slash-less) ids were all dropped by the route
	// parse, leaving the live table EMPTY (2026-09-10: tokenrouter 504 rows
	// logged model "unresolved" because the advertised-scan had nothing to
	// find; the 2026-09-09 commandcode label-honesty fix was wired out of
	// existence the same way). Qualify each advertised id with its provider
	// name — the table's route format — before the single call.
	routes := make([]string, 0, 64)
	for _, p := range cfg.Providers {
		for _, m := range p.Models {
			routes = append(routes, p.Name+"/"+m)
		}
	}
	rt.SetModels(routes)
	var combos []*router.Combo
	for _, c := range cfg.Combos {
		targets := make([]router.Target, 0, len(c.Targets))
		for _, t := range c.Targets {
			prov, model, _ := strings.Cut(t, "/")
			targets = append(targets, router.Target{Provider: prov, Model: model})
		}
		combos = append(combos, &router.Combo{Name: c.Name, Targets: targets, Strategy: c.Strategy})
	}
	rt.SetCombos(combos)
	rt.SetTaskRouting(cfg.TaskRoutingOn())
	if cfg.TaskRoutingOn() {
		// #19 ring: one decision row per request whose combo order the
		// classifier changed (model field = client model, kind =
		// "task_routing", err = decision detail).
		rt.TaskLog = func(model, detail string) {
			s.observeLog("", model, "", 0, "task_routing", types.Usage{}, 0, detail, 0, 0, 0, 0)
		}
	}
	rt.SetAliases(cfg.Aliases)

	var sink usage.Sink
	if s.st != nil {
		sink = s.st
	}
	pusher := usage.NewPusher(cfg.Usage.ExportURL, cfg.Usage.ExportPassword, s.nodeID)
	usageTracker := usage.New(sink, cfg.FlushEvery(), pusher)

	// Quota windows (issue #7): per-provider limits from config; counters
	// seed from the store and inherit live state across hot reloads.
	limits := make(map[string]quota.Limits, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if p.QuotaWindow == "" {
			continue
		}
		l := quota.Limits{Window: p.QuotaWindow, LimitTokens: p.QuotaLimitTokens, LimitRequests: p.QuotaLimitRequests}
		if p.QuotaResetAnchor != "" {
			if a, err := time.Parse(time.RFC3339, p.QuotaResetAnchor); err == nil {
				l.Anchor = a
			}
		}
		limits[p.Name] = l
	}
	var quotaTracker *quota.Tracker
	if len(limits) > 0 {
		quotaTracker = quota.New(limits, s.st, cfg.FlushEvery())
		quotaTracker.Inherit(oldStateQuota(s.state.Load()))
	}

	// Subscription quota (issue #79): upstream-reported vendor windows for
	// opt-in providers (subscription_quota); exhausted accounts park until
	// the vendor's reset so combos fall through. Snapshots inherit across
	// hot reloads like the local quota windows.
	var subTracker *subquota.Tracker
	if targets := subTargets(cfg); len(targets) > 0 {
		subTracker = subquota.New(targets, s.parkExhaustedSubscription, s.liveSubKey)
		subTracker.Inherit(oldStateSub(s.state.Load()))
	}

	old := s.state.Load()
	s.state.Store(&state{
		cfg:    cfg,
		pool:   pool,
		router: rt,
		saver:  saver.New(saverConfigFrom(&cfg.Saver)),
		usage:  usageTracker,
		quota:  quotaTracker,
		subq:   subTracker,
		budget: NewByteBudget(cfg.Server.BufferCap),
		// Fresh LRU per load: SIGHUP drops at most one TTL window of
		// replay state, which is acceptable at the default 5s.
		ido: idempotency.New(cfg.Server.IdempotencyCache, cfg.IdempotencyTTLDur()),
	})
	if !initial && old != nil {
		old.usage.Stop() // flushes remaining data to the store, then ends the loop
		if old.quota != nil {
			old.quota.Stop()
		}
		if old.subq != nil {
			old.subq.Stop()
		}
	}
	s.syncOAuth(cfg)
	return nil
}

// oldStateQuota returns the previous snapshot's quota tracker, or nil.
func oldStateQuota(old *state) *quota.Tracker {
	if old == nil {
		return nil
	}
	return old.quota
}

// oldStateSub returns the previous snapshot's subscription tracker, or nil.
func oldStateSub(old *state) *subquota.Tracker {
	if old == nil {
		return nil
	}
	return old.subq
}

// Reload hot-swaps configuration (SIGHUP, or PUT /admin/config/reload).
// Bad config is rejected by the caller (config.Load) so this always
// applies a valid one.
func (s *Server) Reload(cfg *config.Config) {
	if err := s.apply(cfg, false); err != nil {
		log.Printf("onegw reload rejected: %v", err)
		return
	}
	// A successful reload is the "config changed underneath you" event:
	// re-stamp the ownership record so config_mtime reflects it (#42).
	s.StampOwner()
	s.fireOnConfigReload(cfg) // outer-mux /admin/update sync (#63)
}

// StampOwner records the running process (pid, build stamp, listen,
// config path + mtime, start time, argv) in <data_dir>/owner.json and in
// memory for /admin/health. Called once after startup and after every
// successful reload; a stale owner.json from a crashed predecessor is
// deliberate evidence, so exit does not remove it (#42).
func (s *Server) StampOwner() {
	st := s.cur()
	if st == nil {
		return
	}
	info := owner.Capture(st.cfg.Server.Listen, s.configPath(), s.start)
	s.owner.Store(&info)
	if err := owner.Write(s.dataDir, info); err != nil {
		log.Printf("onegw owner stamp: %v", err)
	}
}

// Close releases resources.
func (s *Server) Close() {
	if s.retainStop != nil {
		close(s.retainStop)
	}
	if s.events != nil {
		s.events.shutdown()
	}
	if s.oauth != nil {
		s.oauth.Stop()
	}
	if st := s.cur(); st != nil {
		st.usage.Stop()
		if st.quota != nil {
			st.quota.Stop()
		}
		if st.subq != nil {
			st.subq.Stop()
		}
	}
	if s.st != nil {
		s.st.Close()
	}
}

// Handler builds the HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/config", s.handleAdminConfigGet)
	mux.HandleFunc("PUT /admin/config/reload", s.handleAdminConfigReload)
	mux.HandleFunc("PATCH /admin/config/keys", s.handleAdminKeys)
	mux.HandleFunc("PATCH /admin/config/aliases", s.handleAdminAliases)
	mux.HandleFunc("PUT /admin/config/password", s.handleAdminPassword)
	mux.HandleFunc("PUT /admin/config/providers", s.handleAdminProviderEdit)
	mux.HandleFunc("PATCH /admin/config/providers/{name}/disabled", s.handleAdminProviderDisabled)
	mux.HandleFunc("PUT /admin/config/combos", s.handleAdminComboEdit)
	mux.HandleFunc("POST /v1/chat/completions", s.withIdempotency(translat.FmtOpenAI, s.handleOpenAI))
	mux.HandleFunc("POST /v1/completions", s.withIdempotency(translat.FmtOpenAI, s.handleOpenAI))
	mux.HandleFunc("POST /v1/messages", s.withIdempotency(translat.FmtAnthropic, s.handleAnthropic))
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /admin/usage/export", s.handleUsageExport)
	mux.HandleFunc("POST /admin/usage/import", s.handleUsageImport)
	mux.HandleFunc("POST /anthropic/v1/messages", s.withIdempotency(translat.FmtAnthropic, s.handleAnthropic))
	mux.HandleFunc("POST /v1beta/models/", s.handleGemini)
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) { s.handlePassthrough(w, r, surfEmbeddings) })
	mux.HandleFunc("POST /v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) { s.handlePassthrough(w, r, surfTranscriptions) })
	mux.HandleFunc("POST /v1/audio/speech", func(w http.ResponseWriter, r *http.Request) { s.handlePassthrough(w, r, surfSpeech) })
	mux.HandleFunc("GET /admin/health", s.handleHealth)
	mux.HandleFunc("GET /admin/usage", s.handleAdminUsage)
	mux.HandleFunc("GET /admin/quota", s.handleAdminQuota)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("POST /admin/login", s.handleAdminLogin)
	mux.HandleFunc("GET /admin", s.handleAdminPage)
	mux.HandleFunc("POST /admin/logout", s.handleAdminLogout)
	mux.HandleFunc("GET /admin/events", s.handleEvents)
	mux.HandleFunc("GET /admin/api/v1/logs", s.handleAPILogs)
	mux.HandleFunc("GET /admin/api/v1/usage/daily", s.handleAPIUsageDaily)
	mux.HandleFunc("GET /admin/api/v1/providers", s.handleAPIProviders)
	mux.HandleFunc("GET /admin/api/v1/combos", s.handleAPICombos)
	mux.HandleFunc("GET /admin/api/v1/quota", s.handleAPIQuota)
	mux.HandleFunc("GET /admin/api/v1/subscription", s.handleAPISubscription)
	mux.HandleFunc("GET /admin/api/v1/saver", s.handleAPISaver)
	mux.HandleFunc("GET /admin/api/v1/update", s.handleUpdateStatus)
	mux.HandleFunc("POST /admin/api/v1/update", s.handleUpdateApply)
	mux.HandleFunc("GET /admin/assets/fonts/", s.handleAdminFont)
	mux.HandleFunc("GET /admin/ui/", s.handleAdminUI)
	mux.HandleFunc("GET /", s.handleDashboard)
	return s.withRecovery(mux)
}

// ---------------------------------------------------------------------------
// Surfaces
// ---------------------------------------------------------------------------

func (s *Server) handleOpenAI(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, translat.FmtOpenAI)
}

func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, translat.FmtAnthropic)
}

func (s *Server) handleGemini(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
	model, method, ok := strings.Cut(rest, ":")
	if !ok || model == "" || r.Method != http.MethodPost {
		writeErr(w, translat.FmtGemini, errAPI(400, "invalid_request", "expected POST /v1beta/models/{model}:generateContent"))
		return
	}
	isStream := method == "streamGenerateContent"
	if method != "generateContent" && !isStream {
		writeErr(w, translat.FmtGemini, errAPI(404, "not_found", "unknown method "+method))
		return
	}
	if isStream && r.URL.Query().Get("alt") != "sse" {
		q := r.URL.Query()
		q.Set("alt", "sse")
		r.URL.RawQuery = q.Encode()
	}
	s.proxyGemini(w, r, model, isStream)
}

// proxyGemini mirrors proxy() for the Gemini surface.
func (s *Server) proxyGemini(w http.ResponseWriter, r *http.Request, model string, stream bool) {
	ak, ok := s.authorize(w, r)
	if !ok {
		return
	}
	d := &delivery{start: time.Now(), model: s.boundedModel(model)}
	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	release, ok := s.acquireForBody(r)
	if !ok {
		s.m.saturated()
		s.rejectSaturated(w, translat.FmtGemini)
		return
	}
	defer release()

	body, err := s.readBody(r)
	if err != nil {
		s.m.tooLarge()
		writeErr(w, translat.FmtGemini, errAPI(413, "body_too_large", err.Error()))
		return
	}
	if !s.enforceRateLimits(w, translat.FmtGemini, ak) {
		return
	}
	st := s.cur()
	var savedTokens int64
	if st.cfg.Saver.Enabled {
		body, savedTokens = st.saver.ApplyRaw(translat.FmtGemini, body)
	}
	body, oerr := s.applyOutputSavers(r.Context(), st, translat.FmtGemini, body, model)
	if oerr != nil {
		writeErr(w, translat.FmtGemini, errAPI(502, "compress_failed", oerr.Error()))
		return
	}
	res, rerr := st.router.Resolve(model)
	if rerr != nil {
		s.m.noRoute(rerr.Status, rerr.Message)
		writeErr(w, translat.FmtGemini, rerr)
		return
	}
	if !s.enforceAllowlist(w, translat.FmtGemini, ak, model, res) {
		return
	}
	execCtx := withDelivery(router.WithIdentity(r.Context(), requestIdentity(r.Header, ak)), d)
	if st.cfg.TaskRoutingOn() {
		execCtx = router.WithTask(execCtx, router.CollectSignals(body))
	}
	attempts := 0
	execErr := st.router.Execute(execCtx, res, func(ctx context.Context, def *provider.Def, acct *provider.Account, m string) (any, *types.APIError) {
		attempts++
		return s.attempt(ctx, def, acct, m, translat.FmtGemini, body, stream, w, savedTokens, r.Header, ak, attempts)
	}, func(v any) {})
	if execErr != nil && w.Header().Get("Content-Type") == "" {
		writeErr(w, translat.FmtGemini, execErr)
	}
}

// proxy is the main pipeline for OpenAI/Anthropic surfaces.
//
// Memory contract: buffered work (body read, saver, unified decode, any
// non-streaming translation) runs under the global byte budget; streaming
// passthrough runs outside it. Saturation returns 503 + Retry-After in the
// client's wire format.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, clientFmt translat.Format) {
	ak, ok := s.authorize(w, r)
	if !ok {
		return
	}
	d := &delivery{start: time.Now()}

	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	// Opt-in streaming passthrough: relay same-format bodies without a
	// full read. proxyStream falls back (false) to the buffered pipeline
	// below with the body intact whenever it is not eligible.
	if st := s.cur(); st.cfg.Server.StreamRequests && !st.cfg.Saver.Enabled {
		if s.proxyStream(w, r, clientFmt, ak) {
			return
		}
	}
	release, ok := s.acquireForBody(r)
	if !ok {
		s.m.saturated()
		s.rejectSaturated(w, clientFmt)
		return
	}
	defer release()

	body, err := s.readBody(r)
	if err != nil {
		s.m.tooLarge()
		writeErr(w, clientFmt, errAPI(413, "body_too_large", err.Error()))
		return
	}
	model := peekModel(body)
	stream := peekStream(body)
	d.model = s.boundedModel(model)
	if !s.enforceRateLimits(w, clientFmt, ak) {
		return
	}

	st := s.cur()
	var savedTokens int64
	if st.cfg.Saver.Enabled {
		body, savedTokens = st.saver.ApplyRaw(clientFmt, body)
	}
	body, oerr := s.applyOutputSavers(r.Context(), st, clientFmt, body, model)
	if oerr != nil {
		writeErr(w, clientFmt, errAPI(502, "compress_failed", oerr.Error()))
		return
	}

	res, rerr := st.router.Resolve(model)
	if rerr != nil {
		s.m.noRoute(rerr.Status, rerr.Message)
		writeErr(w, clientFmt, rerr)
		return
	}
	if !s.enforceAllowlist(w, clientFmt, ak, model, res) {
		return
	}
	execCtx := withDelivery(router.WithIdentity(r.Context(), requestIdentity(r.Header, ak)), d)
	if st.cfg.TaskRoutingOn() {
		execCtx = router.WithTask(execCtx, router.CollectSignals(body))
	}
	attempts := 0
	execErr := st.router.Execute(execCtx, res, func(ctx context.Context, def *provider.Def, acct *provider.Account, m string) (any, *types.APIError) {
		attempts++
		return s.attempt(ctx, def, acct, m, clientFmt, body, stream, w, savedTokens, r.Header, ak, attempts)
	}, func(v any) {})
	if execErr != nil && w.Header().Get("Content-Type") == "" {
		writeErr(w, clientFmt, execErr)
	}
}

// proxyGemini mirrors proxy() for the Gemini surface.
// acquireForBody reserves budget for the request's declared content length
// (plus margin for decoded maps and response handling). Streaming requests
// also reserve: their request bodies are read fully here. Returns a release
// func; ok=false means the gateway is saturated.
func (s *Server) acquireForBody(r *http.Request) (func(), bool) {
	n := r.ContentLength
	if n < 0 {
		n = 0
	}
	// Margin covers the request pipeline's transient amplification: decoded
	// map (~2x), re-marshaled body (~1x), upstream write copy (~1x).
	if n > 0 {
		n += 3*n + 64<<10
	}
	st := s.cur()
	if err := st.budget.Acquire(r.Context(), n); err != nil {
		st.budget.Saturated()
		return nil, false
	}
	return func() { st.budget.Release(n) }, true
}

// rejectSaturated answers 503 with Retry-After in the client's format.
func (s *Server) rejectSaturated(w http.ResponseWriter, f translat.Format) {
	w.Header().Set("Retry-After", "2")
	writeErr(w, f, errAPI(503, "gateway_saturated", "onegw at buffered-memory capacity; retry shortly"))
}

// poolEmptyError answers a request whose target provider's whole account
// pool is cooling. When the cooldown comes from quota-window exhaustion
// (the pool was cooled by the attempt() gate), the answer must be the same
// 503 provider_quota_exhausted the gate itself would return — combo
// fall-through and Retry-After semantics depend on it. Any other cooling
// (upstream 429s) falls back to the router's default 429 + Retry-After.
func (s *Server) poolEmptyError(def *provider.Def, ready time.Time) *types.APIError {
	if q := s.cur().quota; q != nil {
		if st, ok := q.Status(def.Name, time.Now()); ok && st.Exhausted {
			cool := time.Until(st.WindowEnd)
			if cool < 0 {
				cool = 0
			}
			return &types.APIError{Status: 503, Type: "provider_quota_exhausted", Code: "quota_exceeded",
				RetryAfter: strconv.FormatInt(int64(cool.Seconds())+1, 10),
				Message: fmt.Sprintf("provider %s quota exhausted (%s window); resets %s",
					def.Name, st.Window, st.WindowEnd.UTC().Format(time.RFC3339))}
		}
	}
	return router.DefaultPoolEmptyError(def, ready)
}

// requestIdentity derives the sticky-account identity for a request: the
// client session header when present, else the auth key label. Empty
// disables affinity (plain round-robin). Takes the header map so
// headerless callers (attempt tests) can pass a bare http.Header.
func requestIdentity(h http.Header, ak *config.AuthKey) string {
	if sid := h.Get(provider.OpenCodeSessionHeader); sid != "" {
		return "s:" + sid
	}
	if ak != nil {
		return "k:" + ak.Label()
	}
	return ""
}

// attempt performs one upstream call and returns the response to the
// client. Streaming replies are piped/translated event-by-event; a
// non-streaming cross-format reply takes the documented buffered path
// (parse whole response, translate, answer JSON). Usage is recorded.
// r's session-affinity headers are forwarded upstream (issue #36); nil is
// allowed for headerless callers (tests).
func (s *Server) attempt(ctx context.Context, def *provider.Def, acct *provider.Account, model string,
	clientFmt translat.Format, body []byte, stream bool, w http.ResponseWriter, savedTokens int64,
	clientHdr http.Header, ak *config.AuthKey, attempts int) (any, *types.APIError) {
	// mdl is the metrics label only: raw client model strings must not
	// create unbounded series (routing already used the original string).
	mdl := s.boundedModel(model)
	upstreamFmt := def.UpstreamFormat(model)
	// Quota enforcement (issue #7): an exhausted provider cools its whole
	// account pool until the window ends and answers 503 (retryable, so
	// combos fall through to the next target). Checked before the upstream
	// call so a direct hit never reaches the provider. Retry-After rides
	// on the error (written only if this error actually reaches the
	// client), never on w — a fallen-through attempt must not leak it.
	if q := s.cur().quota; q != nil {
		if st, ok := q.Status(def.Name, time.Now()); ok && st.Exhausted {
			cool := time.Until(st.WindowEnd)
			if cool < 0 {
				cool = 0
			}
			for i := range def.Accounts {
				def.Cool(&def.Accounts[i], cool)
			}
			return nil, &types.APIError{Status: 503, Type: "provider_quota_exhausted", Code: "quota_exceeded",
				RetryAfter: strconv.FormatInt(int64(cool.Seconds())+1, 10),
				Message: fmt.Sprintf("provider %s quota exhausted (%s window); resets %s",
					def.Name, st.Window, st.WindowEnd.UTC().Format(time.RFC3339))}
		}
	}
	upBody, err := prepareUpstreamBody(upstreamFmt, clientFmt, body, model, def)
	if err != nil {
		s.m.invalidBody(def.Name, mdl, acctName(acct), err.Error())
		return nil, &types.APIError{Status: 400, Type: "invalid_request", Message: err.Error()}
	}
	// Issue #34: anchor cache markers LAST — after every body mutation
	// including cross-format translation — so anchors never sit at
	// pre-normalization offsets (a stale anchor costs a full prefix
	// rewrite). sessionKey is the same identity sticky-account pinning
	// uses; "" (no session header, no key label) skips sticky-key
	// injection. clientHdr may be nil (headerless tests).
	upBody = anchorCacheProfile(upBody, model, def, upstreamFmt, requestIdentity(clientHdr, ak))
	// X-OneGW-Decision rides the pre-body write: every pre-flight gate
	// above answers WITHOUT touching w, so the attempt that finally
	// commits headers is the one that stamps the header (combo fallback
	// therefore names the serving target).
	setDecisionHeader(w, def, acct, model, attempts)
	res, apiErr := def.Do(ctx, acct, model, clientHdr, bytes.NewReader(upBody), stream || def.Kind.ForcedStream())
	if apiErr != nil {
		s.m.upstreamErr(def.Name, mdl, acctName(acct), apiErr)
		if alwaysThinking400(apiErr) {
			// Runtime self-healing for providers whose config lacks the
			// always_thinking globs (a combo can mix models with different
			// thinking modes): remember the model, and let Execute retry
			// this target once — attempt now coerces upfront because
			// AlwaysThinkingModel consults learned state — then fall
			// through to the next combo target if it still refuses.
			if def.LearnAlwaysThinking(model) {
				log.Printf("server: learned always-thinking %s/%s from upstream 400; future requests coerce effort upfront", def.Name, model)
			}
			apiErr.Fallbackable = true
		} else if noThinkingConflict400(apiErr) {
			// Same medicine for the mirror failure (live kilocode 2026-09-11:
			// "reasoning_effort and reasoning.effort are both provided with
			// conflicting values" — the upstream duplicates the knob itself,
			// so ANY effort value is fatal): learn the model as no-thinking,
			// retry once with the knobs stripped, then fall through. Without
			// this mark the 400 is terminal and kills the whole combo chain.
			if def.LearnNoThinking(model) {
				log.Printf("server: learned no-thinking %s/%s from upstream conflict 400; future requests strip effort upfront", def.Name, model)
			}
			apiErr.Fallbackable = true
		}
		return nil, apiErr
	}
	return nil, s.relayResponse(w, res, def, model, clientFmt, upstreamFmt, stream, len(body), savedTokens, ak, ctx)
}

// speedFloor is the smallest decode window whose tokens/sec quotient is
// reportable: it mirrors provider.minStreamTime — buffered non-stream
// replies finish in sub-millisecond time, and tokens/0.0001s is not a
// speed anyone actually decoded at.
const speedFloor = 200 * time.Millisecond

// relayResponse delivers an upstream response to the client: the buffered
// cross-format path for non-streaming format mismatches, otherwise the
// sniffed/translated pipe. Usage is recorded, TPM and quota observed. It
// owns res.Resp.Body.
func (s *Server) relayResponse(w http.ResponseWriter, res *provider.CallResult, def *provider.Def, model string,
	clientFmt, upstreamFmt translat.Format, stream bool, reqBodyLen int, savedTokens int64, ak *config.AuthKey,
	ctx context.Context) *types.APIError {

	defer res.Resp.Body.Close()

	// CommandCode streams NDJSON and can report errors INSIDE a 200 body.
	// Inspect the head (bounded): an in-200 error event is answered as a
	// real HTTP error before any bytes reach the client.
	var head []byte
	if upstreamFmt == translat.FmtCommandCode {
		var herr *types.APIError
		var err error
		head, herr, err = translat.InspectCommandCodeHead(res.Resp.Body)
		if err != nil {
			herr := errAPI(502, "upstream_unreachable", err.Error())
			s.m.upstreamErr(def.Name, model, acctName(res.Acct), herr)
			return herr
		}
		if herr != nil {
			s.m.upstreamErr(def.Name, model, acctName(res.Acct), herr)
			return herr
		}
	}

	var rec types.Usage
	if !stream && def.Kind.ForcedStream() {
		// Stream-only upstream (commandcode, grok responses), non-streaming
		// client: aggregate the stream into one completion. Errors detected
		// during aggregation still get a real status (nothing was written).
		// OpenCode's responses kind is NOT here: its non-stream reply is a
		// plain JSON body handled by the buffered cross-format path below.
		var src io.Reader = res.Resp.Body
		if len(head) > 0 {
			src = io.MultiReader(bytes.NewReader(head), res.Resp.Body)
		}
		resp, aerr := translat.AggregateStream(src, upstreamFmt, model)
		if aerr != nil {
			var herr *types.APIError
			if apiErr, ok := aerr.(*types.APIError); ok {
				herr = apiErr
			} else {
				herr = errAPI(502, "stream_aggregate_failed", aerr.Error())
			}
			// Mid-stream/aggregate failures bypass provider.Do's
			// classification hooks: an in-stream 429/403 must still cool
			// the account or the next attempt re-picks it and repeats.
			if herr.OverQuota() && res.Acct != nil {
				def.RateLimited(res.Acct, herr.RateWindow()) // no header in-stream: stated window, else adaptive ladder
			}
			if herr.RegionLocked() && res.Acct != nil {
				def.Cool(res.Acct, 5*time.Minute)
			}
			s.m.upstreamErr(def.Name, model, acctName(res.Acct), herr)
			return herr
		}
		rb, merr := translat.EncodeResponse(clientFmt, resp)
		if merr != nil {
			herr := errAPI(501, "response_encode_failed", merr.Error())
			s.m.upstreamErr(def.Name, model, acctName(res.Acct), herr)
			return herr
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rb)
		rec = resp.Usage
	} else if !stream && upstreamFmt != clientFmt {
		// Buffered cross-format path (PRD "parse whole request, parse whole
		// response"). The reply bytes are accounted against the same global
		// byte semaphore as the request, capped at max_body_bytes; all
		// failures here are deterministic and non-retryable — a different
		// key returns the same oversized body, and retrying translate/encode
		// failures would only burn paid upstream calls.
		st := s.cur() // one snapshot: apply() swaps state atomically on SIGHUP
		maxResp := st.cfg.Server.MaxBody
		reserve := res.Resp.ContentLength
		if reserve < 0 || reserve > maxResp {
			reserve = maxResp
		}
		if err := st.budget.Acquire(ctx, reserve); err != nil {
			st.budget.Saturated()
			return errAPI(503, "gateway_saturated", "onegw at buffered-memory capacity; retry shortly")
		}
		defer st.budget.Release(reserve)
		raw, rerr := io.ReadAll(io.LimitReader(res.Resp.Body, maxResp+1))
		if rerr != nil {
			herr := errAPI(502, "upstream_read_failed", rerr.Error())
			s.m.upstreamErr(def.Name, model, acctName(res.Acct), herr)
			return herr
		}
		if int64(len(raw)) > maxResp {
			return errAPI(413, "upstream_response_too_large", "response exceeds max_body_bytes")
		}
		cr, derr := translat.DecodeResponse(upstreamFmt, raw)
		if derr != nil {
			herr := errAPI(501, "response_translate_failed", derr.Error())
			s.m.upstreamErr(def.Name, model, acctName(res.Acct), herr)
			return herr
		}
		out, eerr := translat.EncodeResponse(clientFmt, cr)
		if eerr != nil {
			herr := errAPI(501, "response_encode_failed", eerr.Error())
			s.m.upstreamErr(def.Name, model, acctName(res.Acct), herr)
			return herr
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		rec = cr.Usage
	} else {
		h := w.Header()
		if stream {
			h.Set("Content-Type", "text/event-stream; charset=utf-8")
			h.Set("Cache-Control", "no-cache")
			h.Set("Connection", "keep-alive")
			h.Set("X-Accel-Buffering", "no")
		} else {
			h.Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flush := func() {
			if flusher != nil {
				flusher.Flush()
			}
		}
		flush()

		var src io.Reader = res.Resp.Body
		if len(head) > 0 {
			// Re-attach the inspected head so no events are lost.
			src = io.MultiReader(bytes.NewReader(head), res.Resp.Body)
		}
		if upstreamFmt == clientFmt {
			sn := usage.NewSniffer(src, 0)
			_, _ = io.Copy(w, sn)
			flush()
			in, out, cr, cw, rs, seen := sn.Usage()
			if seen {
				rec = types.Usage{InputTokens: in, OutputTokens: out, CacheReadTokens: cr, CacheWriteTokens: cw, ReasoningTokens: rs}
			}
		} else {
			u, terr := translat.TranslateStream(src, w, flush, upstreamFmt, clientFmt, model)
			if terr != nil {
				var herr *types.APIError
				if apiErr, ok := terr.(*types.APIError); ok {
					herr = apiErr
				} else {
					herr = errAPI(502, "stream_translate_failed", terr.Error())
				}
				// Mid-stream failures bypass provider.Do's classification
				// hooks: cool on in-stream 429/403 like the aggregate path.
				if herr.OverQuota() && res.Acct != nil {
					def.RateLimited(res.Acct, herr.RateWindow()) // no header in-stream: stated window, else adaptive ladder
				}
				if herr.RegionLocked() && res.Acct != nil {
					def.Cool(res.Acct, 5*time.Minute)
				}
				s.m.upstreamErr(def.Name, model, acctName(res.Acct), herr)
				return herr
			}
			rec = u
		}
	}
	rec.UpstreamFormat = string(upstreamFmt)
	if rec.InputTokens == 0 && rec.OutputTokens == 0 {
		rec.Estimated = true
		rec.InputTokens = int64(reqBodyLen) / 4
	}
	// Decode speed: from upstream response headers to relay end — the
	// streaming phase proper, prefill excluded. Feeds the per-model /
	// per-account EWMA (combo + account steering) and the #19 ring's
	// tokens/sec column. Synthetic CallResults (search, cursor) carry no
	// FirstByte and are skipped.
	ms, tps := int64(0), 0.0
	if !res.FirstByte.IsZero() && rec.OutputTokens > 0 {
		d := time.Since(res.FirstByte)
		def.ObserveSpeed(res.Acct, model, rec.OutputTokens, d)
		// Same noise floor as the EWMA (provider speed.go): a buffered
		// non-stream reply lands in sub-millisecond time, and
		// tokens/0.0001s is not a speed anyone decoded at.
		if d >= speedFloor {
			ms = d.Milliseconds()
			tps = float64(rec.OutputTokens) / d.Seconds()
		}
	}
	label := ""
	if ak != nil {
		label = ak.Label()
	}
	s.cur().usage.Observe(usage.Key{Provider: def.Name, Model: model, APIKey: label}, rec, savedTokens)
	s.observeTPM(ak, rec.InputTokens+rec.OutputTokens)
	if q := s.cur().quota; q != nil {
		q.Observe(def.Name, rec.InputTokens+rec.OutputTokens+rec.ReasoningTokens, 1, time.Now())
	}
	// Client-experienced delivery: the winning attempt's output tokens
	// over the WHOLE request wall (handler entry -> now), failed attempts,
	// account rotations and backoff included — the throughput the client
	// (omp) actually received. Folded into the per-client-model EWMA and
	// stamped on the ring row alongside the decode pair.
	e2eMs, dtps := int64(0), 0.0
	if d := deliveryFrom(ctx); d != nil && rec.OutputTokens > 0 {
		wall := time.Since(d.start)
		ttft := res.FirstByte.Sub(d.start)
		if ttft < 0 {
			ttft = 0
		}
		if wall > 0 {
			e2eMs = wall.Milliseconds()
			dtps = float64(rec.OutputTokens) / wall.Seconds()
			s.m.delivered.observe(d.model, rec.OutputTokens, wall, ttft, time.Now())
		}
	}
	s.m.success(def.Name, model, acctName(res.Acct), rec, savedTokens, ms, tps, e2eMs, dtps)
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// authorize authenticates the request and returns the matched key policy.
// ok=false means the 401 response was written. ak==nil with ok==true is
// the open gateway (no keys configured).
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (*config.AuthKey, bool) {
	keys := s.cur().cfg.Auth.KeyList
	if len(keys) == 0 {
		return nil, true
	}
	auth := r.Header.Get("Authorization")
	key := strings.TrimPrefix(auth, "Bearer ")
	if key == "" {
		key = r.Header.Get("x-api-key")
	}
	if key == "" {
		key = r.Header.Get("x-goog-api-key") // native Gemini clients authenticate with this
	}
	for i := range keys {
		if keys[i].Key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(keys[i].Key)) == 1 {
			return &keys[i], true
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	return nil, false
}

func (s *Server) readBody(r *http.Request) ([]byte, error) {
	maxBody := s.cur().cfg.Server.MaxBody
	r.Body = http.MaxBytesReader(nil, r.Body, maxBody)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBody {
		return nil, fmt.Errorf("body exceeds %d bytes", maxBody)
	}
	return b, nil
}

func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic serving %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				if w.Header().Get("Content-Type") == "" {
					writeErr(w, translat.FmtOpenAI, errAPI(500, "internal", fmt.Sprintf("panic: %v", rec)))
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	var models []model
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		models = append(models, model{ID: id, Object: "model", OwnedBy: "onegw"})
	}
	cfg := s.cur().cfg
	for _, p := range cfg.Providers {
		if p.Disabled {
			continue // paused: not advertised, direct hits answer 503
		}
		if p.Kind == "searxng" {
			// Virtual search surface: any "<name>/<x>" model string
			// routes to it; advertise the canonical id so agent CLIs
			// can discover it via /v1/models.
			add(p.Name + "/query")
		}
		for _, m := range p.Models {
			add(p.Name + "/" + m)
		}
	}
	for _, c := range cfg.Combos {
		add(c.Name)
	}
	// Aliases surface as first-class model ids so clients can discover them.
	for a := range cfg.Aliases {
		add(a)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	out := map[string]any{
		"status":        "ok",
		"uptime_s":      int(time.Since(s.start).Seconds()),
		"inflight":      s.inflight.Load(),
		"heap_alloc_mb": m.HeapAlloc >> 20,
		"heap_sys_mb":   m.HeapSys >> 20,
		"sys_mb":        m.Sys >> 20,
		"num_gc":        m.NumGC,
	}
	// Ownership (#42): which process/build/config is canonical, straight
	// from memory — answers "who is running what" without process tables.
	if o := s.owner.Load(); o != nil {
		out["owner"] = o
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// adminOK reports whether the request may touch the admin surface: the
// X-Admin-Password header (constant-time), a live session cookie (#45),
// or an open gateway (no admin_password configured).
func (s *Server) adminOK(r *http.Request) bool {
	pw := s.cur().cfg.Server.AdminPassword
	if pw == "" {
		return true
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Password")), []byte(pw)) == 1 {
		return true
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return s.sessions.valid(c.Value, pw)
	}
	return false
}

// handleDashboard keeps "/" as a friendly entry point: redirect to the
// admin console. Unknown paths stay 404.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleAdminFont serves a vendored woff2 from the dashboard embed.
// Public static bytes (no session data) — the login page loads fonts
// before any cookie exists.
func (s *Server) handleAdminFont(w http.ResponseWriter, r *http.Request) {
	dashboard.ServeFont(w, strings.TrimPrefix(r.URL.Path, "/admin/assets/fonts/"))
}
func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// source=store reads persisted rollups over ?days=N (default 1);
	// default reads the live since-last-flush window. Totals are computed
	// from the same rows as the table so the header and table can never
	// disagree (tracker totals are process-lifetime, not a time window).
	if r.URL.Query().Get("source") == "store" && s.st != nil {
		days := 1
		if v := r.URL.Query().Get("days"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 366 {
				days = n
			}
		}
		from := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
		to := time.Now().UTC().Format("2006-01-02")
		rows, err := s.st.QueryRange(from, to)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"store query failed"}`))
			return
		}
		if rows == nil {
			rows = []store.UsageRow{}
		}
		var totReq, totIn, totOut, totSaved int64
		for _, r := range rows {
			totReq += r.Requests
			totIn += r.InputTok
			totOut += r.OutputTok
			totSaved += r.SavedTok
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"rows":   rows,
			"totals": map[string]any{"requests": totReq, "input": totIn, "output": totOut, "saved": totSaved},
		})
		return
	}
}

// prepareUpstreamBody returns the body to send upstream. Same format →
// verbatim (with the model field rewritten to the routed upstream model);
// different format → full translate via the unified model. def (may be nil
// in tests) carries thinking-knob adaptation for the routed model.
func prepareUpstreamBody(upstream, client translat.Format, body []byte, upstreamModel string, def *provider.Def) ([]byte, error) {
	if upstream == client {
		var err error
		body, err = rewriteModel(body, upstreamModel)
		if err != nil {
			return nil, err
		}
		body = adaptThinkingBody(body, upstreamModel, def)
		return normalizeRoles(body)
	}
	switch client {
	case translat.FmtOpenAI:
		u, err := translat.DecodeOpenAIRequest(body)
		if err != nil {
			return nil, err
		}
		u.Model = upstreamModel
		adaptThinkingUnified(u, upstreamModel, def)
		return encodeFor(upstream, u)
	case translat.FmtAnthropic:
		u, err := translat.DecodeAnthropicRequest(body)
		if err != nil {
			return nil, err
		}
		u.Model = upstreamModel
		adaptThinkingUnified(u, upstreamModel, def)
		return encodeFor(upstream, u)
	case translat.FmtGemini:
		u, err := translat.DecodeGeminiRequest(body)
		if err != nil {
			return nil, err
		}
		u.Model = upstreamModel // model arrives in the URL path on this surface
		adaptThinkingUnified(u, upstreamModel, def)
		return encodeFor(upstream, u)
	default:
		return rewriteModel(body, upstreamModel)
	}
}

// rewriteModel surgically replaces the top-level "model" string in a raw
// JSON body, preserving every other byte of structure (json.Number decode).
func rewriteModel(body []byte, model string) ([]byte, error) {
	if model == "" {
		return body, nil
	}
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return body, nil // not an object; forward verbatim
	}
	if cur, _ := root["model"].(string); cur == model || cur == "" {
		return body, nil
	}
	root["model"] = model
	out, err := json.Marshal(root)
	if err != nil {
		return body, nil
	}
	return out, nil
}

// normalizeRoles maps OpenAI "developer" role messages to "system" for
// upstreams that predate the role (B.AI and friends reject "developer").
// Some clients also refuse `store: false`; it is dropped when present.
// AI-SDK clients serialize assistant thinking as a plain "reasoning"
// string; DeepSeek-dialect thinking upstreams (which emit
// reasoning_content deltas) require it echoed back under that name —
// renamed when present, dropped when null (live 2026-09-10: tokenharbor
// deepseek 400 "The `reasoning_content` in the thinking mode must be
// passed back to the API"). Same medicine for the remaining vendor aliases
// (reasoning_text, the structured reasoning_details[] array pi replays for
// commandcode-served turns): converted only when reasoning_content is
// absent — the native echo already satisfies the contract.
func normalizeRoles(body []byte) ([]byte, error) {
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return body, nil
	}
	changed := false
	if msgs, ok := root["messages"].([]any); ok {
		for _, mv := range msgs {
			m, ok := mv.(map[string]any)
			if !ok {
				continue
			}
			if m["role"] == "developer" {
				m["role"] = "system"
				changed = true
			}
			if m["role"] == "assistant" {
				if v, ok := m["reasoning"]; ok {
					if s, isStr := v.(string); isStr && s != "" {
						if _, has := m["reasoning_content"]; !has {
							m["reasoning_content"] = s
						}
					}
					delete(m, "reasoning")
					changed = true
				}
				if v, ok := m["reasoning_text"]; ok {
					if s, isStr := v.(string); isStr && s != "" {
						if _, has := m["reasoning_content"]; !has {
							m["reasoning_content"] = s
						}
					}
					delete(m, "reasoning_text")
					changed = true
				}
				if v, ok := m["reasoning_details"]; ok {
					if s := flattenReasoningDetails(v); s != "" {
						if _, has := m["reasoning_content"]; !has {
							m["reasoning_content"] = s
							delete(m, "reasoning_details")
							changed = true
						}
					}
				}
			}
		}
	}
	if _, ok := root["store"]; ok {
		delete(root, "store")
		changed = true
	}
	if !changed {
		return body, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body, nil
	}
	return out, nil
}

// flattenReasoningDetails joins the readable entries of an AI-SDK-style
// reasoning_details array ([{type:"reasoning.text","text":"..."}, ...])
// into one echo string; "" when the array carries nothing usable. Field
// precedence text > content > summary mirrors translat.reasoningEcho.
func flattenReasoningDetails(v any) string {
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, d := range arr {
		m, ok := d.(map[string]any)
		if !ok {
			continue
		}
		t := ""
		for _, k := range []string{"text", "content", "summary"} {
			if s, isStr := m[k].(string); isStr && s != "" {
				t = s
				break
			}
		}
		if t != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(t)
		}
	}
	return sb.String()
}

// coerceEffort maps reasoning_effort values onto the enum an
// always-thinking upstream accepts (GLM, error 1210 family: only
// low|high|max). "none"/"minimal"/"medium" become "low"; "xhigh" (client
// ladders above high) becomes "max"; any unrecognized value falls back to
// "high" — always accepted, capability-preserving, and deterministic.
func coerceEffort(effort string) string {
	switch effort {
	case "none", "minimal", "medium":
		return "low"
	case "xhigh":
		return "max"
	case "low", "high", "max":
		return effort
	default:
		return "high"
	}
}

// alwaysThinking400 reports whether an upstream 400 is the GLM-family
// "this model always thinks" rejection. Seen shapes: error code 1210
// (Zhipu direct), code 400001 carrying the Chinese message (B.AI and
// other proxies pass it through), and English phrasings of the same
// "use low, high, or max" instruction. Kept deliberately narrow — a
// plain invalid-request 400 must not be classified as one.
func alwaysThinking400(e *types.APIError) bool {
	if e == nil || e.Status != 400 {
		return false
	}
	if e.Code == "1210" {
		return true
	}
	msg := e.Message
	switch {
	case strings.Contains(msg, "始终思考"), // "always thinks"
		strings.Contains(msg, "不支持关闭思考"), // "does not support disabling thinking"
		strings.Contains(msg, "low、high 或 max"),
		strings.Contains(msg, "low, high or max"),
		strings.Contains(msg, "low, high, or max"),
		// commandcode's zod enum rejection (live 2026-09-11): reasoning_effort
		// outside its accepted set answers `Invalid option: expected one of
		// "low"|"medium"|"high"|"xhigh"|"max"` — the same always-thinking
		// family (a none/minimal-capable dialect the model refuses): learn +
		// coerce, and stay Fallbackable so combos never die on it.
		strings.Contains(msg, `expected one of "low"`):
		return true
	}
	return false
}

// noThinkingConflict400 reports whether an upstream 400 is the "any
// reasoning knob is fatal" rejection (live kilocode 2026-09-11: kilo's
// openrouter gateway duplicates reasoning_effort into reasoning.effort
// and rejects ANY value with "conflicting values" — the resolved free
// rotation carries no thinking mode at all). Same shape family as
// alwaysThinking400: deliberately narrow, status-gated.
func noThinkingConflict400(e *types.APIError) bool {
	if e == nil || e.Status != 400 {
		return false
	}
	return strings.Contains(e.Message, "reasoning_effort") &&
		strings.Contains(e.Message, "reasoning.effort") &&
		strings.Contains(e.Message, "conflicting")
}

// adaptThinkingUnified applies the thinking-knob adaptation to a decoded
// unified request before cross-format encoding — the typed-struct
// counterpart of adaptThinkingBody, which only runs on the same-format
// raw-body branch (#50 residual from #17). A no-thinking match WINS over
// always-thinking when both globs apply (kilo-auto/* rotation): its knobs
// are stripped outright, because the upstream rejects any value at all.
// Same discipline otherwise, adapted to the unified model:
//   - u.ReasoningEffort is coerced only when the client set it (empty stays
//     empty — knobs are never invented): none|minimal|medium → low,
//     xhigh → max, unrecognized → high.
//   - u.Thinking (budget-based, decodes only from an explicit Anthropic
//     thinking:enabled) is dropped for always-thinking upstreams: the GLM
//     wire has no budget representation — encoders either drop it silently
//     (OpenAI/Gemini out) or derive an effort via budgetToEffort that can
//     violate the low|high|max enum (Responses out). Dropping lets the
//     upstream default (thinking on) apply, mirroring the same-format
//     disable-drop. Disable forms never decode into the unified struct, so
//     there is nothing else to strip.
func adaptThinkingUnified(u *types.ChatRequest, upstreamModel string, def *provider.Def) {
	if def == nil {
		return
	}
	if def.NoThinkingModel(upstreamModel) {
		u.ReasoningEffort = ""
		u.Thinking = nil
		return
	}
	if !def.AlwaysThinkingModel(upstreamModel) {
		return
	}
	if u.ReasoningEffort != "" {
		u.ReasoningEffort = coerceEffort(u.ReasoningEffort)
	}
	u.Thinking = nil
}

// adaptThinkingBody rewrites thinking knobs on a raw same-format body when
// the routed model belongs to a no-thinking or always-thinking provider
// (def != nil and the model matches NoThinking/AlwaysThinking globs; a
// no-thinking match wins). No-thinking: reasoning_effort, thinking and
// enable_thinking are deleted outright — the upstream has no thinking mode
// and rejects ANY reasoning knob (live kilocode 2026-09-11: kilo's gateway
// duplicates reasoning_effort into reasoning.effort and 400s "conflicting
// values" for the rotating kilo-auto/free alias). The top-level reasoning
// OBJECT is kilo's accepted dialect and stays. Always-thinking OpenAI
// dialect: reasoning_effort none|minimal|medium → low (GLM accepts only
// low|high|max); thinking{type:disabled} and enable_thinking:false are
// dropped so the upstream default (thinking on) applies. Knobs are never
// added. Returns body unchanged when not applicable.
func adaptThinkingBody(body []byte, model string, def *provider.Def) []byte {
	if def == nil {
		return body
	}
	noThink := def.NoThinkingModel(model)
	if !noThink && !def.AlwaysThinkingModel(model) {
		return body
	}
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return body // not an object; forward verbatim
	}
	changed := false
	if noThink {
		for _, key := range []string{"reasoning_effort", "thinking", "enable_thinking"} {
			if _, ok := root[key]; ok {
				delete(root, key)
				changed = true
			}
		}
	} else {
		if v, ok := root["reasoning_effort"]; ok {
			if s, ok := v.(string); ok {
				if c := coerceEffort(s); c != s {
					root["reasoning_effort"] = c
					changed = true
				}
			}
		}
		for _, key := range []string{"thinking", "enable_thinking"} {
			if v, ok := root[key]; ok {
				switch tv := v.(type) {
				case map[string]any:
					if t, _ := tv["type"].(string); t == "disabled" {
						delete(root, key)
						changed = true
					}
				case bool:
					if !tv {
						delete(root, key)
						changed = true
					}
				}
			}
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

func encodeFor(f translat.Format, u *types.ChatRequest) ([]byte, error) {
	switch f {
	case translat.FmtOpenAI:
		return translat.EncodeOpenAIRequest(u)
	case translat.FmtAnthropic:
		return translat.EncodeAnthropicRequest(u)
	case translat.FmtGemini:
		return translat.EncodeGeminiRequest(u)
	case translat.FmtResponses:
		return translat.EncodeResponsesRequest(u)
	case translat.FmtOpenAIResponses:
		return translat.EncodeGrokCliRequest(u)
	case translat.FmtCommandCode:
		return translat.EncodeCommandCodeRequest(u)
	default:
		return nil, fmt.Errorf("unknown format %s", f)
	}
}

// peekModel extracts the model string without full parsing.
func peekModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		return probe.Model
	}
	return ""
}

func peekStream(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Stream
}

func writeErr(w http.ResponseWriter, f translat.Format, e *types.APIError) {
	w.Header().Set("Content-Type", "application/json")
	if e.RetryAfter != "" {
		w.Header().Set("Retry-After", e.RetryAfter)
	}
	w.WriteHeader(e.Status)
	_, _ = w.Write(translat.EncodeError(f, e))
}

func errAPI(status int, typ, msg string) *types.APIError {
	return &types.APIError{Status: status, Type: typ, Message: msg}
}
